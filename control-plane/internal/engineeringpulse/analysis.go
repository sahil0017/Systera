package engineeringpulse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/systera/control-plane/internal/knowledge"
	"github.com/systera/control-plane/internal/platform/postgres"
)

// RefreshObservations rebuilds daily per-topic observations from `since` onwards. Each
// canonical item counts once as an item; it and its mirrors/reposts/versions count as mentions.
func (s *Store) RefreshObservations(ctx context.Context, since time.Time) error {
	return postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		day := since.UTC().Truncate(24 * time.Hour)
		if _, err := tx.Exec(ctx, `DELETE FROM trend_observations WHERE day >= $1::date`, day); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			WITH m AS (
				SELECT it.topic_id,
				       (COALESCE(c.published_at, c.retrieved_at) AT TIME ZONE 'UTC')::date AS day,
				       x.source_id, (x.id = c.id) AS is_canon, (x.id = c.id AND s.is_primary_source) AS prim, x.engagement
				FROM trend_items c
				JOIN trend_item_topics it ON it.item_id = c.id
				JOIN trend_items x ON x.id = c.id OR x.canonical_item_id = c.id
				JOIN trend_sources s ON s.id = x.source_id
				WHERE c.canonical_item_id IS NULL AND COALESCE(c.published_at, c.retrieved_at) >= $1
			)
			INSERT INTO trend_observations (topic_id, day, item_count, mention_count, source_count, primary_count, engagement, updated_at)
			SELECT topic_id, day, count(*) FILTER (WHERE is_canon), count(*), count(DISTINCT source_id),
			       count(*) FILTER (WHERE prim), COALESCE(sum(engagement), 0), NOW()
			FROM m GROUP BY topic_id, day`, day)
		return err
	})
}

// ScoreConfig tunes trend scoring.
type ScoreConfig struct {
	WindowDays      int // default 28
	BaselineWindows int // default 6
	TopItems        int // significance averages the top N items, default 5
}

func (c ScoreConfig) withDefaults() ScoreConfig {
	if c.WindowDays <= 0 {
		c.WindowDays = 28
	}
	if c.BaselineWindows <= 0 {
		c.BaselineWindows = 6
	}
	if c.TopItems <= 0 {
		c.TopItems = 5
	}
	return c
}

// ObservationHorizon is how far back observations must reach for scoring.
func (c ScoreConfig) ObservationHorizon(now time.Time) time.Time {
	c = c.withDefaults()
	return now.Add(-time.Duration(c.WindowDays*(2+c.BaselineWindows)) * 24 * time.Hour)
}

// ComputeScores scores every topic with activity in the current window (2.5). The total is
// the database's generated column; it must equal the Go formula or the write is rejected.
func (s *Store) ComputeScores(ctx context.Context, cfg ScoreConfig) ([]TrendScore, error) {
	cfg = cfg.withDefaults()
	now := s.now().UTC()
	w := time.Duration(cfg.WindowDays) * 24 * time.Hour
	curStart, prevStart := now.Add(-w), now.Add(-2*w)
	baseStart := now.Add(-time.Duration(2+cfg.BaselineWindows) * w)
	var out []TrendScore
	err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT t.id::text, t.topic_key, t.first_seen_at,
			       COALESCE(sum(o.mention_count) FILTER (WHERE o.day > $1::date), 0),
			       COALESCE(sum(o.mention_count) FILTER (WHERE o.day > $2::date AND o.day <= $1::date), 0),
			       COALESCE(sum(o.mention_count) FILTER (WHERE o.day > $3::date AND o.day <= $2::date), 0),
			       COALESCE(sum(o.engagement) FILTER (WHERE o.day > $1::date), 0)
			FROM trend_topics t JOIN trend_observations o ON o.topic_id = t.id
			GROUP BY t.id, t.topic_key, t.first_seen_at
			HAVING COALESCE(sum(o.mention_count) FILTER (WHERE o.day > $1::date), 0) > 0`,
			curStart, prevStart, baseStart)
		if err != nil {
			return err
		}
		type agg struct {
			id, key string
			first   *time.Time
			in      ScoreInputs
		}
		var aggs []agg
		for rows.Next() {
			var a agg
			if err := rows.Scan(&a.id, &a.key, &a.first, &a.in.Mentions, &a.in.PrevMentions, &a.in.BaselineMentions, &a.in.Engagement); err != nil {
				rows.Close()
				return err
			}
			aggs = append(aggs, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, a := range aggs {
			in := a.in
			in.WindowDays, in.BaselineWindows = cfg.WindowDays, cfg.BaselineWindows
			if a.first != nil {
				in.DaysSinceFirstSeen = now.Sub(*a.first).Hours() / 24
			}
			// Item-level signals for the window: significance of top canonical items, source
			// reliability across the family, primary share and source diversity.
			if err := tx.QueryRow(ctx, `
				WITH c AS (
					SELECT i.id, i.significance::float8 AS sig, s.is_primary_source
					FROM trend_items i JOIN trend_item_topics it ON it.item_id = i.id AND it.topic_id = $1
					JOIN trend_sources s ON s.id = i.source_id
					WHERE i.canonical_item_id IS NULL AND COALESCE(i.published_at, i.retrieved_at) > $2
				), fam AS (
					SELECT x.source_id, s.reliability::float8 AS rel
					FROM c JOIN trend_items x ON x.id = c.id OR x.canonical_item_id = c.id
					JOIN trend_sources s ON s.id = x.source_id
				)
				SELECT COALESCE((SELECT avg(sig) FROM (SELECT sig FROM c ORDER BY sig DESC LIMIT $3) top), 0),
				       COALESCE((SELECT avg(rel) FROM fam), 0),
				       COALESCE((SELECT avg(CASE WHEN is_primary_source THEN 1.0 ELSE 0.0 END) FROM c), 0),
				       (SELECT count(DISTINCT source_id) FROM fam)`,
				a.id, curStart, cfg.TopItems).Scan(&in.TopSignificance, &in.AvgReliability, &in.PrimaryShare, &in.DistinctSources); err != nil {
				return err
			}
			comp := ComputeComponents(in)
			inputs, _ := json.Marshal(in)
			ts := TrendScore{TopicKey: a.key, ComputedAt: now, WindowDays: cfg.WindowDays, Components: comp}
			_ = json.Unmarshal(inputs, &ts.Inputs)
			if err := tx.QueryRow(ctx, `
				INSERT INTO trend_scores (topic_id, computed_at, window_days, popularity, growth, significance, reliability, novelty, inputs)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id::text, score::float8`,
				a.id, now, cfg.WindowDays, comp.Popularity, comp.Growth, comp.Significance, comp.Reliability, comp.Novelty, inputs,
			).Scan(&ts.ID, &ts.Score); err != nil {
				return err
			}
			if math.Abs(ts.Score-comp.Total()) > 0.011 {
				return fmt.Errorf("trend score mismatch for %s: database %.2f, formula %.2f", a.key, ts.Score, comp.Total())
			}
			out = append(out, ts)
		}
		return nil
	})
	return out, err
}

// ResearchOptions selects which topics get researched.
type ResearchOptions struct {
	MinScore float64 // only topics whose latest score is at least this (default 30)
	Research ResearchConfig
	Gate     GateConfig
}

// Research builds and validates reports for scored topics whose evidence changed (2.6, 2.7).
// Every report is stored, including rejected ones, with the gate's check results.
func (s *Store) Research(ctx context.Context, opt ResearchOptions) ([]Report, error) {
	if opt.MinScore <= 0 {
		opt.MinScore = 30
	}
	rcfg := opt.Research.withDefaults()
	now := s.now().UTC()
	var out []Report
	err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		topics, err := loadTopicsTx(ctx, tx)
		if err != nil {
			return err
		}
		byID := map[string]Topic{}
		for _, t := range topics {
			byID[t.ID] = t
		}
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT ON (topic_id) id::text, topic_id::text, computed_at, window_days, popularity::float8, growth::float8,
			       significance::float8, reliability::float8, novelty::float8, score::float8
			FROM trend_scores ORDER BY topic_id, computed_at DESC`)
		if err != nil {
			return err
		}
		var scores []TrendScore
		var scoreTopic []string
		for rows.Next() {
			var ts TrendScore
			var tid string
			c := &ts.Components
			if err := rows.Scan(&ts.ID, &tid, &ts.ComputedAt, &ts.WindowDays, &c.Popularity, &c.Growth, &c.Significance, &c.Reliability, &c.Novelty, &ts.Score); err != nil {
				rows.Close()
				return err
			}
			if ts.Score >= opt.MinScore {
				ts.TopicKey = byID[tid].Key
				scores, scoreTopic = append(scores, ts), append(scoreTopic, tid)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i, ts := range scores {
			topic := byID[scoreTopic[i]]
			cands, err := candidateEvidenceTx(ctx, tx, topic.ID, now.Add(-rcfg.Lookback))
			if err != nil {
				return err
			}
			ev := SelectEvidence(cands, rcfg)
			if len(ev) == 0 {
				continue
			}
			hash := EvidenceHash(ev)
			var lastStatus string
			err = tx.QueryRow(ctx, `SELECT status FROM research_reports WHERE topic_id = $1 AND evidence_hash = $2 ORDER BY created_at DESC LIMIT 1`,
				topic.ID, hash).Scan(&lastStatus)
			if err == nil && lastStatus != ReportStale {
				continue // evidence unchanged since the last report
			}
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			ts := ts
			r := BuildReport(topic, &ts, ev, now, rcfg)
			r.Validation = Validate(r, now, opt.Gate)
			r.Status = ReportRejected
			if r.Validation.Passed {
				r.Status = ReportValidated
			}
			va := r.Validation.ValidatedAt
			r.ValidatedAt = &va
			if err := insertReportTx(ctx, tx, &r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

func insertReportTx(ctx context.Context, tx pgx.Tx, r *Report) error {
	findings, _ := json.Marshal(r.Findings)
	validation, _ := json.Marshal(r.Validation)
	var tsID *string
	if r.TrendScoreID != "" {
		tsID = &r.TrendScoreID
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO research_reports (topic_id, trend_score_id, title, summary, findings, limitations, caveats, confidence,
			confidence_band, maturity, freshness_at, stale_after, evidence_hash, status, validation, validated_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$17) RETURNING id::text`,
		r.TopicID, tsID, r.Title, r.Summary, findings, nonNil(r.Limitations), nonNil(r.Caveats), r.Confidence, r.ConfidenceBand,
		r.Maturity, r.FreshnessAt, r.StaleAfter, r.EvidenceHash, r.Status, validation, r.ValidatedAt, r.Validation.ValidatedAt,
	).Scan(&r.ID); err != nil {
		return err
	}
	r.CreatedAt = r.Validation.ValidatedAt
	for _, e := range r.Evidence {
		if _, err := tx.Exec(ctx, `INSERT INTO research_report_evidence (report_id, item_id, role) VALUES ($1, $2, $3)`, r.ID, e.ItemID, e.Role); err != nil {
			return err
		}
	}
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

const evidenceCols = `i.id::text, i.title, i.url, i.summary, i.published_at, i.retrieved_at, i.fetch_id::text,
	(i.canonical_item_id IS NULL), i.maturity, i.significance::float8, s.source_key, s.name, s.source_type, s.status,
	s.is_primary_source, s.reliability::float8, s.terms_url, s.licence, s.allowed_content, i.retrieval_metadata`

func scanEvidence(row pgx.Row, role *string) (EvidenceItem, error) {
	var e EvidenceItem
	var meta []byte
	dest := []any{&e.ItemID, &e.Title, &e.URL, &e.Summary, &e.PublishedAt, &e.RetrievedAt, &e.FetchID, &e.IsCanonical, &e.Maturity,
		&e.Significance, &e.SourceKey, &e.SourceName, &e.SourceType, &e.SourceStatus, &e.IsPrimarySource, &e.Reliability,
		&e.TermsURL, &e.Licence, &e.AllowedContent, &meta}
	if role != nil {
		dest = append(dest, role)
	}
	if err := row.Scan(dest...); err != nil {
		return EvidenceItem{}, err
	}
	_ = json.Unmarshal(meta, &e.Retrieval)
	if role != nil {
		e.Role = *role
	}
	return e, nil
}

// candidateEvidenceTx returns canonical items classified under the topic (directly, at
// confidence >= 0.5) from approved sources within the lookback.
func candidateEvidenceTx(ctx context.Context, tx pgx.Tx, topicID string, since time.Time) ([]EvidenceItem, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+evidenceCols+`
		FROM trend_items i
		JOIN trend_item_topics it ON it.item_id = i.id AND it.topic_id = $1 AND it.confidence >= 0.5
		JOIN trend_sources s ON s.id = i.source_id
		WHERE i.canonical_item_id IS NULL AND s.status = 'APPROVED' AND COALESCE(i.published_at, i.retrieved_at) >= $2
		ORDER BY i.published_at DESC NULLS LAST
		LIMIT 200`, topicID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EvidenceItem
	for rows.Next() {
		e, err := scanEvidence(rows, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func reportEvidenceTx(ctx context.Context, tx pgx.Tx, reportID string) ([]EvidenceItem, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+evidenceCols+`, e.role
		FROM research_report_evidence e JOIN trend_items i ON i.id = e.item_id JOIN trend_sources s ON s.id = i.source_id
		WHERE e.report_id = $1
		ORDER BY (e.role = 'PRIMARY') DESC, s.reliability DESC, i.published_at DESC NULLS LAST`, reportID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EvidenceItem
	for rows.Next() {
		var role string
		e, err := scanEvidence(rows, &role)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ErrNotPublishable: the report did not pass the gate (or was already handled).
var ErrNotPublishable = errors.New("report is not publishable")

// Publish turns a VALIDATED report into PUBLIC knowledge (2.7, 2.8): a new version of the
// topic's knowledge document (superseding the previous one), a knowledge_updates row (the
// database re-checks the gate), and a notification for every subscribed agent domain.
func (s *Store) Publish(ctx context.Context, reportID string) (KnowledgeUpdate, error) {
	if s.knowledge == nil {
		return KnowledgeUpdate{}, errors.New("engineering pulse: no knowledge store configured")
	}
	r, err := s.GetReport(ctx, "", reportID)
	if err != nil {
		return KnowledgeUpdate{}, err
	}
	if r.Status != ReportValidated || !r.Validation.Passed {
		return KnowledgeUpdate{}, fmt.Errorf("%w: status %s, failed checks %v", ErrNotPublishable, r.Status, r.Validation.Failed())
	}
	topic, err := s.topicByKey(ctx, "", r.TopicKey)
	if err != nil {
		return KnowledgeUpdate{}, err
	}
	in := knowledgeInput(topic, r)
	prepared, err := s.knowledge.Prepare(ctx, in)
	if err != nil {
		return KnowledgeUpdate{}, err
	}
	var ku KnowledgeUpdate
	err = postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM research_reports WHERE id = $1 FOR UPDATE`, reportID).Scan(&status); err != nil {
			return err
		}
		if status != ReportValidated {
			return fmt.Errorf("%w: status %s", ErrNotPublishable, status)
		}
		doc, created, err := knowledge.IngestPreparedTx(ctx, tx, "", prepared)
		if err != nil {
			return err
		}
		if !created {
			_, err := tx.Exec(ctx, `UPDATE research_reports SET status = 'SUPERSEDED', updated_at = NOW(),
				validation = validation || jsonb_build_object('note', 'identical knowledge already published as ' || $2::text)
				WHERE id = $1`, reportID, doc.ID)
			if err != nil {
				return err
			}
			return fmt.Errorf("%w: identical knowledge already published (document %s)", ErrNotPublishable, doc.ID)
		}
		now := s.now().UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_updates SET status = 'SUPERSEDED', updated_at = NOW() WHERE topic_id = $1 AND status IN ('PUBLISHED', 'STALE')`, r.TopicID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE research_reports SET status = 'SUPERSEDED', updated_at = NOW() WHERE topic_id = $1 AND status IN ('PUBLISHED', 'STALE') AND id <> $2`,
			r.TopicID, reportID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO knowledge_updates (report_id, topic_id, document_id, published_at) VALUES ($1, $2, $3, $4)
			RETURNING id::text, published_at`, reportID, r.TopicID, doc.ID, now).Scan(&ku.ID, &ku.PublishedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE research_reports SET status = 'PUBLISHED', updated_at = NOW() WHERE id = $1`, reportID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE trend_topics SET maturity = $2, updated_at = NOW() WHERE id = $1`, r.TopicID, r.Maturity); err != nil {
			return err
		}
		// Distribution: the report's topic and all its ancestors match subscriptions.
		rows, err := tx.Query(ctx, `
			WITH RECURSIVE anc AS (
				SELECT id, parent_id FROM trend_topics WHERE id = $1
				UNION ALL
				SELECT t.id, t.parent_id FROM trend_topics t JOIN anc ON t.id = anc.parent_id
			)
			INSERT INTO agent_knowledge_notifications (subscription_id, agent_type, knowledge_update_id, created_at)
			SELECT sub.id, sub.agent_type, $2, $4
			FROM agent_knowledge_subscriptions sub JOIN anc ON anc.id = sub.topic_id
			WHERE sub.active AND sub.min_confidence <= $3
			ON CONFLICT (subscription_id, knowledge_update_id) DO NOTHING
			RETURNING agent_type`, r.TopicID, ku.ID, r.Confidence, now)
		if err != nil {
			return err
		}
		agents := []string{}
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				rows.Close()
				return err
			}
			agents = append(agents, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		ku.ReportID, ku.TopicKey, ku.DocumentID, ku.Status = reportID, r.TopicKey, doc.ID, "PUBLISHED"
		ku.Title, ku.Confidence, ku.Band, ku.Maturity, ku.StaleAfter = r.Title, r.Confidence, r.ConfidenceBand, r.Maturity, r.StaleAfter
		ku.Notified = sortedUnique(agents)
		payload, _ := json.Marshal(map[string]any{"knowledge_update_id": ku.ID, "document_id": doc.ID, "topic_key": r.TopicKey, "agent_types": ku.Notified})
		_, err = tx.Exec(ctx, `SELECT pg_notify('engineering_pulse_knowledge', $1)`, string(payload))
		return err
	})
	return ku, err
}

func knowledgeInput(topic Topic, r Report) knowledge.IngestInput {
	var primary *EvidenceItem
	for i := range r.Evidence {
		if r.Evidence[i].Role == "PRIMARY" {
			primary = &r.Evidence[i]
			break
		}
	}
	srcURL, srcType := "", "PRIMARY_DOC"
	if primary != nil {
		srcURL, srcType = primary.URL, primary.SourceType
	}
	evidence := make([]map[string]any, 0, len(r.Evidence))
	licences := map[string]bool{}
	for _, e := range r.Evidence {
		licences[e.Licence] = true
		evidence = append(evidence, map[string]any{
			"item_id": e.ItemID, "title": e.Title, "url": e.URL, "source_key": e.SourceKey, "source_name": e.SourceName,
			"role": e.Role, "is_primary_source": e.IsPrimarySource, "published_at": e.PublishedAt, "reliability": e.Reliability,
			"licence": e.Licence,
		})
	}
	licence := "MIXED (see sources)"
	if len(licences) == 1 {
		for l := range licences {
			licence = l
		}
	}
	var adoption map[string]any
	for _, f := range r.Findings {
		if f.Kind == FindingAdoption {
			adoption = map[string]any{"statement": f.Claim, "selection_criterion": false,
				"note": "Attention signal only; compare maturity, operational burden, cost, security and fit before selecting technology."}
		}
	}
	return knowledge.IngestInput{
		CanonicalKey: KnowledgeKeyPrefix + topic.Key, Title: r.Title, SourceURL: srcURL, SourceType: srcType,
		IsPrimarySource: primary != nil, Licence: licence, AccessPolicy: "PUBLIC", PublishedAt: r.FreshnessAt,
		Maturity: r.Maturity, ConfidenceBand: r.ConfidenceBand, Collector: "engineering_pulse", TrustScore: math.Max(0.001, r.Confidence),
		RawContent: RenderMarkdown(topic, r),
		Metadata: map[string]any{
			"kind": "engineering_pulse", "report_id": r.ID, "topic_key": topic.Key, "topic_name": topic.Name, "facet": topic.Facet,
			"keywords": topic.Keywords, "evidence": evidence, "limitations": r.Limitations, "caveats": r.Caveats,
			"confidence": r.Confidence, "freshness_at": r.FreshnessAt, "stale_after": r.StaleAfter, "adoption_signal": adoption,
		},
	}
}

// SweepStale marks published knowledge whose report passed stale_after as STALE (its
// knowledge document too, so retrieval drops it unless include_stale is set).
func (s *Store) SweepStale(ctx context.Context) (int, error) {
	n := 0
	err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE knowledge_updates ku SET status = 'STALE', updated_at = NOW()
			FROM research_reports r
			WHERE ku.report_id = r.id AND ku.status = 'PUBLISHED' AND r.stale_after <= $1
			RETURNING ku.document_id::text, r.id::text`, s.now().UTC())
		if err != nil {
			return err
		}
		var docs, reports []string
		for rows.Next() {
			var d, r string
			if err := rows.Scan(&d, &r); err != nil {
				rows.Close()
				return err
			}
			docs, reports = append(docs, d), append(reports, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		n = len(docs)
		if n == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE knowledge_documents SET status = 'STALE', updated_at = NOW() WHERE id = ANY($1::uuid[]) AND status = 'ACTIVE'`, docs); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE research_reports SET status = 'STALE', updated_at = NOW() WHERE id = ANY($1::uuid[])`, reports)
		return err
	})
	return n, err
}
