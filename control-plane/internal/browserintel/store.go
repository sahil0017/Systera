package browserintel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/evidence"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/projects"
	"github.com/systera/control-plane/internal/tenancy"
)

// Store manages browser investigation sessions, re-sanitized observations and website
// profiles. Sessions are personal: only their owner may add to, read, analyze, promote or
// delete them; tenant OWNER/ADMIN may additionally read and delete (data governance).
type Store struct {
	pool     *pgxpool.Pool
	projects projects.Repository
	graphSvc *graph.Service
	ledger   *evidence.Store
	runs     RunCreator
	plans    PlanSubmitter
	now      func() time.Time
}

func NewStore(pool *pgxpool.Pool, projRepo projects.Repository, graphSvc *graph.Service) *Store {
	return &Store{pool: pool, projects: projRepo, graphSvc: graphSvc, now: time.Now}
}

// Actor is who is acting on a session.
type Actor struct {
	UserID string
	Role   tenancy.Role
}

func (a Actor) admin() bool { return a.Role == tenancy.RoleOwner || a.Role == tenancy.RoleAdmin }

// ActorFrom builds an Actor from a verified principal.
func ActorFrom(p tenancy.Principal) Actor { return Actor{UserID: p.UserID, Role: p.Role} }

func (s *Store) tx(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.AppRole, fn)
}

// CreateSession opens a consent-recorded session. Consent must be explicit and for the
// current consent text; selected pages (multi-page mode) must be on the domain.
func (s *Store) CreateSession(ctx context.Context, tenantID string, a Actor, in CreateSessionInput) (Session, error) {
	if !in.ConsentConfirmed {
		return Session{}, ErrConsentRequired
	}
	if in.ConsentVersion != ConsentVersion {
		return Session{}, fmt.Errorf("%w: consent_version must be %q (the consent text shown to the user)", ErrConsentRequired, ConsentVersion)
	}
	domain, err := ValidateDomain(in.Domain)
	if err != nil {
		return Session{}, err
	}
	if in.AnalysisMode == "" {
		in.AnalysisMode = ModeQuickScan
	}
	if !in.AnalysisMode.valid() {
		return Session{}, fmt.Errorf("%w: unknown analysis_mode %q", ErrInvalidInput, in.AnalysisMode)
	}
	if in.MaxPages <= 0 {
		in.MaxPages = 10
	}
	if in.MaxPages > MaxPagesPerSession {
		return Session{}, fmt.Errorf("%w: max_pages is at most %d", ErrInvalidInput, MaxPagesPerSession)
	}
	if in.RetentionDays <= 0 {
		in.RetentionDays = DefaultRetention
	}
	if in.RetentionDays > MaxRetentionDays {
		return Session{}, fmt.Errorf("%w: retention_days is at most %d", ErrInvalidInput, MaxRetentionDays)
	}
	pages := []string{}
	for _, p := range in.AllowedPages {
		np, err := NormalizePage(p)
		if err != nil {
			return Session{}, err
		}
		if err := CheckPage(Session{Domain: domain}, np); err != nil {
			return Session{}, err
		}
		pages = append(pages, np)
	}
	pages = dedupeStrings(pages)
	if len(pages) > in.MaxPages {
		return Session{}, fmt.Errorf("%w: %d pages selected, max_pages is %d", ErrPageLimitReached, len(pages), in.MaxPages)
	}

	now := s.now().UTC()
	out := Session{ID: ids.New(), TenantID: tenantID, UserID: a.UserID, Domain: domain, AnalysisMode: in.AnalysisMode,
		AllowedPages: pages, MaxPages: in.MaxPages, ConsentConfirmed: true, ConsentVersion: ConsentVersion, ConsentedAt: now,
		RetentionExpiresAt: now.AddDate(0, 0, in.RetentionDays), Status: "ACTIVE"}
	err = s.tx(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO browser_sessions (id, tenant_id, user_id, domain, analysis_mode, allowed_pages, max_pages,
				consent_confirmed, consent_version, consented_at, retention_expires_at, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, true, $8, $9, $10, 'ACTIVE')
			RETURNING created_at, updated_at`,
			out.ID, tenantID, a.UserID, domain, string(in.AnalysisMode), pages, in.MaxPages, ConsentVersion, now, out.RetentionExpiresAt,
		).Scan(&out.CreatedAt, &out.UpdatedAt); err != nil {
			return err
		}
		return postgres.InsertAudit(ctx, tx, postgres.AuditRecord{TenantID: tenantID, ActorID: a.UserID, ActorType: "USER",
			Action: "browser.session.consented", ResourceType: "browser_session", ResourceID: out.ID, CorrelationID: ids.New(),
			Metadata: map[string]any{"consent_version": ConsentVersion, "domain": domain, "mode": in.AnalysisMode, "pages": len(pages)}})
	})
	return out, err
}

const sessionCols = `id::text, tenant_id::text, user_id::text, domain, analysis_mode, allowed_pages, max_pages, consent_confirmed,
	consent_version, consented_at, retention_expires_at, status, created_at, updated_at,
	(SELECT count(*) FROM browser_observations o WHERE o.session_id = browser_sessions.id)`

func scanSession(row pgx.Row) (Session, error) {
	var out Session
	var mode string
	err := row.Scan(&out.ID, &out.TenantID, &out.UserID, &out.Domain, &mode, &out.AllowedPages, &out.MaxPages, &out.ConsentConfirmed,
		&out.ConsentVersion, &out.ConsentedAt, &out.RetentionExpiresAt, &out.Status, &out.CreatedAt, &out.UpdatedAt, &out.ObservationCount)
	out.AnalysisMode = AnalysisMode(mode)
	return out, err
}

// sessionTx loads a session the actor may act on. Others' sessions look like they do not
// exist. readOnly allows tenant admins.
func sessionTx(ctx context.Context, tx pgx.Tx, id string, a Actor, readOnly, lock bool) (Session, error) {
	if !ids.Valid(id) {
		return Session{}, ErrNotFound
	}
	q := `SELECT ` + sessionCols + ` FROM browser_sessions WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	sess, err := scanSession(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	if sess.UserID != a.UserID && !(readOnly && a.admin()) {
		return Session{}, ErrNotFound
	}
	return sess, nil
}

// GetSession returns a session (owner, or tenant admin).
func (s *Store) GetSession(ctx context.Context, tenantID string, a Actor, sessionID string) (Session, error) {
	var out Session
	err := s.tx(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = sessionTx(ctx, tx, sessionID, a, true, false)
		return err
	})
	return out, err
}

// ListSessions returns the actor's own sessions, newest first.
func (s *Store) ListSessions(ctx context.Context, tenantID string, a Actor) ([]Session, error) {
	out := []Session{}
	err := s.tx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+sessionCols+` FROM browser_sessions WHERE user_id = $1 ORDER BY created_at DESC LIMIT 100`, a.UserID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			sess, err := scanSession(rows)
			if err != nil {
				return err
			}
			out = append(out, sess)
		}
		return rows.Err()
	})
	return out, err
}

func audit(ctx context.Context, tx pgx.Tx, tenantID, actor, action, resType, resID string, meta map[string]any) error {
	actorType := "USER"
	if actor == "" {
		actorType, actor = "SYSTEM", "00000000-0000-0000-0000-000000000000" // audit_events.actor_id is NOT NULL
	}
	return postgres.InsertAudit(ctx, tx, postgres.AuditRecord{TenantID: tenantID, ActorID: actor, ActorType: actorType, Action: action,
		ResourceType: resType, ResourceID: resID, CorrelationID: ids.New(), Metadata: meta})
}

// DeleteSession deletes the session; observations and derived profiles go with it (FK
// cascade). Projects already promoted into the workspace are the user's own work and stay.
func (s *Store) DeleteSession(ctx context.Context, tenantID string, a Actor, sessionID string) (DeletionReport, error) {
	rep := DeletionReport{SessionID: sessionID, Status: "DELETED"}
	var refs []ledgerRef
	err := s.tx(ctx, tenantID, func(tx pgx.Tx) error {
		sess, err := sessionTx(ctx, tx, sessionID, a, true, true)
		if err != nil {
			return err
		}
		if refs, err = ledgerRefsTx(ctx, tx, "session_id = $1", sess.ID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM browser_observations WHERE session_id = $1),
			(SELECT count(*) FROM website_profiles WHERE session_id = $1)`, sess.ID).Scan(&rep.Observations, &rep.Profiles); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM browser_sessions WHERE id = $1`, sess.ID); err != nil {
			return err
		}
		if err := audit(ctx, tx, tenantID, a.UserID, "browser.session.deleted", "browser_session", sess.ID,
			map[string]any{"observations": rep.Observations, "profiles": rep.Profiles, "by_owner": sess.UserID == a.UserID}); err != nil {
			return err
		}
		_, err = outbox.InsertTx(ctx, tx, outbox.EventEnvelope{EventType: "browser.session.deleted", TenantID: tenantID,
			CorrelationID: ids.New(), Payload: map[string]any{"session_id": sess.ID}})
		return err
	})
	if err != nil {
		return rep, err
	}
	rep.LedgerEvidenceRevoked = len(refs)
	return rep, s.revokeLedger(ctx, refs, evidence.Actor{Type: "USER", ID: a.UserID})
}

// DeleteObservation deletes one captured page and every profile derived from it; the
// session returns to ACTIVE so the user can re-analyze what remains.
func (s *Store) DeleteObservation(ctx context.Context, tenantID string, a Actor, observationID string) (DeletionReport, error) {
	if !ids.Valid(observationID) {
		return DeletionReport{}, ErrNotFound
	}
	rep := DeletionReport{Status: "DELETED"}
	var refs []ledgerRef
	err := s.tx(ctx, tenantID, func(tx pgx.Tx) error {
		var sid string
		if err := tx.QueryRow(ctx, `SELECT session_id::text FROM browser_observations WHERE id = $1`, observationID).Scan(&sid); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if _, err := sessionTx(ctx, tx, sid, a, true, true); err != nil {
			return err
		}
		rep.SessionID = sid
		var err error
		if refs, err = ledgerRefsTx(ctx, tx, "observation_id = $1", observationID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM website_profiles WHERE session_id = $1 AND $2::uuid = ANY(source_observation_ids)`, sid, observationID)
		if err != nil {
			return err
		}
		rep.Profiles = int(tag.RowsAffected())
		if _, err := tx.Exec(ctx, `DELETE FROM browser_observations WHERE id = $1`, observationID); err != nil {
			return err
		}
		rep.Observations = 1
		if _, err := tx.Exec(ctx, `UPDATE browser_sessions SET status = 'ACTIVE', updated_at = NOW() WHERE id = $1 AND status = 'ANALYZED'`, sid); err != nil {
			return err
		}
		return audit(ctx, tx, tenantID, a.UserID, "browser.observation.deleted", "browser_observation", observationID,
			map[string]any{"session_id": sid, "profiles": rep.Profiles})
	})
	if err != nil {
		return rep, err
	}
	rep.LedgerEvidenceRevoked = len(refs)
	return rep, s.revokeLedger(ctx, refs, evidence.Actor{Type: "USER", ID: a.UserID})
}

// SweepExpired deletes every session past its retention (system role; cascades).
func (s *Store) SweepExpired(ctx context.Context) (int, error) {
	n := 0
	var refs []ledgerRef
	err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if refs, err = ledgerRefsTx(ctx, tx, "session_id IN (SELECT id FROM browser_sessions WHERE retention_expires_at <= $1)", s.now().UTC()); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `DELETE FROM browser_sessions WHERE retention_expires_at <= $1 RETURNING id::text, tenant_id::text`, s.now().UTC())
		if err != nil {
			return err
		}
		type gone struct{ id, tenant string }
		var gs []gone
		for rows.Next() {
			var g gone
			if err := rows.Scan(&g.id, &g.tenant); err != nil {
				rows.Close()
				return err
			}
			gs = append(gs, g)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, g := range gs {
			if err := audit(ctx, tx, g.tenant, "", "browser.session.retention_expired", "browser_session", g.id, nil); err != nil {
				return err
			}
		}
		n = len(gs)
		return nil
	})
	if err != nil {
		return n, err
	}
	return n, s.revokeLedger(ctx, refs, evidence.Actor{Type: "SYSTEM", ID: "retention-sweep"})
}

// RecordObservation re-sanitizes and stores one page snapshot from the session owner.
func (s *Store) RecordObservation(ctx context.Context, tenantID string, a Actor, in ObservationInput) (Observation, error) {
	if !ids.Valid(in.SessionID) {
		return Observation{}, ErrNotFound
	}
	if raw, _ := json.Marshal(in); len(raw) > MaxObservationBytes {
		return Observation{}, ErrPayloadTooLarge
	}
	redactions, injectionSuspected, err := SanitizeObservation(&in)
	if err != nil {
		return Observation{}, err
	}
	obsJSON, err := json.Marshal(in.Observations)
	if err != nil {
		return Observation{}, err
	}
	if len(obsJSON) > MaxObservationBytes {
		return Observation{}, ErrPayloadTooLarge
	}

	out := Observation{ID: ids.New(), TenantID: tenantID, SessionID: in.SessionID, URL: in.URL, Title: in.Title,
		CollectorVersion: in.CollectorVersion, SchemaVersion: in.SchemaVersion, Observations: in.Observations,
		RedactionsApplied: redactions, InjectionSuspected: injectionSuspected, Untrusted: true, PayloadBytes: len(obsJSON)}
	err = s.tx(ctx, tenantID, func(tx pgx.Tx) error {
		sess, err := sessionTx(ctx, tx, in.SessionID, a, false, true)
		if err != nil {
			return err
		}
		// A promoted investigation still grows (re-analysis keeps the workspace model and ledger
		// in sync); selected pages, the page limit and retention still bound it.
		if sess.Status != "ACTIVE" && sess.Status != "ANALYZED" && sess.Status != "PROMOTED" {
			return fmt.Errorf("%w: status %s", ErrSessionClosed, sess.Status)
		}
		if !sess.RetentionExpiresAt.After(s.now()) {
			return fmt.Errorf("%w: retention expired", ErrSessionClosed)
		}
		if err := CheckPage(sess, in.URL); err != nil {
			return err
		}
		if sess.ObservationCount >= sess.MaxPages {
			return ErrPageLimitReached
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO browser_observations (id, tenant_id, session_id, url, title, collector_version, schema_version,
				observations_json, redactions_applied, injection_suspected, payload_bytes)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			RETURNING captured_at`,
			out.ID, tenantID, in.SessionID, in.URL, in.Title, in.CollectorVersion, in.SchemaVersion,
			obsJSON, redactions, injectionSuspected, len(obsJSON)).Scan(&out.CapturedAt); err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "23505" {
				return ErrDuplicatePage
			}
			return err
		}
		_, err = outbox.InsertTx(ctx, tx, outbox.EventEnvelope{EventType: "browser.observation.received", TenantID: tenantID,
			CorrelationID: ids.New(), Payload: map[string]any{"session_id": in.SessionID, "observation_id": out.ID, "injection_suspected": injectionSuspected}})
		return err
	})
	return out, err
}

func scanObservation(row pgx.Row) (Observation, error) {
	var o Observation
	var raw []byte
	err := row.Scan(&o.ID, &o.TenantID, &o.SessionID, &o.URL, &o.Title, &o.CollectorVersion, &o.SchemaVersion, &raw,
		&o.RedactionsApplied, &o.InjectionSuspected, &o.PayloadBytes, &o.CapturedAt)
	if err != nil {
		return Observation{}, err
	}
	o.Untrusted = true
	_ = json.Unmarshal(raw, &o.Observations)
	return o, nil
}

const obsCols = `id::text, tenant_id::text, session_id::text, url, title, collector_version, schema_version, observations_json,
	redactions_applied, injection_suspected, payload_bytes, captured_at`

func observationsTx(ctx context.Context, tx pgx.Tx, sessionID string) ([]Observation, error) {
	rows, err := tx.Query(ctx, `SELECT `+obsCols+` FROM browser_observations WHERE session_id = $1 ORDER BY captured_at, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Observation{}
	for rows.Next() {
		o, err := scanObservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ListObservations shows exactly what is stored for a session (transparency).
func (s *Store) ListObservations(ctx context.Context, tenantID string, a Actor, sessionID string) ([]Observation, error) {
	var out []Observation
	err := s.tx(ctx, tenantID, func(tx pgx.Tx) error {
		sess, err := sessionTx(ctx, tx, sessionID, a, true, false)
		if err != nil {
			return err
		}
		out, err = observationsTx(ctx, tx, sess.ID)
		return err
	})
	return out, err
}

// AgentContext returns a session's observations framed as untrusted data for model prompts.
func (s *Store) AgentContext(ctx context.Context, tenantID string, a Actor, sessionID string) (string, error) {
	obs, err := s.ListObservations(ctx, tenantID, a, sessionID)
	if err != nil {
		return "", err
	}
	parts := make([]string, len(obs))
	for i, o := range obs {
		parts[i] = FrameForAgent(o)
	}
	return strings.Join(parts, "\n"), nil
}

// AnalyzeSession derives (or re-derives) the session's single website profile.
func (s *Store) AnalyzeSession(ctx context.Context, tenantID string, a Actor, in AnalyzeInput) (WebsiteProfile, error) {
	var out WebsiteProfile
	var staleClaims []string
	err := s.tx(ctx, tenantID, func(tx pgx.Tx) error {
		sess, err := sessionTx(ctx, tx, in.SessionID, a, false, true)
		if err != nil {
			return err
		}
		obs, err := observationsTx(ctx, tx, sess.ID)
		if err != nil {
			return err
		}
		if len(obs) == 0 {
			return ErrNoObservations
		}
		d := DeriveWebsiteProfile(sess.Domain, obs)
		capJSON, _ := json.Marshal(d.Capabilities)
		jrnJSON, _ := json.Marshal(d.Journeys)
		techJSON, _ := json.Marshal(d.TechSignals)
		fpJSON, _ := json.Marshal(d.Fingerprint)
		altJSON, _ := json.Marshal(d.Alternatives)
		var promoting bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM website_profiles WHERE session_id = $1 AND status = 'PROMOTING')`, sess.ID).Scan(&promoting); err != nil {
			return err
		}
		if promoting {
			return ErrPromotionInProgress
		}
		out, err = scanWebsite(tx.QueryRow(ctx, `
			INSERT INTO website_profiles (id, tenant_id, session_id, domain, capabilities_json, journeys_json, tech_signals_json,
				fingerprint_json, alternatives_json, source_observation_ids, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'ANALYZED')
			ON CONFLICT (session_id) DO UPDATE SET capabilities_json = EXCLUDED.capabilities_json, journeys_json = EXCLUDED.journeys_json,
				tech_signals_json = EXCLUDED.tech_signals_json, fingerprint_json = EXCLUDED.fingerprint_json,
				alternatives_json = EXCLUDED.alternatives_json, source_observation_ids = EXCLUDED.source_observation_ids,
				status = CASE WHEN website_profiles.status = 'PROMOTED' THEN 'PROMOTED' ELSE 'ANALYZED' END, updated_at = NOW()
			RETURNING `+websiteCols,
			ids.New(), tenantID, sess.ID, sess.Domain, capJSON, jrnJSON, techJSON, fpJSON, altJSON, d.SourceObservationIDs))
		if err != nil {
			return err
		}
		if staleClaims, err = replaceBaselineTx(ctx, tx, tenantID, out, obs); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE browser_sessions SET status = 'ANALYZED', updated_at = NOW() WHERE id = $1 AND status = 'ACTIVE'`, sess.ID); err != nil {
			return err
		}
		_, err = outbox.InsertTx(ctx, tx, outbox.EventEnvelope{EventType: "website.capabilities.detected", TenantID: tenantID,
			CorrelationID: ids.New(), Payload: map[string]any{"website_id": out.ID, "session_id": sess.ID, "domain": sess.Domain}})
		return err
	})
	if err != nil || out.Status != "PROMOTED" {
		return out, err
	}
	// Re-analysis of a promoted investigation: the replaced findings' claims are retracted and
	// the new findings become claims.
	user := evidence.Actor{Type: "USER", ID: a.UserID}
	if err := s.retractClaims(ctx, tenantID, out.ModelID, staleClaims, "superseded by re-analysis of the investigation", user); err != nil {
		return out, err
	}
	return out, s.syncLedger(ctx, tenantID, out.ID, user)
}

const websiteCols = `website_profiles.id::text, website_profiles.tenant_id::text, website_profiles.session_id::text, website_profiles.domain,
	COALESCE(website_profiles.project_id::text, ''), COALESCE(website_profiles.model_id::text, ''), COALESCE(website_profiles.run_id::text, ''),
	website_profiles.capabilities_json, website_profiles.journeys_json, website_profiles.tech_signals_json,
	website_profiles.fingerprint_json, website_profiles.alternatives_json, website_profiles.source_observation_ids::text[],
	website_profiles.status, website_profiles.promoted_at, website_profiles.created_at, website_profiles.updated_at`

func scanWebsite(row pgx.Row) (WebsiteProfile, error) {
	var w WebsiteProfile
	var capRaw, jrnRaw, techRaw, fpRaw, altRaw []byte
	if err := row.Scan(&w.ID, &w.TenantID, &w.SessionID, &w.Domain, &w.ProjectID, &w.ModelID, &w.RunID,
		&capRaw, &jrnRaw, &techRaw, &fpRaw, &altRaw, &w.SourceObservationIDs, &w.Status, &w.PromotedAt, &w.CreatedAt, &w.UpdatedAt); err != nil {
		return WebsiteProfile{}, err
	}
	_ = json.Unmarshal(capRaw, &w.Capabilities)
	_ = json.Unmarshal(jrnRaw, &w.Journeys)
	_ = json.Unmarshal(techRaw, &w.TechSignals)
	_ = json.Unmarshal(fpRaw, &w.Fingerprint)
	_ = json.Unmarshal(altRaw, &w.Alternatives)
	return w, nil
}

func websiteTx(ctx context.Context, tx pgx.Tx, id string, a Actor, readOnly, lock bool) (WebsiteProfile, error) {
	if !ids.Valid(id) {
		return WebsiteProfile{}, ErrNotFound
	}
	q := `SELECT ` + websiteCols + `, s.user_id::text FROM website_profiles JOIN browser_sessions s ON s.id = website_profiles.session_id WHERE website_profiles.id = $1`
	if lock {
		q += ` FOR UPDATE OF website_profiles`
	}
	var owner string
	var w WebsiteProfile
	var capRaw, jrnRaw, techRaw, fpRaw, altRaw []byte
	err := tx.QueryRow(ctx, q, id).Scan(&w.ID, &w.TenantID, &w.SessionID, &w.Domain, &w.ProjectID, &w.ModelID, &w.RunID,
		&capRaw, &jrnRaw, &techRaw, &fpRaw, &altRaw, &w.SourceObservationIDs, &w.Status, &w.PromotedAt, &w.CreatedAt, &w.UpdatedAt, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return WebsiteProfile{}, ErrNotFound
	}
	if err != nil {
		return WebsiteProfile{}, err
	}
	if owner != a.UserID && !(readOnly && a.admin()) {
		return WebsiteProfile{}, ErrNotFound
	}
	_ = json.Unmarshal(capRaw, &w.Capabilities)
	_ = json.Unmarshal(jrnRaw, &w.Journeys)
	_ = json.Unmarshal(techRaw, &w.TechSignals)
	_ = json.Unmarshal(fpRaw, &w.Fingerprint)
	_ = json.Unmarshal(altRaw, &w.Alternatives)
	return w, nil
}

// GetWebsite returns a profile (owner, or tenant admin).
func (s *Store) GetWebsite(ctx context.Context, tenantID string, a Actor, websiteID string) (WebsiteProfile, error) {
	var out WebsiteProfile
	err := s.tx(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = websiteTx(ctx, tx, websiteID, a, true, false)
		return err
	})
	return out, err
}

// ListWebsites lists the actor's profiles (all of the tenant's for OWNER/ADMIN).
func (s *Store) ListWebsites(ctx context.Context, tenantID string, a Actor) ([]WebsiteProfile, error) {
	out := []WebsiteProfile{}
	err := s.tx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+websiteCols+` FROM website_profiles JOIN browser_sessions s ON s.id = website_profiles.session_id
			WHERE $2 OR s.user_id = $1 ORDER BY website_profiles.created_at DESC LIMIT 200`, a.UserID, a.admin())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			w, err := scanWebsite(rows)
			if err != nil {
				return err
			}
			out = append(out, w)
		}
		return rows.Err()
	})
	return out, err
}

var nonKey = regexp.MustCompile(`[^a-z0-9]+`)

func nodeKey(prefix, name string) string {
	k := strings.Trim(nonKey.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(k) > 60 {
		k = strings.Trim(k[:60], "-")
	}
	return prefix + "." + k
}

// PromoteToWorkspace ("Continue in workspace", 3.10) creates a project and architecture
// model and seeds the model with the PROPOSED components behind the observed capabilities.
// It is idempotent: a promoted profile returns its existing project; a concurrent promotion
// is refused (the PROMOTING claim) instead of creating a second project.
func (s *Store) PromoteToWorkspace(ctx context.Context, tenantID string, a Actor, websiteID string) (WebsiteProfile, error) {
	if s.projects == nil || s.graphSvc == nil {
		return WebsiteProfile{}, errors.New("workspace promotion requires projects and graph services")
	}
	var w WebsiteProfile
	err := s.tx(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		if w, err = websiteTx(ctx, tx, websiteID, a, false, true); err != nil {
			return err
		}
		switch w.Status {
		case "PROMOTED":
			return nil
		case "PROMOTING":
			return ErrPromotionInProgress
		}
		_, err = tx.Exec(ctx, `UPDATE website_profiles SET status = 'PROMOTING', updated_at = NOW() WHERE id = $1`, websiteID)
		return err
	})
	if err != nil {
		return w, err
	}
	if w.Status == "PROMOTED" {
		return w, s.syncLedger(ctx, tenantID, w.ID, evidence.Actor{Type: "USER", ID: a.UserID}) // completes an interrupted sync
	}
	release := func() {
		_ = s.tx(context.WithoutCancel(ctx), tenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE website_profiles SET status = 'ANALYZED', updated_at = NOW() WHERE id = $1 AND status = 'PROMOTING'`, websiteID)
			return err
		})
	}

	proj, err := s.projects.Create(ctx, projects.Project{TenantID: tenantID, Name: "Website investigation: " + w.Domain,
		Description: fmt.Sprintf("Promoted from Engineering Lens investigation %s. Components are proposed hypotheses from public UI, not observed infrastructure.", w.SessionID),
		CreatedBy:   a.UserID})
	if err != nil {
		release()
		return WebsiteProfile{}, err
	}
	author := graph.Author{Type: "USER", ID: a.UserID}
	model, _, err := s.graphSvc.CreateModel(ctx, tenantID, proj.ID, author, ids.New())
	if err != nil {
		release()
		return WebsiteProfile{}, err
	}
	if err := s.seedModel(ctx, tenantID, model.ID, w, author); err != nil {
		release()
		return WebsiteProfile{}, fmt.Errorf("seed architecture model: %w", err)
	}

	now := s.now().UTC()
	err = s.tx(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE website_profiles SET project_id = $1, model_id = $2, status = 'PROMOTED', promoted_at = $3, updated_at = NOW()
			WHERE id = $4`, proj.ID, model.ID, now, websiteID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE browser_sessions SET status = 'PROMOTED', updated_at = NOW() WHERE id = $1`, w.SessionID); err != nil {
			return err
		}
		return audit(ctx, tx, tenantID, a.UserID, "website.promoted", "website_profile", websiteID,
			map[string]any{"project_id": proj.ID, "model_id": model.ID})
	})
	if err != nil {
		release()
		return WebsiteProfile{}, err
	}
	w.ProjectID, w.ModelID, w.Status, w.PromotedAt = proj.ID, model.ID, "PROMOTED", &now
	return w, s.syncLedger(ctx, tenantID, w.ID, evidence.Actor{Type: "USER", ID: a.UserID})
}

// seedModel adds a system node for the site and one PROPOSED service per proposed
// component, each citing the observations behind it.
func (s *Store) seedModel(ctx context.Context, tenantID, modelID string, w WebsiteProfile, author graph.Author) error {
	mk := func(key, typ, name, desc string, props map[string]any, rationale string, refs []string) graph.Operation {
		pl, _ := json.Marshal(graph.AddNodePayload{Key: key, Type: typ, Name: name, Description: desc, Properties: props, EvidenceState: "PROPOSED"})
		_ = refs // observation refs are kept in node properties; ledger evidence comes with the website agents (4.3)
		return graph.Operation{Op: graph.OpAddNode, TargetID: ids.New(), Payload: pl, Rationale: rationale}
	}
	ops := []graph.Operation{mk(nodeKey("system", w.Domain), "system", w.Domain, "Website under investigation (Engineering Lens).",
		map[string]any{"source": "engineering_lens", "website_id": w.ID}, "Root of the promoted investigation.", nil)}
	seen := map[string]bool{}
	for _, c := range w.Capabilities {
		if c.EvidenceClass != "PROPOSED" || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		ops = append(ops, mk(nodeKey("svc", c.Name), "service", c.Name, c.Rationale,
			map[string]any{"source": "engineering_lens", "evidence_class": "PROPOSED", "confidence": c.Confidence, "observations": c.EvidenceRefs},
			c.Rationale, c.EvidenceRefs))
	}
	base := 0
	res, err := s.graphSvc.ApplyPatch(ctx, tenantID, modelID, graph.Patch{BaseGraphVersion: &base, IdempotencyKey: "lens-promote:" + w.ID, Operations: ops}, author, ids.New())
	if err != nil {
		return err
	}
	if !res.Accepted {
		return fmt.Errorf("seed patch not accepted (status %s)", res.Status)
	}
	return nil
}
