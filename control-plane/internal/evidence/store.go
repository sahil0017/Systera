package evidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
)

var hexHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Store is the PostgreSQL evidence ledger.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool, now: time.Now} }

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// modelProject resolves the model's project and current graph version (tenant-scoped).
func modelProject(ctx context.Context, tx pgx.Tx, modelID string) (projectID string, current int, err error) {
	if !ids.Valid(modelID) {
		return "", 0, ErrModelNotFound
	}
	err = tx.QueryRow(ctx, `
		SELECT m.project_id::text, v.version_number FROM architecture_models m
		JOIN architecture_versions v ON v.id = m.current_version_id WHERE m.id = $1`, modelID).Scan(&projectID, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, ErrModelNotFound
	}
	return projectID, current, err
}

func (s *Store) emit(ctx context.Context, tx pgx.Tx, tenantID, projectID, runID, eventType string, payload map[string]any) error {
	_, err := outbox.InsertTx(ctx, tx, outbox.EventEnvelope{
		EventType: eventType, TenantID: tenantID, ProjectID: projectID, RunID: runID,
		CorrelationID: ids.New(), Payload: payload,
	})
	return err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func validActor(a Actor) error {
	if !slices.Contains([]string{"USER", "AGENT", "SYSTEM"}, a.Type) || a.ID == "" {
		return fmt.Errorf("%w: actor type and id are required", ErrInvalidInput)
	}
	if (a.RunID != "" && !ids.Valid(a.RunID)) || (a.AgentTaskID != "" && !ids.Valid(a.AgentTaskID)) {
		return fmt.Errorf("%w: run_id and agent_task_id must be UUIDs", ErrInvalidInput)
	}
	return nil
}

// provenance checks the actor's run/task belong to the model's project.
func provenance(ctx context.Context, tx pgx.Tx, projectID string, a Actor) []Finding {
	var fs []Finding
	if a.RunID != "" {
		var ok bool
		_ = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM design_runs WHERE id = $1 AND project_id = $2)`, a.RunID, projectID).Scan(&ok)
		if !ok {
			fs = append(fs, Finding{Field: "run_id", Code: "unknown_run", Message: "run_id is not a run of this model's project"})
		}
	}
	if a.AgentTaskID != "" {
		var ok bool
		_ = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_tasks WHERE id = $1 AND run_id = $2)`, a.AgentTaskID, a.RunID).Scan(&ok)
		if !ok {
			fs = append(fs, Finding{Field: "agent_task_id", Code: "unknown_agent_task", Message: "agent_task_id is not a task of run_id"})
		}
	}
	return fs
}

// liveNodes returns which of ids are live nodes of the model at version.
func liveNodes(ctx context.Context, tx pgx.Tx, modelID string, version int, nodeIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(nodeIDs) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT node_id::text FROM architecture_nodes
		WHERE model_id = $1 AND valid_from_version <= $2 AND (valid_to_version IS NULL OR valid_to_version > $2)
		  AND node_id = ANY($3::uuid[])`, modelID, version, nodeIDs)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	for _, id := range list {
		out[id] = true
	}
	return out, err
}

func subjectFindings(ctx context.Context, tx pgx.Tx, modelID string, version int, nodeIDs []string) ([]Finding, error) {
	var fs []Finding
	for _, id := range nodeIDs {
		if !ids.Valid(id) {
			return []Finding{{Field: "subject_node_ids", Code: "invalid_node_id", Message: "subject_node_ids must be UUIDs"}}, nil
		}
	}
	live, err := liveNodes(ctx, tx, modelID, version, nodeIDs)
	if err != nil {
		return nil, err
	}
	for _, id := range nodeIDs {
		if !live[id] {
			fs = append(fs, Finding{Field: "subject_node_ids", Code: "unknown_node", Message: fmt.Sprintf("node %s is not in the current graph version", id)})
		}
	}
	return fs, nil
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// isCheck reports whether err is a database rule violation (the backstop).
func isCheck(err error) (*pgconn.PgError, bool) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && (pe.Code == "23514" || pe.Code == "23001" || pe.Code == "23503") {
		return pe, true
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// Evidence
// ---------------------------------------------------------------------------

const evidenceCols = `id::text, model_id::text, source_ref, source_type, directness, content_hash, excerpt, captured_at,
	trust_level, access_policy, expires_at, status, COALESCE(status_reason, ''), status_changed_at, created_by_type, created_by, created_at`

func scanEvidence(row pgx.Row) (Evidence, error) {
	var e Evidence
	err := row.Scan(&e.ID, &e.ModelID, &e.SourceRef, &e.SourceType, &e.Directness, &e.ContentHash, &e.Excerpt, &e.CapturedAt,
		&e.TrustLevel, &e.AccessPolicy, &e.ExpiresAt, &e.Status, &e.StatusReason, &e.StatusChangedAt, &e.CreatedByType, &e.CreatedBy, &e.CreatedAt)
	e.Ref = "evidence:" + e.ID
	e.CapturedAt, e.CreatedAt = e.CapturedAt.UTC(), e.CreatedAt.UTC()
	return e, err
}

// CaptureEvidence records evidence. It is idempotent on (source_ref, content_hash): the
// same content from the same source returns the existing record with created=false.
func (s *Store) CaptureEvidence(ctx context.Context, tenantID, modelID string, in EvidenceInput, a Actor) (Evidence, bool, error) {
	if err := validActor(a); err != nil {
		return Evidence{}, false, err
	}
	now := s.now().UTC()
	var fs []Finding
	in.SourceRef = strings.TrimSpace(in.SourceRef)
	if in.SourceRef == "" || len(in.SourceRef) > 2000 {
		fs = append(fs, Finding{Field: "source_ref", Code: "invalid_source_ref", Message: "source_ref is required (max 2000 characters)"})
	}
	if !slices.Contains(SourceTypes, in.SourceType) {
		fs = append(fs, Finding{Field: "source_type", Code: "invalid_source_type", Message: fmt.Sprintf("source_type must be one of %v", SourceTypes)})
	}
	if in.ContentHash == "" && in.Content != "" {
		sum := sha256.Sum256([]byte(in.Content))
		in.ContentHash = hex.EncodeToString(sum[:])
	}
	in.ContentHash = strings.ToLower(in.ContentHash)
	if !hexHash.MatchString(in.ContentHash) {
		fs = append(fs, Finding{Field: "content_hash", Code: "invalid_content_hash", Message: "content_hash (hex SHA-256) or content is required"})
	}
	if !slices.Contains(TrustLevels, in.TrustLevel) {
		fs = append(fs, Finding{Field: "trust_level", Code: "invalid_trust_level", Message: fmt.Sprintf("trust_level must be one of %v", TrustLevels)})
	}
	if in.AccessPolicy == "" {
		in.AccessPolicy = "TENANT"
	}
	if !slices.Contains(AccessPolicies, in.AccessPolicy) {
		fs = append(fs, Finding{Field: "access_policy", Code: "invalid_access_policy", Message: fmt.Sprintf("access_policy must be one of %v", AccessPolicies)})
	}
	if len(in.Excerpt) > 4000 {
		fs = append(fs, Finding{Field: "excerpt", Code: "excerpt_too_long", Message: "excerpt is limited to 4000 characters"})
	}
	captured := now
	if in.CapturedAt != nil {
		captured = in.CapturedAt.UTC()
		if captured.After(now.Add(5 * time.Minute)) {
			fs = append(fs, Finding{Field: "captured_at", Code: "captured_in_future", Message: "captured_at cannot be in the future"})
		}
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(captured) {
		fs = append(fs, Finding{Field: "expires_at", Code: "invalid_expiry", Message: "expires_at must be after captured_at"})
	}

	var (
		ev      Evidence
		created bool
	)
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		projectID, _, err := modelProject(ctx, tx, modelID)
		if err != nil {
			return err
		}
		fs = append(fs, provenance(ctx, tx, projectID, a)...)
		if len(fs) > 0 {
			return &ValidationError{Findings: fs}
		}
		ev, err = scanEvidence(tx.QueryRow(ctx, `
			INSERT INTO evidence_records (tenant_id, model_id, source_ref, source_type, content_hash, excerpt, captured_at,
			                              trust_level, access_policy, expires_at, created_by_type, created_by, run_id, agent_task_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			ON CONFLICT (model_id, source_ref, content_hash) DO NOTHING
			RETURNING `+evidenceCols,
			tenantID, modelID, in.SourceRef, in.SourceType, in.ContentHash, in.Excerpt, captured, in.TrustLevel, in.AccessPolicy,
			in.ExpiresAt, a.Type, a.ID, nullable(a.RunID), nullable(a.AgentTaskID)))
		if errors.Is(err, pgx.ErrNoRows) {
			ev, err = scanEvidence(tx.QueryRow(ctx, `SELECT `+evidenceCols+` FROM evidence_records
				WHERE model_id = $1 AND source_ref = $2 AND content_hash = $3`, modelID, in.SourceRef, in.ContentHash))
			return err
		}
		if err != nil {
			return err
		}
		created = true
		return s.emit(ctx, tx, tenantID, projectID, a.RunID, EventEvidenceCaptured, map[string]any{
			"model_id": modelID, "evidence_id": ev.ID, "source_type": ev.SourceType, "trust_level": ev.TrustLevel,
		})
	})
	return ev, created, err
}

func (s *Store) GetEvidence(ctx context.Context, tenantID, modelID, id string) (Evidence, error) {
	if !ids.Valid(id) || !ids.Valid(modelID) {
		return Evidence{}, ErrNotFound
	}
	var ev Evidence
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var err error
		ev, err = scanEvidence(tx.QueryRow(ctx, `SELECT `+evidenceCols+` FROM evidence_records WHERE id = $1 AND model_id = $2`, id, modelID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Evidence{}, ErrNotFound
	}
	return ev, err
}

// ListEvidence lists a model's evidence, optionally filtered by status.
func (s *Store) ListEvidence(ctx context.Context, tenantID, modelID, status string, limit int) ([]Evidence, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	out := []Evidence{}
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if _, _, err := modelProject(ctx, tx, modelID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+evidenceCols+` FROM evidence_records
			WHERE model_id = $1 AND ($2 = '' OR status = $2) ORDER BY created_at DESC, id LIMIT $3`, modelID, status, limit)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Evidence, error) { return scanEvidence(r) })
		out = append(out, list...)
		return err
	})
	return out, err
}

// RevokeEvidence withdraws evidence; claims that no longer meet their requirements
// become STALE in the same transaction.
func (s *Store) RevokeEvidence(ctx context.Context, tenantID, modelID, id, reason string, a Actor) (Evidence, int, error) {
	if strings.TrimSpace(reason) == "" {
		return Evidence{}, 0, &ValidationError{Findings: []Finding{{Field: "reason", Code: "missing_reason", Message: "a reason is required"}}}
	}
	var (
		ev    Evidence
		stale int
	)
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		projectID, _, err := modelProject(ctx, tx, modelID)
		if err != nil {
			return err
		}
		if !ids.Valid(id) {
			return ErrNotFound
		}
		ev, err = scanEvidence(tx.QueryRow(ctx, `
			UPDATE evidence_records SET status = 'REVOKED', status_reason = $3, status_changed_at = NOW()
			WHERE id = $1 AND model_id = $2 AND status = 'ACTIVE' RETURNING `+evidenceCols, id, modelID, reason))
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			_ = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM evidence_records WHERE id = $1 AND model_id = $2)`, id, modelID).Scan(&exists)
			if exists {
				return fmt.Errorf("%w: evidence is not ACTIVE", ErrInvalidState)
			}
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := s.emit(ctx, tx, tenantID, projectID, a.RunID, EventEvidenceRevoked, map[string]any{"model_id": modelID, "evidence_id": id, "reason": reason}); err != nil {
			return err
		}
		stale, err = s.staleDependents(ctx, tx, tenantID, `SELECT claim_id FROM claim_evidence WHERE evidence_id = $1`, id)
		return err
	})
	return ev, stale, err
}

// staleDependents marks ACTIVE claims selected by depQuery that no longer satisfy the
// ledger rules as STALE (with the rule that failed as the reason) and emits claim.stale.
func (s *Store) staleDependents(ctx context.Context, tx pgx.Tx, tenantID, depQuery string, args ...any) (int, error) {
	rows, err := tx.Query(ctx, `
		UPDATE claims c SET lifecycle = 'STALE', stale_reason = claim_requirements_unmet(c), stale_since = NOW()
		WHERE c.lifecycle = 'ACTIVE' AND c.id IN (`+depQuery+`) AND claim_requirements_unmet(c) IS NOT NULL
		RETURNING c.id::text, c.model_id::text, c.stale_reason,
		          (SELECT project_id::text FROM architecture_models m WHERE m.id = c.model_id)`, args...)
	if err != nil {
		return 0, err
	}
	type staled struct{ id, model, reason, project string }
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (staled, error) {
		var st staled
		return st, r.Scan(&st.id, &st.model, &st.reason, &st.project)
	})
	if err != nil {
		return 0, err
	}
	for _, st := range list {
		if err := s.emit(ctx, tx, tenantID, st.project, "", EventClaimStale, map[string]any{
			"model_id": st.model, "claim_id": st.id, "reason": st.reason,
		}); err != nil {
			return 0, err
		}
	}
	return len(list), nil
}

// ---------------------------------------------------------------------------
// Assumptions
// ---------------------------------------------------------------------------

const assumptionCols = `id::text, model_id::text, statement, rationale, owner, risk_if_wrong, validation_method,
	subject_node_ids::text[], status, COALESCE(status_note, ''), COALESCE(status_changed_by, ''), status_changed_at,
	expires_at, created_by, created_at`

func scanAssumption(row pgx.Row) (Assumption, error) {
	var a Assumption
	err := row.Scan(&a.ID, &a.ModelID, &a.Statement, &a.Rationale, &a.Owner, &a.RiskIfWrong, &a.ValidationMethod,
		&a.SubjectNodeIDs, &a.Status, &a.StatusNote, &a.StatusChangedBy, &a.StatusChangedAt, &a.ExpiresAt, &a.CreatedBy, &a.CreatedAt)
	if a.SubjectNodeIDs == nil {
		a.SubjectNodeIDs = []string{}
	}
	a.CreatedAt = a.CreatedAt.UTC()
	return a, err
}

// RecordAssumption records an explicit assumption with an owner, rationale and risk.
func (s *Store) RecordAssumption(ctx context.Context, tenantID, modelID string, in AssumptionInput, a Actor) (Assumption, error) {
	if err := validActor(a); err != nil {
		return Assumption{}, err
	}
	var fs []Finding
	in.Statement, in.Rationale = strings.TrimSpace(in.Statement), strings.TrimSpace(in.Rationale)
	if in.Statement == "" || len(in.Statement) > 2000 {
		fs = append(fs, Finding{Field: "statement", Code: "invalid_statement", Message: "statement is required (max 2000 characters)"})
	}
	if in.Rationale == "" {
		fs = append(fs, Finding{Field: "rationale", Code: "missing_rationale", Message: "an assumption must say why it is being made"})
	}
	if !slices.Contains(RiskLevels, in.RiskIfWrong) {
		fs = append(fs, Finding{Field: "risk_if_wrong", Code: "invalid_risk", Message: fmt.Sprintf("risk_if_wrong must be one of %v", RiskLevels)})
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(s.now()) {
		fs = append(fs, Finding{Field: "expires_at", Code: "invalid_expiry", Message: "expires_at must be in the future"})
	}
	if in.Owner == "" {
		in.Owner = a.ID
	}
	in.SubjectNodeIDs = dedup(in.SubjectNodeIDs)

	var out Assumption
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		projectID, current, err := modelProject(ctx, tx, modelID)
		if err != nil {
			return err
		}
		sf, err := subjectFindings(ctx, tx, modelID, current, in.SubjectNodeIDs)
		if err != nil {
			return err
		}
		fs = append(fs, sf...)
		fs = append(fs, provenance(ctx, tx, projectID, a)...)
		if len(fs) > 0 {
			return &ValidationError{Findings: fs}
		}
		out, err = scanAssumption(tx.QueryRow(ctx, `
			INSERT INTO assumptions (tenant_id, model_id, statement, rationale, owner, risk_if_wrong, validation_method,
			                         subject_node_ids, expires_at, created_by_type, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::uuid[], $9, $10, $11) RETURNING `+assumptionCols,
			tenantID, modelID, in.Statement, in.Rationale, in.Owner, in.RiskIfWrong, in.ValidationMethod, in.SubjectNodeIDs,
			in.ExpiresAt, a.Type, a.ID))
		if err != nil {
			return err
		}
		return s.emit(ctx, tx, tenantID, projectID, a.RunID, EventAssumptionRecorded, map[string]any{
			"model_id": modelID, "assumption_id": out.ID, "risk_if_wrong": out.RiskIfWrong,
		})
	})
	return out, err
}

var assumptionTransitions = map[string][]string{
	"ACTIVE":             {"VALIDATED", "INVALIDATED", "RETIRED", "NEEDS_REVALIDATION"},
	"VALIDATED":          {"INVALIDATED", "RETIRED", "NEEDS_REVALIDATION"},
	"NEEDS_REVALIDATION": {"VALIDATED", "INVALIDATED", "RETIRED"},
}

// SetAssumptionStatus moves an assumption through its lifecycle. Anything but VALIDATED
// makes ACTIVE claims that rest on it STALE in the same transaction.
func (s *Store) SetAssumptionStatus(ctx context.Context, tenantID, modelID, id string, in AssumptionStatusInput, a Actor) (Assumption, int, error) {
	if strings.TrimSpace(in.Note) == "" {
		return Assumption{}, 0, &ValidationError{Findings: []Finding{{Field: "note", Code: "missing_note", Message: "a note explaining the change is required"}}}
	}
	if in.ExpiresAt != nil && (in.Status != "VALIDATED" || !in.ExpiresAt.After(s.now())) {
		return Assumption{}, 0, &ValidationError{Findings: []Finding{{Field: "expires_at", Code: "invalid_expiry", Message: "expires_at may only be set, in the future, when validating"}}}
	}
	var (
		out   Assumption
		stale int
	)
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		projectID, _, err := modelProject(ctx, tx, modelID)
		if err != nil {
			return err
		}
		if !ids.Valid(id) {
			return ErrNotFound
		}
		var cur string
		if err := tx.QueryRow(ctx, `SELECT status FROM assumptions WHERE id = $1 AND model_id = $2 FOR UPDATE`, id, modelID).Scan(&cur); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if !slices.Contains(assumptionTransitions[cur], in.Status) {
			return fmt.Errorf("%w: assumption cannot go from %s to %s", ErrInvalidState, cur, in.Status)
		}
		out, err = scanAssumption(tx.QueryRow(ctx, `
			UPDATE assumptions SET status = $3, status_note = $4, status_changed_by = $5, status_changed_at = NOW(),
			       expires_at = CASE WHEN $3 = 'VALIDATED' THEN $6 ELSE expires_at END
			WHERE id = $1 AND model_id = $2 RETURNING `+assumptionCols, id, modelID, in.Status, in.Note, a.ID, in.ExpiresAt))
		if err != nil {
			return err
		}
		if err := s.emit(ctx, tx, tenantID, projectID, a.RunID, EventAssumptionStatus, map[string]any{
			"model_id": modelID, "assumption_id": id, "from": cur, "to": in.Status,
		}); err != nil {
			return err
		}
		stale, err = s.staleDependents(ctx, tx, tenantID, `SELECT claim_id FROM claim_assumptions WHERE assumption_id = $1`, id)
		return err
	})
	return out, stale, err
}

func (s *Store) ListAssumptions(ctx context.Context, tenantID, modelID, status string) ([]Assumption, error) {
	out := []Assumption{}
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if _, _, err := modelProject(ctx, tx, modelID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+assumptionCols+` FROM assumptions WHERE model_id = $1 AND ($2 = '' OR status = $2)
			ORDER BY created_at DESC, id`, modelID, status)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Assumption, error) { return scanAssumption(r) })
		out = append(out, list...)
		return err
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Claims
// ---------------------------------------------------------------------------

const claimCols = `id::text, model_id::text, statement, claim_class, reasoning, confidence_band, confidence_score::float8,
	subject_node_ids::text[], graph_version, review_status, COALESCE(reviewer, ''), COALESCE(review_note, ''), reviewed_at,
	lifecycle, COALESCE(stale_reason, ''), stale_since, expires_at, COALESCE(revalidate_after::text, ''), last_validated_at,
	COALESCE(superseded_by::text, ''), COALESCE(retracted_reason, ''), created_by_type, created_by, created_at`

func scanClaim(row pgx.Row) (Claim, error) {
	var c Claim
	var score pgtype.Float8
	err := row.Scan(&c.ID, &c.ModelID, &c.Statement, &c.Class, &c.Reasoning, &c.ConfidenceBand, &score,
		&c.SubjectNodeIDs, &c.GraphVersion, &c.ReviewStatus, &c.Reviewer, &c.ReviewNote, &c.ReviewedAt,
		&c.Lifecycle, &c.StaleReason, &c.StaleSince, &c.ExpiresAt, &c.RevalidateAfter, &c.LastValidatedAt,
		&c.SupersededBy, &c.RetractedReason, &c.CreatedByType, &c.CreatedBy, &c.CreatedAt)
	if score.Valid {
		c.ConfidenceScore = &score.Float64
	}
	if c.SubjectNodeIDs == nil {
		c.SubjectNodeIDs = []string{}
	}
	c.Evidence, c.Assumptions = []LinkedEvidence{}, []Assumption{}
	c.CreatedAt, c.LastValidatedAt = c.CreatedAt.UTC(), c.LastValidatedAt.UTC()
	return c, err
}

func bandFor(score float64) string {
	switch {
	case score >= 0.8:
		return "HIGH"
	case score >= 0.5:
		return "MEDIUM"
	default:
		return "LOW"
	}
}

// linkFindings checks evidence links against the ledger and the claim's class/band, so a
// caller sees every unmet rule at once (the DB trigger is the backstop).
func linkFindings(ctx context.Context, tx pgx.Tx, modelID, class, band string, links []EvidenceLink, assumptionIDs []string) ([]Finding, error) {
	var fs []Finding
	seen := map[string]bool{}
	var evIDs []string
	for i, l := range links {
		field := fmt.Sprintf("evidence[%d]", i)
		switch {
		case !ids.Valid(l.EvidenceID):
			fs = append(fs, Finding{Field: field, Code: "invalid_evidence_id", Message: "evidence_id must be a UUID"})
			continue
		case !slices.Contains(LinkRelations, l.Relation):
			fs = append(fs, Finding{Field: field, Code: "invalid_relation", Message: fmt.Sprintf("relation must be one of %v", LinkRelations)})
		case seen[l.EvidenceID]:
			fs = append(fs, Finding{Field: field, Code: "duplicate_evidence", Message: "evidence linked twice"})
		}
		seen[l.EvidenceID] = true
		evIDs = append(evIDs, l.EvidenceID)
	}
	type evRow struct {
		status, directness, trust string
		expired                   bool
	}
	found := map[string]evRow{}
	if len(evIDs) > 0 {
		rows, err := tx.Query(ctx, `SELECT id::text, status, directness, trust_level, (expires_at IS NOT NULL AND expires_at <= NOW())
			FROM evidence_records WHERE model_id = $1 AND id = ANY($2::uuid[])`, modelID, evIDs)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var r evRow
			if err := rows.Scan(&id, &r.status, &r.directness, &r.trust, &r.expired); err != nil {
				rows.Close()
				return nil, err
			}
			found[id] = r
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	supports, directSupports, highTrustSupport, highTrustContradiction := 0, 0, false, false
	for i, l := range links {
		r, ok := found[l.EvidenceID]
		field := fmt.Sprintf("evidence[%d]", i)
		if !ids.Valid(l.EvidenceID) {
			continue
		}
		if !ok {
			fs = append(fs, Finding{Field: field, Code: "unknown_evidence", Message: "evidence does not exist in this model"})
			continue
		}
		usable := r.status == "ACTIVE" && !r.expired
		if !usable && l.Relation == "SUPPORTS" {
			fs = append(fs, Finding{Field: field, Code: "evidence_not_usable", Message: fmt.Sprintf("supporting evidence is %s", map[bool]string{true: "expired", false: r.status}[r.expired && r.status == "ACTIVE"])})
			continue
		}
		highTrust := r.trust == "VERIFIED" || r.trust == "HIGH"
		switch l.Relation {
		case "SUPPORTS":
			supports++
			if r.directness == "DIRECT" {
				directSupports++
			}
			if highTrust {
				highTrustSupport = true
			}
		case "CONTRADICTS":
			if usable && highTrust {
				highTrustContradiction = true
			}
		}
	}
	const inv = "claim_evidence_requirements"
	switch class {
	case "OBSERVED":
		if directSupports == 0 {
			fs = append(fs, Finding{Field: "evidence", Code: "missing_direct_evidence", Invariant: inv,
				Message: "OBSERVED claims require supporting, active, unexpired DIRECT evidence (the source material itself)"})
		}
		if len(assumptionIDs) > 0 {
			fs = append(fs, Finding{Field: "assumption_ids", Code: "observation_on_assumption", Invariant: inv,
				Message: "OBSERVED claims cannot rest on assumptions; classify the claim as INFERRED or PROPOSED"})
		}
	case "INFERRED":
		if supports == 0 {
			fs = append(fs, Finding{Field: "evidence", Code: "missing_evidence", Invariant: inv,
				Message: "INFERRED claims require supporting, active, unexpired evidence"})
		}
	}
	if band == "HIGH" && class != "PROPOSED" {
		if !highTrustSupport {
			fs = append(fs, Finding{Field: "confidence_band", Code: "confidence_not_supported", Invariant: "confidence_calibration",
				Message: "HIGH confidence requires supporting evidence with VERIFIED or HIGH trust"})
		}
		if highTrustContradiction {
			fs = append(fs, Finding{Field: "confidence_band", Code: "confidence_contradicted", Invariant: "confidence_calibration",
				Message: "HIGH confidence is not allowed while high-trust evidence contradicts the claim"})
		}
	}

	if len(assumptionIDs) > 0 {
		rows, err := tx.Query(ctx, `SELECT id::text, status, (expires_at IS NOT NULL AND expires_at <= NOW()) FROM assumptions
			WHERE model_id = $1 AND id = ANY($2::uuid[])`, modelID, assumptionIDs)
		if err != nil {
			return nil, err
		}
		type aRow struct {
			status  string
			expired bool
		}
		got := map[string]aRow{}
		for rows.Next() {
			var id string
			var r aRow
			if err := rows.Scan(&id, &r.status, &r.expired); err != nil {
				rows.Close()
				return nil, err
			}
			got[id] = r
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, id := range assumptionIDs {
			r, ok := got[id]
			switch {
			case !ok:
				fs = append(fs, Finding{Field: "assumption_ids", Code: "unknown_assumption", Message: fmt.Sprintf("assumption %s does not exist in this model", id)})
			case (r.status != "ACTIVE" && r.status != "VALIDATED") || r.expired:
				fs = append(fs, Finding{Field: "assumption_ids", Code: "assumption_not_usable", Message: fmt.Sprintf("assumption %s is %s", id, map[bool]string{true: "expired", false: r.status}[r.expired])})
			}
		}
	}
	return fs, nil
}

// MakeClaim records a claim with its evidence and assumption links. Edits are expressed
// by superseding: Supersedes marks the old claim SUPERSEDED in the same transaction.
func (s *Store) MakeClaim(ctx context.Context, tenantID, modelID string, in ClaimInput, a Actor) (Claim, error) {
	if err := validActor(a); err != nil {
		return Claim{}, err
	}
	var fs []Finding
	in.Statement, in.Reasoning = strings.TrimSpace(in.Statement), strings.TrimSpace(in.Reasoning)
	if in.Statement == "" || len(in.Statement) > 2000 {
		fs = append(fs, Finding{Field: "statement", Code: "invalid_statement", Message: "statement is required (max 2000 characters)"})
	}
	if !slices.Contains(ClaimClasses, in.Class) {
		fs = append(fs, Finding{Field: "claim_class", Code: "invalid_class", Message: fmt.Sprintf("claim_class must be one of %v", ClaimClasses)})
	}
	if in.Class == "INFERRED" && in.Reasoning == "" {
		fs = append(fs, Finding{Field: "reasoning", Code: "missing_reasoning", Invariant: "claim_evidence_requirements", Message: "INFERRED claims require reasoning"})
	}
	if !slices.Contains(ConfidenceBands, in.ConfidenceBand) {
		fs = append(fs, Finding{Field: "confidence_band", Code: "invalid_band", Message: fmt.Sprintf("confidence_band must be one of %v", ConfidenceBands)})
	}
	if sc := in.ConfidenceScore; sc != nil && (*sc < 0 || *sc > 1 || bandFor(*sc) != in.ConfidenceBand) {
		fs = append(fs, Finding{Field: "confidence_score", Code: "score_outside_band", Invariant: "confidence_calibration",
			Message: "confidence_score must be within 0..1 and inside its band (HIGH >= 0.8, MEDIUM 0.5-0.8, LOW < 0.5)"})
	}
	var revalidate *time.Duration
	if in.RevalidateAfter != "" {
		d, err := time.ParseDuration(in.RevalidateAfter)
		if err != nil || d < time.Minute {
			fs = append(fs, Finding{Field: "revalidate_after", Code: "invalid_revalidate_after", Message: `revalidate_after must be a duration of at least 1m, e.g. "720h"`})
		} else {
			revalidate = &d
		}
	}
	if in.ExpiresAt != nil && in.RevalidateAfter != "" {
		fs = append(fs, Finding{Field: "expires_at", Code: "conflicting_expiry", Message: "set expires_at or revalidate_after, not both"})
	}
	now := s.now().UTC()
	expires := in.ExpiresAt
	if expires != nil && !expires.After(now) {
		fs = append(fs, Finding{Field: "expires_at", Code: "invalid_expiry", Message: "expires_at must be in the future"})
	}
	if revalidate != nil {
		t := now.Add(*revalidate)
		expires = &t
	}
	if in.Supersedes != "" && !ids.Valid(in.Supersedes) {
		fs = append(fs, Finding{Field: "supersedes", Code: "invalid_claim_id", Message: "supersedes must be a claim id"})
	}
	in.SubjectNodeIDs, in.AssumptionIDs = dedup(in.SubjectNodeIDs), dedup(in.AssumptionIDs)
	for _, id := range in.AssumptionIDs {
		if !ids.Valid(id) {
			fs = append(fs, Finding{Field: "assumption_ids", Code: "invalid_assumption_id", Message: "assumption_ids must be UUIDs"})
			break
		}
	}

	var out Claim
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		projectID, current, err := modelProject(ctx, tx, modelID)
		if err != nil {
			return err
		}
		sf, err := subjectFindings(ctx, tx, modelID, current, in.SubjectNodeIDs)
		if err != nil {
			return err
		}
		fs = append(fs, sf...)
		lf, err := linkFindings(ctx, tx, modelID, in.Class, in.ConfidenceBand, in.Evidence, in.AssumptionIDs)
		if err != nil {
			return err
		}
		fs = append(fs, lf...)
		fs = append(fs, provenance(ctx, tx, projectID, a)...)
		if in.Supersedes != "" && ids.Valid(in.Supersedes) {
			var lifecycle string
			err := tx.QueryRow(ctx, `SELECT lifecycle FROM claims WHERE id = $1 AND model_id = $2 FOR UPDATE`, in.Supersedes, modelID).Scan(&lifecycle)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				fs = append(fs, Finding{Field: "supersedes", Code: "unknown_claim", Message: "superseded claim does not exist in this model"})
			case err != nil:
				return err
			case lifecycle == "RETRACTED" || lifecycle == "SUPERSEDED":
				fs = append(fs, Finding{Field: "supersedes", Code: "claim_final", Message: "claim is already " + lifecycle})
			}
		}
		if len(fs) > 0 {
			return &ValidationError{Findings: fs}
		}

		var revalidateArg any
		if revalidate != nil {
			revalidateArg = fmt.Sprintf("%d seconds", int64(revalidate.Seconds()))
		}
		out, err = scanClaim(tx.QueryRow(ctx, `
			INSERT INTO claims (tenant_id, model_id, statement, claim_class, reasoning, confidence_band, confidence_score,
			                    subject_node_ids, graph_version, expires_at, revalidate_after, created_by_type, created_by, run_id, agent_task_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::uuid[], $9, $10, $11::interval, $12, $13, $14, $15) RETURNING `+claimCols,
			tenantID, modelID, in.Statement, in.Class, in.Reasoning, in.ConfidenceBand, in.ConfidenceScore, in.SubjectNodeIDs,
			current, expires, revalidateArg, a.Type, a.ID, nullable(a.RunID), nullable(a.AgentTaskID)))
		if err != nil {
			return err
		}
		if err := insertLinks(ctx, tx, tenantID, modelID, out.ID, in.Evidence, in.AssumptionIDs, a.ID); err != nil {
			return err
		}
		if in.Supersedes != "" {
			if _, err := tx.Exec(ctx, `UPDATE claims SET lifecycle = 'SUPERSEDED', superseded_by = $2, stale_reason = NULL, stale_since = NULL
				WHERE id = $1`, in.Supersedes, out.ID); err != nil {
				return err
			}
		}
		if err := s.emit(ctx, tx, tenantID, projectID, a.RunID, EventClaimCreated, map[string]any{
			"model_id": modelID, "claim_id": out.ID, "claim_class": out.Class, "confidence_band": out.ConfidenceBand,
			"subject_node_ids": out.SubjectNodeIDs, "supersedes": nullable(in.Supersedes),
		}); err != nil {
			return err
		}
		return loadLinks(ctx, tx, &out)
	})
	if pe, ok := isCheck(err); ok {
		return Claim{}, &ValidationError{Findings: []Finding{{Code: "ledger_rule", Message: pe.Message, Invariant: "claim_evidence_requirements"}}}
	}
	return out, err
}

func insertLinks(ctx context.Context, tx pgx.Tx, tenantID, modelID, claimID string, links []EvidenceLink, assumptionIDs []string, by string) error {
	for _, l := range links {
		if _, err := tx.Exec(ctx, `INSERT INTO claim_evidence (tenant_id, model_id, claim_id, evidence_id, relation, note, linked_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (claim_id, evidence_id) DO NOTHING`,
			tenantID, modelID, claimID, l.EvidenceID, l.Relation, l.Note, by); err != nil {
			return err
		}
	}
	for _, id := range assumptionIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO claim_assumptions (tenant_id, model_id, claim_id, assumption_id)
			VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, tenantID, modelID, claimID, id); err != nil {
			return err
		}
	}
	return nil
}

func loadLinks(ctx context.Context, tx pgx.Tx, c *Claim) error {
	rows, err := tx.Query(ctx, `SELECT ev.*, ce.relation, ce.note, ce.linked_at
		FROM claim_evidence ce CROSS JOIN LATERAL (SELECT `+evidenceCols+` FROM evidence_records WHERE id = ce.evidence_id) ev
		WHERE ce.claim_id = $1 ORDER BY ce.linked_at, ce.evidence_id`, c.ID)
	if err != nil {
		return err
	}
	c.Evidence, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (LinkedEvidence, error) {
		var le LinkedEvidence
		e := &le.Evidence
		err := r.Scan(&e.ID, &e.ModelID, &e.SourceRef, &e.SourceType, &e.Directness, &e.ContentHash, &e.Excerpt, &e.CapturedAt,
			&e.TrustLevel, &e.AccessPolicy, &e.ExpiresAt, &e.Status, &e.StatusReason, &e.StatusChangedAt, &e.CreatedByType,
			&e.CreatedBy, &e.CreatedAt, &le.Relation, &le.Note, &le.LinkedAt)
		e.Ref = "evidence:" + e.ID
		return le, err
	})
	if err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT a.* FROM claim_assumptions ca
		CROSS JOIN LATERAL (SELECT `+assumptionCols+` FROM assumptions WHERE id = ca.assumption_id) a
		WHERE ca.claim_id = $1 ORDER BY ca.linked_at, ca.assumption_id`, c.ID)
	if err != nil {
		return err
	}
	c.Assumptions, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Assumption, error) { return scanAssumption(r) })
	if c.Evidence == nil {
		c.Evidence = []LinkedEvidence{}
	}
	if c.Assumptions == nil {
		c.Assumptions = []Assumption{}
	}
	return err
}

func (s *Store) GetClaim(ctx context.Context, tenantID, modelID, id string) (Claim, error) {
	if !ids.Valid(id) || !ids.Valid(modelID) {
		return Claim{}, ErrNotFound
	}
	var c Claim
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var err error
		c, err = scanClaim(tx.QueryRow(ctx, `SELECT `+claimCols+` FROM claims WHERE id = $1 AND model_id = $2`, id, modelID))
		if err != nil {
			return err
		}
		return loadLinks(ctx, tx, &c)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Claim{}, ErrNotFound
	}
	return c, err
}

// ClaimFilter narrows ListClaims.
type ClaimFilter struct {
	Lifecycle string
	Class     string
	NodeID    string
	Limit     int
}

func (s *Store) ListClaims(ctx context.Context, tenantID, modelID string, f ClaimFilter) ([]Claim, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 200
	}
	if f.NodeID != "" && !ids.Valid(f.NodeID) {
		return nil, fmt.Errorf("%w: node_id must be a UUID", ErrInvalidInput)
	}
	out := []Claim{}
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if _, _, err := modelProject(ctx, tx, modelID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+claimCols+` FROM claims
			WHERE model_id = $1 AND ($2 = '' OR lifecycle = $2) AND ($3 = '' OR claim_class = $3)
			  AND ($4 = '' OR subject_node_ids @> ARRAY[NULLIF($4, '')::uuid])
			ORDER BY created_at DESC, id LIMIT $5`, modelID, f.Lifecycle, f.Class, f.NodeID, f.Limit)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Claim, error) { return scanClaim(r) })
		out = append(out, list...)
		return err
	})
	return out, err
}

// ReviewClaim records an independent review (the author cannot review their own claim).
func (s *Store) ReviewClaim(ctx context.Context, tenantID, modelID, id string, in ReviewInput, a Actor) (Claim, error) {
	if !slices.Contains(ReviewDecisions, in.Decision) {
		return Claim{}, &ValidationError{Findings: []Finding{{Field: "decision", Code: "invalid_decision", Message: fmt.Sprintf("decision must be one of %v", ReviewDecisions)}}}
	}
	if strings.TrimSpace(in.Note) == "" {
		return Claim{}, &ValidationError{Findings: []Finding{{Field: "note", Code: "missing_note", Message: "a review note is required"}}}
	}
	return s.updateClaim(ctx, tenantID, modelID, id, a, func(ctx context.Context, tx pgx.Tx, c Claim, projectID string) (Claim, error) {
		if c.CreatedBy == a.ID {
			return Claim{}, ErrReviewerIsOwner
		}
		if c.Lifecycle != "ACTIVE" && c.Lifecycle != "STALE" {
			return Claim{}, fmt.Errorf("%w: a %s claim cannot be reviewed", ErrInvalidState, c.Lifecycle)
		}
		out, err := scanClaim(tx.QueryRow(ctx, `UPDATE claims SET review_status = $2, reviewer = $3, review_note = $4, reviewed_at = NOW()
			WHERE id = $1 RETURNING `+claimCols, c.ID, in.Decision, a.ID, in.Note))
		if err != nil {
			return Claim{}, err
		}
		return out, s.emit(ctx, tx, tenantID, projectID, a.RunID, EventClaimReviewed, map[string]any{
			"model_id": modelID, "claim_id": c.ID, "decision": in.Decision, "reviewer": a.ID,
		})
	})
}

// RevalidateClaim re-establishes a claim with (optionally) fresh evidence and a new
// expiry. It succeeds only if the claim then satisfies every ledger rule.
func (s *Store) RevalidateClaim(ctx context.Context, tenantID, modelID, id string, in RevalidateInput, a Actor) (Claim, error) {
	return s.updateClaim(ctx, tenantID, modelID, id, a, func(ctx context.Context, tx pgx.Tx, c Claim, projectID string) (Claim, error) {
		if c.Lifecycle != "ACTIVE" && c.Lifecycle != "STALE" {
			return Claim{}, fmt.Errorf("%w: a %s claim cannot be revalidated", ErrInvalidState, c.Lifecycle)
		}
		now := s.now().UTC()
		var expires *time.Time
		switch {
		case in.ExpiresAt != nil:
			if !in.ExpiresAt.After(now) {
				return Claim{}, &ValidationError{Findings: []Finding{{Field: "expires_at", Code: "invalid_expiry", Message: "expires_at must be in the future"}}}
			}
			expires = in.ExpiresAt
		case c.RevalidateAfter != "":
			var d pgtype.Interval
			if err := tx.QueryRow(ctx, `SELECT revalidate_after FROM claims WHERE id = $1`, c.ID).Scan(&d); err != nil {
				return Claim{}, err
			}
			t := now.Add(time.Duration(d.Microseconds)*time.Microsecond + time.Duration(d.Days)*24*time.Hour + time.Duration(d.Months)*30*24*time.Hour)
			expires = &t
		default:
			expires = nil
			if c.ExpiresAt != nil && c.ExpiresAt.After(now) {
				expires = c.ExpiresAt
			}
		}
		fs, err := linkFindings(ctx, tx, modelID, c.Class, c.ConfidenceBand, in.Evidence, nil)
		if err != nil {
			return Claim{}, err
		}
		// Report problems with the new links here; overall validity is decided by the
		// shared database rule after the update (old links may legitimately have expired).
		var linkErrors []Finding
		for _, f := range fs {
			if strings.HasPrefix(f.Field, "evidence[") {
				linkErrors = append(linkErrors, f)
			}
		}
		if len(linkErrors) > 0 {
			return Claim{}, &ValidationError{Findings: linkErrors}
		}
		if err := insertLinks(ctx, tx, tenantID, modelID, c.ID, in.Evidence, nil, a.ID); err != nil {
			return Claim{}, err
		}
		out, err := scanClaim(tx.QueryRow(ctx, `
			UPDATE claims SET lifecycle = 'ACTIVE', stale_reason = NULL, stale_since = NULL, last_validated_at = NOW(), expires_at = $2
			WHERE id = $1 RETURNING `+claimCols, c.ID, expires))
		if err != nil {
			return Claim{}, err
		}
		var unmet *string
		if err := tx.QueryRow(ctx, `SELECT claim_requirements_unmet(c) FROM claims c WHERE id = $1`, c.ID).Scan(&unmet); err != nil {
			return Claim{}, err
		}
		if unmet != nil {
			return Claim{}, &ValidationError{Findings: []Finding{{Code: "requirements_unmet", Invariant: "claim_evidence_requirements", Message: *unmet}}}
		}
		if err := s.emit(ctx, tx, tenantID, projectID, a.RunID, EventClaimRevalidated, map[string]any{
			"model_id": modelID, "claim_id": c.ID, "expires_at": expires,
		}); err != nil {
			return Claim{}, err
		}
		return out, nil
	})
}

// RetractClaim withdraws a claim permanently.
func (s *Store) RetractClaim(ctx context.Context, tenantID, modelID, id, reason string, a Actor) (Claim, error) {
	if strings.TrimSpace(reason) == "" {
		return Claim{}, &ValidationError{Findings: []Finding{{Field: "reason", Code: "missing_reason", Message: "a reason is required"}}}
	}
	return s.updateClaim(ctx, tenantID, modelID, id, a, func(ctx context.Context, tx pgx.Tx, c Claim, projectID string) (Claim, error) {
		out, err := scanClaim(tx.QueryRow(ctx, `UPDATE claims SET lifecycle = 'RETRACTED', retracted_reason = $2, stale_reason = NULL, stale_since = NULL
			WHERE id = $1 RETURNING `+claimCols, c.ID, reason))
		if err != nil {
			return Claim{}, err
		}
		return out, s.emit(ctx, tx, tenantID, projectID, a.RunID, EventClaimRetracted, map[string]any{"model_id": modelID, "claim_id": c.ID, "reason": reason})
	})
}

// updateClaim locks a claim and applies fn; final-state claims are rejected.
func (s *Store) updateClaim(ctx context.Context, tenantID, modelID, id string, a Actor,
	fn func(context.Context, pgx.Tx, Claim, string) (Claim, error)) (Claim, error) {
	if err := validActor(a); err != nil {
		return Claim{}, err
	}
	if !ids.Valid(id) {
		return Claim{}, ErrNotFound
	}
	var out Claim
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		projectID, _, err := modelProject(ctx, tx, modelID)
		if err != nil {
			return err
		}
		c, err := scanClaim(tx.QueryRow(ctx, `SELECT `+claimCols+` FROM claims WHERE id = $1 AND model_id = $2 FOR UPDATE`, id, modelID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if c.Lifecycle == "RETRACTED" || c.Lifecycle == "SUPERSEDED" {
			return fmt.Errorf("%w: claim is %s", ErrInvalidState, c.Lifecycle)
		}
		if err := loadLinks(ctx, tx, &c); err != nil {
			return err
		}
		if out, err = fn(ctx, tx, c, projectID); err != nil {
			return err
		}
		return loadLinks(ctx, tx, &out)
	})
	if pe, ok := isCheck(err); ok {
		return Claim{}, &ValidationError{Findings: []Finding{{Code: "ledger_rule", Message: pe.Message}}}
	}
	return out, err
}

// ---------------------------------------------------------------------------
// 4.4 Revalidation sweep (run periodically by cmd/scheduler)
// ---------------------------------------------------------------------------

// Sweep expires evidence and assumptions whose time has passed and marks every ACTIVE
// claim that no longer satisfies the ledger rules (its own expiry, expired/revoked
// evidence, unusable assumptions) as STALE, across all tenants.
func (s *Store) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE evidence_records ev SET status = 'EXPIRED', status_reason = 'expired', status_changed_at = NOW()
			WHERE ev.status = 'ACTIVE' AND ev.expires_at IS NOT NULL AND ev.expires_at <= NOW()
			RETURNING ev.id::text, ev.tenant_id::text, ev.model_id::text,
			          (SELECT project_id::text FROM architecture_models m WHERE m.id = ev.model_id)`)
		if err != nil {
			return err
		}
		type rec struct{ id, tenant, model, project string }
		expired, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (rec, error) {
			var x rec
			return x, r.Scan(&x.id, &x.tenant, &x.model, &x.project)
		})
		if err != nil {
			return err
		}
		for _, x := range expired {
			// Surface any graph nodes whose accepted GraphPatch operations cited this expired evidence.
			var citedNodes []string
			nRows, nErr := tx.Query(ctx, `
				SELECT DISTINCT op->>'target_id'
				FROM graph_patches gp,
				     jsonb_array_elements(gp.operations) AS op
				WHERE gp.model_id = $1
				  AND gp.tenant_id = $2
				  AND gp.status = 'ACCEPTED'
				  AND op->'evidence_refs' ? $3`, x.model, x.tenant, "evidence:"+x.id)
			if nErr == nil {
				for nRows.Next() {
					var nid string
					if err := nRows.Scan(&nid); err == nil && nid != "" {
						citedNodes = append(citedNodes, nid)
					}
				}
				nRows.Close()
			}
			payload := map[string]any{"model_id": x.model, "evidence_id": x.id}
			if len(citedNodes) > 0 {
				payload["affected_node_ids"] = citedNodes
			}
			if err := s.emit(ctx, tx, x.tenant, x.project, "", EventEvidenceExpired, payload); err != nil {
				return err
			}
		}
		res.EvidenceExpired = len(expired)

		rows, err = tx.Query(ctx, `
			UPDATE assumptions a SET status = 'NEEDS_REVALIDATION', status_note = 'expired', status_changed_by = 'system:revalidation',
			       status_changed_at = NOW()
			WHERE a.status IN ('ACTIVE', 'VALIDATED') AND a.expires_at IS NOT NULL AND a.expires_at <= NOW()
			RETURNING a.id::text, a.tenant_id::text, a.model_id::text,
			          (SELECT project_id::text FROM architecture_models m WHERE m.id = a.model_id)`)
		if err != nil {
			return err
		}
		due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (rec, error) {
			var x rec
			return x, r.Scan(&x.id, &x.tenant, &x.model, &x.project)
		})
		if err != nil {
			return err
		}
		for _, x := range due {
			if err := s.emit(ctx, tx, x.tenant, x.project, "", EventAssumptionStatus, map[string]any{
				"model_id": x.model, "assumption_id": x.id, "to": "NEEDS_REVALIDATION", "reason": "expired",
			}); err != nil {
				return err
			}
		}
		res.AssumptionsNeedingReview = len(due)

		rows, err = tx.Query(ctx, `
			UPDATE claims c SET lifecycle = 'STALE', stale_reason = claim_requirements_unmet(c), stale_since = NOW()
			WHERE c.lifecycle = 'ACTIVE' AND claim_requirements_unmet(c) IS NOT NULL
			RETURNING c.id::text, c.tenant_id::text, c.model_id::text,
			          (SELECT project_id::text FROM architecture_models m WHERE m.id = c.model_id), c.stale_reason`)
		if err != nil {
			return err
		}
		type st struct{ id, tenant, model, project, reason string }
		stale, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (st, error) {
			var x st
			return x, r.Scan(&x.id, &x.tenant, &x.model, &x.project, &x.reason)
		})
		if err != nil {
			return err
		}
		for _, x := range stale {
			if err := s.emit(ctx, tx, x.tenant, x.project, "", EventClaimStale, map[string]any{
				"model_id": x.model, "claim_id": x.id, "reason": x.reason,
			}); err != nil {
				return err
			}
		}
		res.ClaimsMarkedStale = len(stale)
		return nil
	})
	return res, err
}
