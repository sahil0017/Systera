package engineeringpulse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/systera/control-plane/internal/platform/postgres"
)

// Claim is a source due for fetching, with the fetch row that records the attempt.
type Claim struct {
	Source  Source
	FetchID string
}

// ClaimDueSources leases up to limit approved sources that are due, within their per-hour
// rate limit and minimum interval. The attempt is recorded (and next_fetch_at pushed out) in
// the same transaction, so concurrent workers never fetch a source twice and every attempt
// counts against the rate limit.
func (s *Store) ClaimDueSources(ctx context.Context, limit int) ([]Claim, error) {
	if limit <= 0 {
		limit = 10
	}
	now := s.now().UTC()
	var out []Claim
	err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+sourceCols+` FROM trend_sources src
			WHERE src.status = 'APPROVED' AND src.next_fetch_at <= $1::timestamptz
			  AND (src.last_fetched_at IS NULL OR src.last_fetched_at + make_interval(secs => src.min_interval_seconds) <= $1::timestamptz)
			  AND (SELECT count(*) FROM trend_source_fetches f
			       WHERE f.source_id = src.id AND f.started_at > $1::timestamptz - interval '1 hour') < src.rate_limit_per_hour
			ORDER BY src.next_fetch_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED`, now, limit)
		if err != nil {
			return err
		}
		var srcs []Source
		for rows.Next() {
			src, err := scanSource(rows)
			if err != nil {
				rows.Close()
				return err
			}
			srcs = append(srcs, src)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, src := range srcs {
			var fetchID string
			if err := tx.QueryRow(ctx, `INSERT INTO trend_source_fetches (source_id, requested_url, started_at) VALUES ($1, $2, $3) RETURNING id::text`,
				src.ID, src.FeedURL, now).Scan(&fetchID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE trend_sources SET last_fetched_at = $2::timestamptz, next_fetch_at = $2::timestamptz + make_interval(secs => min_interval_seconds), updated_at = NOW()
				WHERE id = $1`, src.ID, now); err != nil {
				return err
			}
			out = append(out, Claim{Source: src, FetchID: fetchID})
		}
		return nil
	})
	return out, err
}

// FetchOutcome summarizes one recorded fetch.
type FetchOutcome struct {
	SourceKey   string         `json:"source_key"`
	FetchID     string         `json:"fetch_id"`
	Status      int            `json:"http_status"`
	NotModified bool           `json:"not_modified"`
	ItemsSeen   int            `json:"items_seen"`
	ItemsNew    int            `json:"items_new"`
	Duplicates  map[string]int `json:"duplicates,omitempty"`
	Error       string         `json:"error,omitempty"`
}

// maxConsecutiveFailures suspends a source that keeps failing (it needs a human look).
const maxConsecutiveFailures = 10

// RecordFetch stores the result of a fetch: new items (content trimmed to what the source's
// terms allow, deduplicated against everything seen so far), the retrieval metadata, and the
// source's conditional-request and back-off state.
func (s *Store) RecordFetch(ctx context.Context, c Claim, res FetchResult, fetchErr error) (FetchOutcome, error) {
	now := s.now().UTC()
	out := FetchOutcome{SourceKey: c.Source.Key, FetchID: c.FetchID, Status: res.Status, NotModified: res.NotModified, ItemsSeen: len(res.Items)}
	err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		if fetchErr != nil {
			out.Error = fetchErr.Error()
			backoff := time.Duration(c.Source.MinIntervalSeconds) * time.Second * time.Duration(math.Pow(2, float64(min(c.Source.ConsecutiveFailures+1, 10))))
			if backoff > 24*time.Hour {
				backoff = 24 * time.Hour
			}
			if res.RetryAfter > backoff {
				backoff = res.RetryAfter
			}
			if _, err := tx.Exec(ctx, `
				UPDATE trend_source_fetches SET finished_at = $2, http_status = NULLIF($3, 0), content_type = $4, bytes = $5, error = $6
				WHERE id = $1`, c.FetchID, now, res.Status, res.ContentType, res.Bytes, truncateRunes(out.Error, 2000)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `
				UPDATE trend_sources SET consecutive_failures = consecutive_failures + 1, last_error = $2,
				       next_fetch_at = $3,
				       status = CASE WHEN consecutive_failures + 1 >= $4 THEN 'SUSPENDED' ELSE status END, updated_at = NOW()
				WHERE id = $1`, c.Source.ID, truncateRunes(out.Error, 2000), now.Add(backoff), maxConsecutiveFailures)
			return err
		}

		// Serialize dedup decisions across workers.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('engineering_pulse_dedup'))`); err != nil {
			return err
		}
		out.Duplicates = map[string]int{}
		for _, raw := range res.Items {
			created, kind, err := s.insertItemTx(ctx, tx, c, res, raw, now)
			if err != nil {
				return fmt.Errorf("item %q: %w", raw.ExternalID, err)
			}
			if created {
				out.ItemsNew++
				if kind != "" {
					out.Duplicates[kind]++
				}
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE trend_source_fetches SET finished_at = $2, http_status = $3, not_modified = $4, content_type = $5,
			       bytes = $6, items_seen = $7, items_new = $8
			WHERE id = $1`, c.FetchID, now, res.Status, res.NotModified, res.ContentType, res.Bytes, out.ItemsSeen, out.ItemsNew); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE trend_sources SET etag = $2, last_modified = $3, consecutive_failures = 0, last_error = '', updated_at = NOW()
			WHERE id = $1`, c.Source.ID, res.ETag, res.LastModified)
		return err
	})
	return out, err
}

func (s *Store) insertItemTx(ctx context.Context, tx pgx.Tx, c Claim, res FetchResult, raw RawItem, now time.Time) (bool, string, error) {
	raw = ApplyContentPolicy(raw, c.Source.AllowedContent, c.Source.MaxSummaryChars)
	body := raw.Summary + " " + raw.Content
	hash := ContentHash(raw.Title, body)

	// Same entry already stored? Unchanged -> skip; edited -> store a new version of it.
	extID := raw.ExternalID
	var existingID, existingHash, existingSeries string
	err := tx.QueryRow(ctx, `SELECT id::text, content_hash, series_key FROM trend_items WHERE source_id = $1 AND external_id = $2`,
		c.Source.ID, extID).Scan(&existingID, &existingHash, &existingSeries)
	switch {
	case err == nil && existingHash == hash:
		return false, "", nil
	case err == nil:
		var dup int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM trend_items WHERE source_id = $1 AND content_hash = $2`, c.Source.ID, hash).Scan(&dup); err != nil {
			return false, "", err
		}
		if dup > 0 {
			return false, "", nil
		}
		if raw.SeriesKey == "" {
			raw.SeriesKey = existingSeries
		}
		if raw.SeriesKey == "" {
			raw.SeriesKey = "edit:" + c.Source.Key + ":" + extID
			if _, err := tx.Exec(ctx, `UPDATE trend_items SET series_key = $2 WHERE id = $1`, existingID, raw.SeriesKey); err != nil {
				return false, "", err
			}
		}
		extID = extID + "@" + hash[:12]
	case !errors.Is(err, pgx.ErrNoRows):
		return false, "", err
	}

	curl := CanonicalURL(raw.URL)
	normTitle := NormTitle(raw.Title)
	sim, ntok := SimHash(raw.Title + " " + body)
	n := DedupCandidate{SourceID: c.Source.ID, IsPrimarySource: c.Source.IsPrimarySource, CanonicalURL: curl, NormTitle: normTitle,
		ContentHash: hash, SeriesKey: raw.SeriesKey, VersionLabel: raw.VersionLabel, SimHash: sim, Tokens: ntok, PublishedUnix: pubUnix(raw.PublishedAt)}
	cands, err := dedupCandidatesTx(ctx, tx, n, now)
	if err != nil {
		return false, "", err
	}
	dec := DecideDuplicate(n, cands)

	meta := map[string]any{
		"fetch_id": c.FetchID, "requested_url": res.RequestedURL, "http_status": res.Status, "content_type": res.ContentType,
		"etag": res.ETag, "retrieved_at": now.Format(time.RFC3339), "connector": c.Source.Connector,
		"allowed_content": c.Source.AllowedContent, "licence": c.Source.Licence, "terms_url": c.Source.TermsURL,
		"attribution_required": c.Source.AttributionRequired,
	}
	if len(raw.Hints) > 0 {
		meta["hints"] = raw.Hints
	}
	metaJSON, _ := json.Marshal(meta)
	authors := raw.Authors
	if authors == nil {
		authors = []string{}
	}

	var canonID *string
	var kind *string
	var score *float64
	if dec.Kind != "" && !dec.Promote {
		canonID, kind, score = &dec.CanonicalID, &dec.Kind, &dec.Score
	}
	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO trend_items (source_id, fetch_id, external_id, url, canonical_url, title, norm_title, summary, content,
			authors, published_at, retrieved_at, retrieval_metadata, series_key, version_label, engagement, content_hash,
			simhash, sim_tokens, canonical_item_id, duplicate_kind, duplicate_score, classified_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,
		        CASE WHEN $20::uuid IS NULL THEN NULL ELSE $12::timestamptz END)
		RETURNING id::text`,
		c.Source.ID, c.FetchID, extID, raw.URL, curl, raw.Title, normTitle, raw.Summary, raw.Content,
		authors, raw.PublishedAt, now, metaJSON, raw.SeriesKey, raw.VersionLabel, raw.Engagement, hash,
		int64(sim), ntok, canonID, kind, score).Scan(&id); err != nil {
		return false, "", err
	}
	if dec.Kind != "" && dec.Promote {
		// The new item becomes canonical (newer version / primary source); the old canonical
		// and its duplicates now point at it.
		if _, err := tx.Exec(ctx, `UPDATE trend_items SET canonical_item_id = $1 WHERE canonical_item_id = $2`, id, dec.CanonicalID); err != nil {
			return false, "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE trend_items SET canonical_item_id = $1, duplicate_kind = $3, duplicate_score = $4 WHERE id = $2`,
			id, dec.CanonicalID, dec.Kind, dec.Score); err != nil {
			return false, "", err
		}
	}
	return true, dec.Kind, nil
}

// nearDupWindow bounds the SimHash comparison set.
const nearDupWindow = 60 * 24 * time.Hour

func dedupCandidatesTx(ctx context.Context, tx pgx.Tx, n DedupCandidate, now time.Time) ([]DedupCandidate, error) {
	rows, err := tx.Query(ctx, `
		SELECT i.id::text, COALESCE(i.canonical_item_id::text, ''), i.source_id::text, s.is_primary_source, i.canonical_url,
		       i.norm_title, i.content_hash, i.series_key, i.version_label, i.simhash, i.sim_tokens,
		       COALESCE(extract(epoch FROM i.published_at)::bigint, 0)
		FROM trend_items i JOIN trend_sources s ON s.id = i.source_id
		WHERE i.content_hash = $1 OR i.canonical_url = $2 OR ($3 <> '' AND i.series_key = $3) OR i.norm_title = $4
		   OR (i.canonical_item_id IS NULL AND i.sim_tokens >= $6 AND i.retrieved_at > $5)
		ORDER BY i.created_at
		LIMIT 5000`, n.ContentHash, n.CanonicalURL, n.SeriesKey, n.NormTitle, now.Add(-nearDupWindow), minNearDupTokens)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DedupCandidate
	for rows.Next() {
		var c DedupCandidate
		var sim int64
		if err := rows.Scan(&c.ID, &c.CanonicalID, &c.SourceID, &c.IsPrimarySource, &c.CanonicalURL, &c.NormTitle, &c.ContentHash,
			&c.SeriesKey, &c.VersionLabel, &sim, &c.Tokens, &c.PublishedUnix); err != nil {
			return nil, err
		}
		c.SimHash = uint64(sim)
		out = append(out, c)
	}
	return out, rows.Err()
}

// LoadTopics returns the taxonomy.
func (s *Store) LoadTopics(ctx context.Context, tenantID string) ([]Topic, error) {
	var out []Topic
	err := s.read(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = loadTopicsTx(ctx, tx)
		return err
	})
	return out, err
}

func loadTopicsTx(ctx context.Context, tx pgx.Tx) ([]Topic, error) {
	rows, err := tx.Query(ctx, `
		SELECT t.id::text, t.topic_key, t.name, t.facet, COALESCE(p.topic_key, ''), t.keywords, t.description, t.maturity, t.first_seen_at
		FROM trend_topics t LEFT JOIN trend_topics p ON p.id = t.parent_id ORDER BY t.facet, t.topic_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Topic{}
	for rows.Next() {
		var t Topic
		if err := rows.Scan(&t.ID, &t.Key, &t.Name, &t.Facet, &t.ParentKey, &t.Keywords, &t.Description, &t.Maturity, &t.FirstSeenAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ClassifyPending classifies canonical items that have not been classified yet (2.4).
func (s *Store) ClassifyPending(ctx context.Context, cl Classifier, limit int) (int, error) {
	if limit <= 0 {
		limit = 500
	}
	n := 0
	err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		if cl == nil {
			topics, err := loadTopicsTx(ctx, tx)
			if err != nil {
				return err
			}
			cl = NewRuleClassifier(topics)
		}
		rows, err := tx.Query(ctx, `
			SELECT i.id::text, i.title, i.summary, i.content, s.source_type, i.version_label, i.retrieval_metadata->'hints', i.published_at
			FROM trend_items i JOIN trend_sources s ON s.id = i.source_id
			WHERE i.classified_at IS NULL AND i.canonical_item_id IS NULL
			ORDER BY i.created_at LIMIT $1
			FOR UPDATE OF i SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		type pending struct {
			id  string
			in  ClassifyInput
			pub *time.Time
		}
		var ps []pending
		for rows.Next() {
			var p pending
			var hints []byte
			if err := rows.Scan(&p.id, &p.in.Title, &p.in.Summary, &p.in.Content, &p.in.SourceType, &p.in.VersionLabel, &hints, &p.pub); err != nil {
				rows.Close()
				return err
			}
			_ = json.Unmarshal(hints, &p.in.Hints)
			ps = append(ps, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range ps {
			res := cl.Classify(p.in)
			for _, m := range res.Topics {
				if _, err := tx.Exec(ctx, `
					INSERT INTO trend_item_topics (item_id, topic_id, confidence, matched_terms, method)
					SELECT $1, id, $3, $4, 'RULES' FROM trend_topics WHERE topic_key = $2
					ON CONFLICT (item_id, topic_id) DO UPDATE SET confidence = EXCLUDED.confidence, matched_terms = EXCLUDED.matched_terms`,
					p.id, m.TopicKey, m.Confidence, m.MatchedTerms); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `
					UPDATE trend_topics SET first_seen_at = LEAST(COALESCE(first_seen_at, $2), $2), updated_at = NOW()
					WHERE topic_key = $1`, m.TopicKey, coalesceTime(p.pub, s.now().UTC())); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE trend_items SET maturity = $2, significance = $3, classified_at = $4 WHERE id = $1`,
				p.id, res.Maturity, res.Significance, s.now().UTC()); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

func coalesceTime(t *time.Time, d time.Time) time.Time {
	if t == nil {
		return d
	}
	return *t
}
