package engineeringpulse

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/systera/control-plane/internal/platform/ids"
)

// All reads take tenantID: the caller's tenant (Pulse data is platform-wide and readable to
// every tenant, but reads still run as the tenant-bound app role), or "" for system callers.

const reportCols = `r.id::text, r.topic_id::text, t.topic_key, COALESCE(r.trend_score_id::text, ''), r.title, r.summary, r.findings,
	r.limitations, r.caveats, r.confidence::float8, r.confidence_band, r.maturity, r.freshness_at, r.stale_after, r.evidence_hash,
	r.status, r.validation, r.validated_at, r.created_at`

func scanReport(row pgx.Row) (Report, error) {
	var r Report
	var findings, validation []byte
	if err := row.Scan(&r.ID, &r.TopicID, &r.TopicKey, &r.TrendScoreID, &r.Title, &r.Summary, &findings, &r.Limitations, &r.Caveats,
		&r.Confidence, &r.ConfidenceBand, &r.Maturity, &r.FreshnessAt, &r.StaleAfter, &r.EvidenceHash, &r.Status, &validation,
		&r.ValidatedAt, &r.CreatedAt); err != nil {
		return Report{}, err
	}
	_ = json.Unmarshal(findings, &r.Findings)
	_ = json.Unmarshal(validation, &r.Validation)
	return r, nil
}

// GetReport returns a report with its evidence and, if published, its knowledge update.
func (s *Store) GetReport(ctx context.Context, tenantID, id string) (Report, error) {
	if !ids.Valid(id) {
		return Report{}, ErrNotFound
	}
	var r Report
	err := s.read(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		r, err = scanReport(tx.QueryRow(ctx, `SELECT `+reportCols+` FROM research_reports r JOIN trend_topics t ON t.id = r.topic_id WHERE r.id = $1`, id))
		if err != nil {
			return err
		}
		if r.Evidence, err = reportEvidenceTx(ctx, tx, id); err != nil {
			return err
		}
		ku, err := knowledgeUpdatesTx(ctx, tx, `ku.report_id = $1`, id)
		if err != nil {
			return err
		}
		if len(ku) > 0 {
			r.Knowledge = &ku[0]
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Report{}, ErrNotFound
	}
	return r, err
}

// ListReports lists reports (without evidence), newest first.
func (s *Store) ListReports(ctx context.Context, tenantID, topicKey, status string, limit int) ([]Report, error) {
	limit = clampLimit(limit, 50, 200)
	out := []Report{}
	err := s.read(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+reportCols+` FROM research_reports r JOIN trend_topics t ON t.id = r.topic_id
			WHERE ($1 = '' OR t.topic_key = $1) AND ($2 = '' OR r.status = $2)
			ORDER BY r.created_at DESC LIMIT $3`, topicKey, status, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanReport(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Store) topicByKey(ctx context.Context, tenantID, key string) (Topic, error) {
	var t Topic
	err := s.read(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT t.id::text, t.topic_key, t.name, t.facet, COALESCE(p.topic_key, ''), t.keywords, t.description, t.maturity, t.first_seen_at
			FROM trend_topics t LEFT JOIN trend_topics p ON p.id = t.parent_id WHERE t.topic_key = $1`, key).
			Scan(&t.ID, &t.Key, &t.Name, &t.Facet, &t.ParentKey, &t.Keywords, &t.Description, &t.Maturity, &t.FirstSeenAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Topic{}, ErrNotFound
	}
	return t, err
}

// TrendSummary is one row of the trend board.
type TrendSummary struct {
	Topic     Topic            `json:"topic"`
	Score     TrendScore       `json:"score"`
	Report    *ReportBrief     `json:"latest_report,omitempty"`
	Knowledge *KnowledgeUpdate `json:"knowledge,omitempty"`
}

// ReportBrief summarizes the latest report for a topic.
type ReportBrief struct {
	ID             string   `json:"id"`
	Status         string   `json:"status"`
	Confidence     float64  `json:"confidence"`
	ConfidenceBand string   `json:"confidence_band"`
	FailedChecks   []string `json:"failed_checks,omitempty"`
}

// ListTrends returns topics ranked by their latest trend score. The score is an attention
// signal; each row links the validated knowledge (if any) that carries the technical evidence.
func (s *Store) ListTrends(ctx context.Context, tenantID, facet string, minScore float64, limit int) ([]TrendSummary, error) {
	limit = clampLimit(limit, 25, 100)
	out := []TrendSummary{}
	err := s.read(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH latest AS (
				SELECT DISTINCT ON (topic_id) * FROM trend_scores ORDER BY topic_id, computed_at DESC
			)
			SELECT t.id::text, t.topic_key, t.name, t.facet, COALESCE(p.topic_key, ''), t.keywords, t.description, t.maturity, t.first_seen_at,
			       l.id::text, l.computed_at, l.window_days, l.popularity::float8, l.growth::float8, l.significance::float8,
			       l.reliability::float8, l.novelty::float8, l.score::float8, l.inputs,
			       rr.id::text, rr.status, rr.confidence::float8, rr.confidence_band, rr.validation
			FROM latest l JOIN trend_topics t ON t.id = l.topic_id
			LEFT JOIN trend_topics p ON p.id = t.parent_id
			LEFT JOIN LATERAL (SELECT * FROM research_reports r WHERE r.topic_id = t.id ORDER BY r.created_at DESC LIMIT 1) rr ON true
			WHERE ($1 = '' OR t.facet = $1) AND l.score >= $2
			ORDER BY l.score DESC, t.topic_key LIMIT $3`, facet, minScore, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var ts TrendSummary
			t, sc := &ts.Topic, &ts.Score
			c := &sc.Components
			var inputs, validation []byte
			var rid, rstatus, rband *string
			var rconf *float64
			if err := rows.Scan(&t.ID, &t.Key, &t.Name, &t.Facet, &t.ParentKey, &t.Keywords, &t.Description, &t.Maturity, &t.FirstSeenAt,
				&sc.ID, &sc.ComputedAt, &sc.WindowDays, &c.Popularity, &c.Growth, &c.Significance, &c.Reliability, &c.Novelty, &sc.Score, &inputs,
				&rid, &rstatus, &rconf, &rband, &validation); err != nil {
				rows.Close()
				return err
			}
			sc.TopicKey = t.Key
			_ = json.Unmarshal(inputs, &sc.Inputs)
			if rid != nil {
				var g GateResult
				_ = json.Unmarshal(validation, &g)
				ts.Report = &ReportBrief{ID: *rid, Status: *rstatus, Confidence: *rconf, ConfidenceBand: *rband, FailedChecks: g.Failed()}
			}
			out = append(out, ts)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			ku, err := knowledgeUpdatesTx(ctx, tx, `t.topic_key = $1 AND ku.status = 'PUBLISHED'`, out[i].Topic.Key)
			if err != nil {
				return err
			}
			if len(ku) > 0 {
				out[i].Knowledge = &ku[0]
			}
		}
		return nil
	})
	return out, err
}

// TopicDetail is everything Pulse knows about one topic.
type TopicDetail struct {
	Topic        Topic             `json:"topic"`
	Scores       []TrendScore      `json:"scores"`
	Observations []Observation     `json:"observations"`
	Items        []Item            `json:"items"`
	Reports      []Report          `json:"reports"`
	Knowledge    []KnowledgeUpdate `json:"knowledge"`
}

// GetTopic returns a topic's score history, daily observations, canonical items and reports.
func (s *Store) GetTopic(ctx context.Context, tenantID, key string) (TopicDetail, error) {
	t, err := s.topicByKey(ctx, tenantID, key)
	if err != nil {
		return TopicDetail{}, err
	}
	d := TopicDetail{Topic: t, Scores: []TrendScore{}, Observations: []Observation{}}
	err = s.read(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id::text, computed_at, window_days, popularity::float8, growth::float8, significance::float8, reliability::float8,
			       novelty::float8, score::float8, inputs
			FROM trend_scores WHERE topic_id = $1 ORDER BY computed_at DESC LIMIT 30`, t.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var sc TrendScore
			c := &sc.Components
			var inputs []byte
			if err := rows.Scan(&sc.ID, &sc.ComputedAt, &sc.WindowDays, &c.Popularity, &c.Growth, &c.Significance, &c.Reliability, &c.Novelty, &sc.Score, &inputs); err != nil {
				rows.Close()
				return err
			}
			sc.TopicKey = key
			_ = json.Unmarshal(inputs, &sc.Inputs)
			d.Scores = append(d.Scores, sc)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `
			SELECT day, item_count, mention_count, source_count, primary_count, engagement
			FROM trend_observations WHERE topic_id = $1 ORDER BY day DESC LIMIT 120`, t.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var o Observation
			if err := rows.Scan(&o.Day, &o.ItemCount, &o.MentionCount, &o.SourceCount, &o.PrimaryCount, &o.Engagement); err != nil {
				rows.Close()
				return err
			}
			d.Observations = append(d.Observations, o)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if d.Items, err = itemsTx(ctx, tx, key, false, 25); err != nil {
			return err
		}
		d.Knowledge, err = knowledgeUpdatesTx(ctx, tx, `t.topic_key = $1`, key)
		return err
	})
	if err != nil {
		return TopicDetail{}, err
	}
	d.Reports, err = s.ListReports(ctx, tenantID, key, "", 10)
	return d, err
}

const itemCols = `i.id::text, i.source_id::text, s.source_key, s.name, i.external_id, i.url, i.canonical_url, i.title, i.summary,
	i.authors, i.published_at, i.retrieved_at, i.retrieval_metadata, i.series_key, i.version_label, i.engagement,
	COALESCE(i.canonical_item_id::text, ''), COALESCE(i.duplicate_kind, ''), i.duplicate_score::float8, i.maturity, i.significance::float8`

func scanItem(row pgx.Row) (Item, error) {
	var it Item
	var meta []byte
	if err := row.Scan(&it.ID, &it.SourceID, &it.SourceKey, &it.SourceName, &it.ExternalID, &it.URL, &it.CanonicalURL, &it.Title,
		&it.Summary, &it.Authors, &it.PublishedAt, &it.RetrievedAt, &meta, &it.SeriesKey, &it.VersionLabel, &it.Engagement,
		&it.CanonicalItemID, &it.DuplicateKind, &it.DuplicateScore, &it.Maturity, &it.Significance); err != nil {
		return Item{}, err
	}
	_ = json.Unmarshal(meta, &it.RetrievalMetadata)
	return it, nil
}

func itemsTx(ctx context.Context, tx pgx.Tx, topicKey string, includeDuplicates bool, limit int) ([]Item, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+itemCols+`
		FROM trend_items i JOIN trend_sources s ON s.id = i.source_id
		WHERE ($1 OR i.canonical_item_id IS NULL)
		  AND ($2 = '' OR EXISTS (SELECT 1 FROM trend_item_topics it JOIN trend_topics t ON t.id = it.topic_id
		                          WHERE t.topic_key = $2 AND it.item_id = COALESCE(i.canonical_item_id, i.id)))
		ORDER BY i.published_at DESC NULLS LAST, i.created_at DESC LIMIT $3`, includeDuplicates, topicKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Item{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ListItems lists trend items (canonical only unless includeDuplicates).
func (s *Store) ListItems(ctx context.Context, tenantID, topicKey string, includeDuplicates bool, limit int) ([]Item, error) {
	var out []Item
	err := s.read(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = itemsTx(ctx, tx, topicKey, includeDuplicates, clampLimit(limit, 50, 500))
		return err
	})
	return out, err
}

// GetItem returns one item with its topics and its duplicates.
func (s *Store) GetItem(ctx context.Context, tenantID, id string) (Item, error) {
	if !ids.Valid(id) {
		return Item{}, ErrNotFound
	}
	var it Item
	err := s.read(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		it, err = scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+` FROM trend_items i JOIN trend_sources s ON s.id = i.source_id WHERE i.id = $1`, id))
		if err != nil {
			return err
		}
		canon := it.ID
		if it.CanonicalItemID != "" {
			canon = it.CanonicalItemID
		}
		rows, err := tx.Query(ctx, `
			SELECT t.topic_key, t.facet, it.confidence::float8, it.matched_terms
			FROM trend_item_topics it JOIN trend_topics t ON t.id = it.topic_id WHERE it.item_id = $1 ORDER BY it.confidence DESC, t.topic_key`, canon)
		if err != nil {
			return err
		}
		for rows.Next() {
			var tp ItemTopic
			if err := rows.Scan(&tp.TopicKey, &tp.Facet, &tp.Confidence, &tp.MatchedTerms); err != nil {
				rows.Close()
				return err
			}
			it.Topics = append(it.Topics, tp)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT `+itemCols+` FROM trend_items i JOIN trend_sources s ON s.id = i.source_id
			WHERE i.canonical_item_id = $1 ORDER BY i.created_at`, it.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanItem(rows)
			if err != nil {
				return err
			}
			it.Duplicates = append(it.Duplicates, d)
		}
		return rows.Err()
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	return it, err
}

func knowledgeUpdatesTx(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]KnowledgeUpdate, error) {
	rows, err := tx.Query(ctx, `
		SELECT ku.id::text, ku.report_id::text, t.topic_key, ku.document_id::text, ku.status, ku.published_at,
		       r.title, r.confidence::float8, r.confidence_band, r.maturity, r.stale_after,
		       COALESCE((SELECT array_agg(DISTINCT n.agent_type ORDER BY n.agent_type) FROM agent_knowledge_notifications n WHERE n.knowledge_update_id = ku.id), '{}')
		FROM knowledge_updates ku JOIN trend_topics t ON t.id = ku.topic_id JOIN research_reports r ON r.id = ku.report_id
		WHERE `+where+` ORDER BY ku.published_at DESC LIMIT 100`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KnowledgeUpdate{}
	for rows.Next() {
		var k KnowledgeUpdate
		if err := rows.Scan(&k.ID, &k.ReportID, &k.TopicKey, &k.DocumentID, &k.Status, &k.PublishedAt, &k.Title, &k.Confidence,
			&k.Band, &k.Maturity, &k.StaleAfter, &k.Notified); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ListKnowledgeUpdates lists published Pulse knowledge (optionally by topic and status).
func (s *Store) ListKnowledgeUpdates(ctx context.Context, tenantID, topicKey, status string) ([]KnowledgeUpdate, error) {
	var out []KnowledgeUpdate
	err := s.read(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = knowledgeUpdatesTx(ctx, tx, `($1 = '' OR t.topic_key = $1) AND ($2 = '' OR ku.status = $2)`, topicKey, status)
		return err
	})
	return out, err
}

// ListSubscriptions lists agent-domain subscriptions.
func (s *Store) ListSubscriptions(ctx context.Context, tenantID, agentType string) ([]Subscription, error) {
	out := []Subscription{}
	err := s.read(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT s.id::text, s.agent_type, t.topic_key, s.min_confidence::float8, s.active
			FROM agent_knowledge_subscriptions s JOIN trend_topics t ON t.id = s.topic_id
			WHERE ($1 = '' OR s.agent_type = $1) ORDER BY s.agent_type, t.topic_key`, agentType)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sub Subscription
			if err := rows.Scan(&sub.ID, &sub.AgentType, &sub.TopicKey, &sub.MinConfidence, &sub.Active); err != nil {
				return err
			}
			out = append(out, sub)
		}
		return rows.Err()
	})
	return out, err
}

// ListNotifications returns an agent domain's knowledge inbox, newest first.
func (s *Store) ListNotifications(ctx context.Context, tenantID, agentType string, since time.Time, limit int) ([]Notification, error) {
	out := []Notification{}
	err := s.read(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT n.id::text, n.agent_type, n.knowledge_update_id::text, ku.document_id::text, t.topic_key, r.title, n.created_at
			FROM agent_knowledge_notifications n
			JOIN knowledge_updates ku ON ku.id = n.knowledge_update_id
			JOIN trend_topics t ON t.id = ku.topic_id JOIN research_reports r ON r.id = ku.report_id
			WHERE ($1 = '' OR n.agent_type = $1) AND n.created_at >= $2 AND ku.status = 'PUBLISHED'
			ORDER BY n.created_at DESC LIMIT $3`, agentType, since, clampLimit(limit, 50, 500))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n Notification
			if err := rows.Scan(&n.ID, &n.AgentType, &n.KnowledgeUpdateID, &n.DocumentID, &n.TopicKey, &n.Title, &n.CreatedAt); err != nil {
				return err
			}
			out = append(out, n)
		}
		return rows.Err()
	})
	return out, err
}

func clampLimit(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}
