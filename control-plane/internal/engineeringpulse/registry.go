package engineeringpulse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/knowledge"
	"github.com/systera/control-plane/internal/platform/postgres"
)

// Store persists the Engineering Pulse pipeline. Writes run as systera_system (platform-wide
// public-source data); reads for API callers run as systera_app.
type Store struct {
	pool      *pgxpool.Pool
	knowledge *knowledge.Store
	now       func() time.Time
}

// NewStore returns a Store. ks publishes validated reports as PUBLIC knowledge.
func NewStore(pool *pgxpool.Pool, ks *knowledge.Store) *Store {
	return &Store{pool: pool, knowledge: ks, now: time.Now}
}

// SetClock overrides the clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// RegistryFile is knowledge/source-registry/sources.json.
type RegistryFile struct {
	Sources []Source `json:"sources"`
}

// LoadRegistryFile reads a registry definition file.
func LoadRegistryFile(path string) ([]Source, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f RegistryFile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return f.Sources, nil
}

var sourceKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,79}$`)

func validateSourceDef(d Source) error {
	var p []string
	if !sourceKeyRe.MatchString(d.Key) {
		p = append(p, "source_key must be a lower-case slug")
	}
	if strings.TrimSpace(d.Name) == "" {
		p = append(p, "name is required")
	}
	switch d.Connector {
	case ConnectorGitHubReleases, ConnectorRSS, ConnectorArXiv:
	default:
		p = append(p, "connector must be GITHUB_RELEASES, RSS or ARXIV")
	}
	if u, err := url.Parse(d.FeedURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		p = append(p, "feed_url must be an absolute http(s) URL")
	}
	switch d.SourceType {
	case "PRIMARY_DOC", "RFC", "RELEASE_NOTES", "ENGINEERING_BLOG", "PAPER", "COMMUNITY":
	default:
		p = append(p, "source_type is invalid")
	}
	if d.Reliability < 0 || d.Reliability > 1 {
		p = append(p, "reliability must be within [0,1]")
	}
	switch d.AllowedContent {
	case ContentMetadataOnly, ContentSummary, ContentFullText:
	default:
		p = append(p, "allowed_content must be METADATA_ONLY, SUMMARY or FULL_TEXT")
	}
	if d.TermsURL == "" || d.TermsSummary == "" {
		p = append(p, "terms_url and terms_summary are required (record the source's terms of use)")
	}
	if d.RateLimitPerHour < 1 || d.MinIntervalSeconds < 1 {
		p = append(p, "rate_limit_per_hour and min_interval_seconds must be positive")
	}
	if len(p) > 0 {
		return fmt.Errorf("%w: source %q: %s", ErrInvalidInput, d.Key, strings.Join(p, "; "))
	}
	return nil
}

// SyncResult summarizes a registry sync.
type SyncResult struct {
	Created     []string `json:"created"`
	Updated     []string `json:"updated"`
	NeedsReview []string `json:"needs_review"` // approved sources whose terms changed
}

// SyncSources upserts registry definitions by source_key. New sources start PENDING_REVIEW;
// approval is a separate, attributable act. If the terms, allowed content or feed URL of an
// approved source change, it returns to PENDING_REVIEW until re-approved.
func (s *Store) SyncSources(ctx context.Context, defs []Source) (SyncResult, error) {
	var res SyncResult
	for _, d := range defs {
		if d.MaxSummaryChars == 0 {
			d.MaxSummaryChars = 1000
		}
		if d.Licence == "" {
			d.Licence = "UNKNOWN"
		}
		if err := validateSourceDef(d); err != nil {
			return res, err
		}
	}
	err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		for _, d := range defs {
			cfg, _ := json.Marshal(d.Config)
			if string(cfg) == "null" {
				cfg = []byte("{}")
			}
			var inserted, review bool
			err := tx.QueryRow(ctx, `
				WITH prev AS (
					SELECT status, terms_url, terms_summary, allowed_content, feed_url, max_summary_chars
					FROM trend_sources WHERE source_key = $1
				)
				INSERT INTO trend_sources (source_key, name, connector, feed_url, homepage_url, source_type,
					is_primary_source, reliability, terms_url, terms_summary, licence, allowed_content,
					max_summary_chars, attribution_required, rate_limit_per_hour, min_interval_seconds, config)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
				ON CONFLICT (source_key) DO UPDATE SET
					name = EXCLUDED.name, connector = EXCLUDED.connector, feed_url = EXCLUDED.feed_url,
					homepage_url = EXCLUDED.homepage_url, source_type = EXCLUDED.source_type,
					is_primary_source = EXCLUDED.is_primary_source, reliability = EXCLUDED.reliability,
					terms_url = EXCLUDED.terms_url, terms_summary = EXCLUDED.terms_summary, licence = EXCLUDED.licence,
					allowed_content = EXCLUDED.allowed_content, max_summary_chars = EXCLUDED.max_summary_chars,
					attribution_required = EXCLUDED.attribution_required,
					rate_limit_per_hour = EXCLUDED.rate_limit_per_hour, min_interval_seconds = EXCLUDED.min_interval_seconds,
					config = EXCLUDED.config,
					status = CASE WHEN trend_sources.status = 'APPROVED' AND (
							trend_sources.terms_url <> EXCLUDED.terms_url OR trend_sources.terms_summary <> EXCLUDED.terms_summary
							OR trend_sources.allowed_content <> EXCLUDED.allowed_content OR trend_sources.feed_url <> EXCLUDED.feed_url
							OR trend_sources.max_summary_chars < EXCLUDED.max_summary_chars)
						THEN 'PENDING_REVIEW' ELSE trend_sources.status END,
					updated_at = NOW()
				RETURNING (xmax = 0),
					COALESCE((SELECT status FROM prev), '') = 'APPROVED' AND status = 'PENDING_REVIEW'`,
				d.Key, d.Name, d.Connector, d.FeedURL, d.HomepageURL, d.SourceType, d.IsPrimarySource, d.Reliability,
				d.TermsURL, d.TermsSummary, d.Licence, d.AllowedContent, d.MaxSummaryChars, d.AttributionRequired,
				d.RateLimitPerHour, d.MinIntervalSeconds, cfg).Scan(&inserted, &review)
			if err != nil {
				return fmt.Errorf("sync source %s: %w", d.Key, err)
			}
			switch {
			case inserted:
				res.Created = append(res.Created, d.Key)
			case review:
				res.NeedsReview = append(res.NeedsReview, d.Key)
				if _, err := tx.Exec(ctx, `UPDATE trend_sources SET approved_by = NULL, approved_at = NULL WHERE source_key = $1`, d.Key); err != nil {
					return err
				}
			default:
				res.Updated = append(res.Updated, d.Key)
			}
		}
		return nil
	})
	return res, err
}

// ApproveSource records that approver reviewed the source's terms and allows fetching.
func (s *Store) ApproveSource(ctx context.Context, key, approver string) (Source, error) {
	if strings.TrimSpace(approver) == "" {
		return Source{}, fmt.Errorf("%w: approver is required", ErrInvalidInput)
	}
	var src Source
	err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE trend_sources SET status = 'APPROVED', approved_by = $2, approved_at = $3,
			       consecutive_failures = 0, last_error = '', next_fetch_at = $3, updated_at = NOW()
			WHERE source_key = $1 AND status IN ('PENDING_REVIEW', 'SUSPENDED', 'APPROVED')`, key, approver, s.now().UTC())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		src, err = getSourceTx(ctx, tx, key)
		return err
	})
	return src, err
}

// SetSourceStatus suspends or retires a source (or returns it to review).
func (s *Store) SetSourceStatus(ctx context.Context, key, status, reason string) error {
	switch status {
	case SourceSuspended, SourceRetired, SourcePending:
	default:
		return fmt.Errorf("%w: status must be SUSPENDED, RETIRED or PENDING_REVIEW (use ApproveSource to approve)", ErrInvalidInput)
	}
	return postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE trend_sources SET status = $2, last_error = CASE WHEN $3 = '' THEN last_error ELSE $3 END,
			       approved_by = CASE WHEN $2 = 'PENDING_REVIEW' THEN NULL ELSE approved_by END,
			       approved_at = CASE WHEN $2 = 'PENDING_REVIEW' THEN NULL ELSE approved_at END, updated_at = NOW()
			WHERE source_key = $1`, key, status, reason)
		if err == nil && tag.RowsAffected() == 0 {
			err = ErrNotFound
		}
		return err
	})
}

const sourceCols = `id::text, source_key, name, connector, feed_url, homepage_url, source_type, is_primary_source,
	reliability::float8, terms_url, terms_summary, licence, allowed_content, max_summary_chars, attribution_required,
	rate_limit_per_hour, min_interval_seconds, config, status, COALESCE(approved_by, ''), approved_at, etag, last_modified,
	last_fetched_at, next_fetch_at, consecutive_failures, last_error`

func scanSource(row pgx.Row) (Source, error) {
	var s Source
	var cfg []byte
	err := row.Scan(&s.ID, &s.Key, &s.Name, &s.Connector, &s.FeedURL, &s.HomepageURL, &s.SourceType, &s.IsPrimarySource,
		&s.Reliability, &s.TermsURL, &s.TermsSummary, &s.Licence, &s.AllowedContent, &s.MaxSummaryChars, &s.AttributionRequired,
		&s.RateLimitPerHour, &s.MinIntervalSeconds, &cfg, &s.Status, &s.ApprovedBy, &s.ApprovedAt, &s.ETag, &s.LastModified,
		&s.LastFetchedAt, &s.NextFetchAt, &s.ConsecutiveFailures, &s.LastError)
	if err != nil {
		return Source{}, err
	}
	_ = json.Unmarshal(cfg, &s.Config)
	return s, nil
}

func getSourceTx(ctx context.Context, tx pgx.Tx, key string) (Source, error) {
	src, err := scanSource(tx.QueryRow(ctx, `SELECT `+sourceCols+` FROM trend_sources WHERE source_key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, ErrNotFound
	}
	return src, err
}

// ListSources returns the registry (any role).
func (s *Store) ListSources(ctx context.Context, tenantID string) ([]Source, error) {
	out := []Source{}
	err := s.read(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+sourceCols+` FROM trend_sources ORDER BY source_key`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			src, err := scanSource(rows)
			if err != nil {
				return err
			}
			out = append(out, src)
		}
		return rows.Err()
	})
	return out, err
}

// read runs fn as the tenant-bound app role (Pulse tables are readable to every tenant);
// with tenantID "" it runs as the system role (worker / admin).
func (s *Store) read(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if tenantID == "" {
		return postgres.WithSystemTx(ctx, s.pool, fn)
	}
	return postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.AppRole, fn)
}
