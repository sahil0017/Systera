package test

// Stage 2 evidence ledger (checklist 4.1-4.4) integration tests. Require SYSTERA_TEST_DATABASE_URL.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/systera/control-plane/internal/auth"
	"github.com/systera/control-plane/internal/evidence"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/httpserver"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/projects"
	"github.com/systera/control-plane/internal/runs"
)

type ledgerEnv struct {
	client
	graphEnv
	ledger   *evidence.Store
	svc      *graph.Service
	token    string // author
	reviewer string // a second owner in the same tenant
	otherTok string
	modelID  string
	base     string
	nodeA    string // svc.redirect
	nodeB    string // db.links
}

func newLedgerEnv(t *testing.T) *ledgerEnv {
	t.Helper()
	ge := newGraphEnv(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := graph.NewService(ge.pool)
	ledger := evidence.NewStore(ge.pool)
	srv := httpserver.New(httpserver.Options{
		Logger:        logger,
		Projects:      projects.NewPostgresRepository(ge.pool),
		Runs:          runs.NewPostgresRepository(ge.pool),
		Events:        outbox.NewPostgresStore(ge.pool, logger),
		Graph:         svc,
		Evidence:      ledger,
		Authenticator: auth.NewAuthenticator(newVerifier(t), auth.NewPostgresMemberships(ge.pool)),
		Readiness:     []httpserver.ReadinessCheck{{Name: "postgres", Check: postgres.PingCheck(ge.pool)}},
	})
	sub := func(q string, args ...any) string {
		var s string
		if err := ge.pool.QueryRow(ctx, q, args...).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	authorSub := sub(`SELECT external_sub FROM users WHERE id = $1`, ge.userID)
	otherSub := sub(`SELECT u.external_sub FROM users u JOIN tenant_memberships m ON m.user_id = u.id WHERE m.tenant_id = $1`, ge.otherTenant)
	reviewerSub := "reviewer-" + ids.New()[:8]
	reviewerID := sub(`INSERT INTO users (email, display_name, external_sub) VALUES ($1, $1, $1) RETURNING id::text`, reviewerSub)
	if _, err := ge.pool.Exec(ctx, `INSERT INTO tenant_memberships (tenant_id, user_id, role) VALUES ($1, $2, 'ARCHITECT')`, ge.tenantID, reviewerID); err != nil {
		t.Fatal(err)
	}

	e := &ledgerEnv{client: client{t: t, handler: srv.Handler()}, graphEnv: *ge, ledger: ledger, svc: svc,
		token: mintToken(t, authorSub, "", time.Hour), reviewer: mintToken(t, reviewerSub, "", time.Hour),
		otherTok: mintToken(t, otherSub, "", time.Hour)}

	m, _, err := svc.CreateModel(ctx, ge.tenantID, ge.projectID, ge.author, "")
	if err != nil {
		t.Fatal(err)
	}
	e.modelID, e.base = m.ID, "/v1/architecture/"+m.ID
	e.nodeA, e.nodeB = ids.New(), ids.New()
	r := e.do(http.MethodPost, e.base+"/patches", e.token, patchBody(0, "seed",
		addNodeOp(e.nodeA, "svc.redirect", "service", "Redirect"), addNodeOp(e.nodeB, "db.links", "database", "Links"),
		addEdgeOp(ids.New(), e.nodeA, e.nodeB, "writes")), nil)
	expect(t, r, http.StatusCreated, "seed graph")
	return e
}

func (e *ledgerEnv) post(path, token string, body any) response {
	e.t.Helper()
	b, _ := json.Marshal(body)
	return e.do(http.MethodPost, e.base+path, token, string(b), nil)
}

func (e *ledgerEnv) capture(t *testing.T, ref, typ, trust, content string, extra map[string]any) evidence.Evidence {
	t.Helper()
	body := map[string]any{"source_ref": ref, "source_type": typ, "trust_level": trust, "content": content, "excerpt": content}
	for k, v := range extra {
		body[k] = v
	}
	r := e.post("/evidence", e.token, body)
	expect(t, r, http.StatusCreated, "capture "+ref)
	var ev evidence.Evidence
	r.decode(t, &ev)
	return ev
}

func ledgerCodes(t *testing.T, r response) string {
	t.Helper()
	var body struct{ Findings []evidence.Finding }
	r.decode(t, &body)
	var c []string
	for _, f := range body.Findings {
		c = append(c, f.Code)
	}
	return strings.Join(c, ",")
}

func link(id, rel string) map[string]any { return map[string]any{"evidence_id": id, "relation": rel} }

func TestEvidenceLedgerEndToEnd(t *testing.T) {
	e := newLedgerEnv(t)
	ctx := context.Background()

	// 4.2 Evidence capture: hashed, typed, trust-rated, idempotent per (source, content).
	page := e.capture(t, "https://example.com/pricing", "WEB_PAGE", "HIGH", "Plans: 10k links/month", nil)
	analysis := e.capture(t, "agent:requirements_analyst/run-1", "AGENT_ANALYSIS", "MEDIUM", "Read/write ratio likely 100:1", nil)
	note := e.capture(t, "interview:pm-2026-11-01", "USER_STATEMENT", "LOW", "We expect spikes on launches", nil)
	if page.Directness != "DIRECT" || analysis.Directness != "INDIRECT" || page.Ref != "evidence:"+page.ID || len(page.ContentHash) != 64 {
		t.Fatalf("evidence classification: %+v %+v", page, analysis)
	}
	r := e.post("/evidence", e.token, map[string]any{"source_ref": "https://example.com/pricing", "source_type": "WEB_PAGE", "trust_level": "HIGH", "content": "Plans: 10k links/month"})
	expect(t, r, http.StatusOK, "re-capture")
	var again evidence.Evidence
	r.decode(t, &again)
	if again.ID != page.ID || r.header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("re-capturing identical evidence must return the existing record")
	}
	r = e.post("/evidence", e.token, map[string]any{"source_ref": "", "source_type": "RUMOUR", "trust_level": "SUPER"})
	expect(t, r, http.StatusUnprocessableEntity, "bad evidence")
	if c := ledgerCodes(t, r); c != "invalid_source_ref,invalid_source_type,invalid_content_hash,invalid_trust_level" {
		t.Fatalf("evidence findings: %s", c)
	}
	past := time.Now().Add(-2 * time.Hour)
	stale := e.capture(t, "doc:old-sla", "SOURCE_DOCUMENT", "HIGH", "SLA 99.9", map[string]any{"captured_at": past, "expires_at": past.Add(time.Hour)})

	// 4.3 Assumptions are explicit, owned records.
	r = e.post("/assumptions", e.token, map[string]any{"statement": "Peak traffic is 5x average", "rationale": "Launch spikes reported by PM",
		"risk_if_wrong": "HIGH", "validation_method": "Load test before GA", "subject_node_ids": []string{e.nodeA}})
	expect(t, r, http.StatusCreated, "assumption")
	var assumption evidence.Assumption
	r.decode(t, &assumption)
	expect(t, e.post("/assumptions", e.token, map[string]any{"statement": "x", "risk_if_wrong": "HUGE"}), http.StatusUnprocessableEntity, "bad assumption")

	// 4.1 Claims and the Appendix D rules.
	claim := func(body map[string]any, code int, what string) (evidence.Claim, response) {
		t.Helper()
		r := e.post("/claims", e.token, body)
		expect(t, r, code, what)
		var c evidence.Claim
		if code == http.StatusCreated {
			r.decode(t, &c)
		}
		return c, r
	}
	_, r = claim(map[string]any{"statement": "Pricing caps links at 10k/month", "claim_class": "OBSERVED", "confidence_band": "MEDIUM",
		"evidence": []any{link(analysis.ID, "SUPPORTS")}}, 422, "observed on indirect evidence")
	if c := ledgerCodes(t, r); c != "missing_direct_evidence" {
		t.Fatalf("findings: %s", c)
	}
	_, r = claim(map[string]any{"statement": "x", "claim_class": "OBSERVED", "confidence_band": "LOW",
		"evidence": []any{link(page.ID, "SUPPORTS")}, "assumption_ids": []string{assumption.ID}}, 422, "observation on assumption")
	if c := ledgerCodes(t, r); c != "observation_on_assumption" {
		t.Fatalf("findings: %s", c)
	}
	_, r = claim(map[string]any{"statement": "x", "claim_class": "OBSERVED", "confidence_band": "LOW",
		"evidence": []any{link(stale.ID, "SUPPORTS")}}, 422, "expired evidence")
	if c := ledgerCodes(t, r); !strings.Contains(c, "evidence_not_usable") {
		t.Fatalf("findings: %s", c)
	}
	observed, _ := claim(map[string]any{"statement": "Pricing caps links at 10k/month", "claim_class": "OBSERVED", "confidence_band": "HIGH",
		"confidence_score": 0.9, "subject_node_ids": []string{e.nodeA}, "evidence": []any{link(page.ID, "SUPPORTS")}}, 201, "observed claim")
	if observed.GraphVersion != 1 || len(observed.Evidence) != 1 || observed.Lifecycle != "ACTIVE" || observed.ReviewStatus != "UNREVIEWED" {
		t.Fatalf("observed claim: %+v", observed)
	}

	_, r = claim(map[string]any{"statement": "Reads dominate", "claim_class": "INFERRED", "confidence_band": "MEDIUM"}, 422, "bare inference")
	if c := ledgerCodes(t, r); c != "missing_reasoning,missing_evidence" {
		t.Fatalf("findings: %s", c)
	}
	_, r = claim(map[string]any{"statement": "Reads dominate", "claim_class": "INFERRED", "confidence_band": "HIGH", "reasoning": "ratio",
		"evidence": []any{link(analysis.ID, "SUPPORTS")}}, 422, "uncalibrated HIGH")
	if c := ledgerCodes(t, r); c != "confidence_not_supported" {
		t.Fatalf("findings: %s", c)
	}
	_, r = claim(map[string]any{"statement": "x", "claim_class": "PROPOSED", "confidence_band": "LOW", "confidence_score": 0.9,
		"subject_node_ids": []string{ids.New()}}, 422, "score outside band, unknown node")
	if c := ledgerCodes(t, r); c != "score_outside_band,unknown_node" {
		t.Fatalf("findings: %s", c)
	}
	inferred, _ := claim(map[string]any{"statement": "Reads dominate writes ~100:1", "claim_class": "INFERRED", "confidence_band": "MEDIUM",
		"reasoning": "Analysis of link usage patterns", "subject_node_ids": []string{e.nodeA, e.nodeB},
		"evidence": []any{link(analysis.ID, "SUPPORTS"), link(note.ID, "CONTEXT")}, "revalidate_after": "720h"}, 201, "inferred claim")
	if inferred.ExpiresAt == nil || inferred.RevalidateAfter == "" {
		t.Fatalf("rolling expiry not set: %+v", inferred)
	}
	proposal, _ := claim(map[string]any{"statement": "Add a read-through cache", "claim_class": "PROPOSED", "confidence_band": "MEDIUM",
		"reasoning": "Given peak traffic", "assumption_ids": []string{assumption.ID}, "subject_node_ids": []string{e.nodeA}}, 201, "proposal")

	// Independent review.
	expect(t, e.post("/claims/"+observed.ID+"/review", e.token, map[string]any{"decision": "ACCEPTED", "note": "mine"}), http.StatusForbidden, "self review")
	r = e.post("/claims/"+observed.ID+"/review", e.reviewer, map[string]any{"decision": "ACCEPTED", "note": "verified against the page"})
	expect(t, r, http.StatusOK, "independent review")
	var reviewed evidence.Claim
	r.decode(t, &reviewed)
	if reviewed.ReviewStatus != "ACCEPTED" || reviewed.Reviewer == reviewed.CreatedBy {
		t.Fatalf("review: %+v", reviewed)
	}

	// 4.4 Dependency-driven staleness: revoking the evidence stales the observed claim at once.
	r = e.post("/evidence/"+page.ID+"/revoke", e.token, map[string]any{"reason": "page changed"})
	expect(t, r, http.StatusOK, "revoke")
	var revoke struct {
		ClaimsMarkedStale int `json:"claims_marked_stale"`
	}
	r.decode(t, &revoke)
	if revoke.ClaimsMarkedStale != 1 {
		t.Fatalf("revoke staled %d claims", revoke.ClaimsMarkedStale)
	}
	expect(t, e.post("/evidence/"+page.ID+"/revoke", e.token, map[string]any{"reason": "again"}), http.StatusConflict, "double revoke")
	r = e.do(http.MethodGet, e.base+"/claims/"+observed.ID, e.token, "", nil)
	var got evidence.Claim
	got = evidence.Claim{}
	r.decode(t, &got)
	if got.Lifecycle != "STALE" || !strings.Contains(got.StaleReason, "direct evidence") {
		t.Fatalf("observed claim after revoke: %s %q", got.Lifecycle, got.StaleReason)
	}
	// Revalidation needs fresh evidence that satisfies the rules again.
	r = e.post("/claims/"+observed.ID+"/revalidate", e.token, map[string]any{})
	expect(t, r, http.StatusUnprocessableEntity, "revalidate without evidence")
	fresh := e.capture(t, "https://example.com/pricing#2026-11-19", "WEB_PAGE", "VERIFIED", "Plans: 10k links/month (unchanged)", nil)
	r = e.post("/claims/"+observed.ID+"/revalidate", e.token, map[string]any{"evidence": []any{link(fresh.ID, "SUPPORTS")}})
	expect(t, r, http.StatusOK, "revalidate")
	got = evidence.Claim{}
	r.decode(t, &got)
	if got.Lifecycle != "ACTIVE" || got.StaleReason != "" || len(got.Evidence) != 2 {
		t.Fatalf("revalidated: %+v", got)
	}

	// Invalidating an assumption stales the proposal that rests on it.
	r = e.post("/assumptions/"+assumption.ID+"/status", e.token, map[string]any{"status": "INVALIDATED", "note": "load test showed 20x"})
	expect(t, r, http.StatusOK, "invalidate assumption")
	r = e.do(http.MethodGet, e.base+"/claims/"+proposal.ID, e.token, "", nil)
	got = evidence.Claim{}
	r.decode(t, &got)
	if got.Lifecycle != "STALE" || !strings.Contains(got.StaleReason, "assumption") {
		t.Fatalf("proposal after invalidation: %s %q", got.Lifecycle, got.StaleReason)
	}
	expect(t, e.post("/assumptions/"+assumption.ID+"/status", e.token, map[string]any{"status": "VALIDATED", "note": "x"}), http.StatusConflict, "reopening an invalidated assumption")

	// Edits supersede; final states are final.
	newer, _ := claim(map[string]any{"statement": "Reads dominate writes ~200:1", "claim_class": "INFERRED", "confidence_band": "MEDIUM",
		"reasoning": "New analysis", "evidence": []any{link(analysis.ID, "SUPPORTS")}, "supersedes": inferred.ID}, 201, "supersede")
	r = e.do(http.MethodGet, e.base+"/claims/"+inferred.ID, e.token, "", nil)
	got = evidence.Claim{}
	r.decode(t, &got)
	if got.Lifecycle != "SUPERSEDED" || got.SupersededBy != newer.ID {
		t.Fatalf("superseded: %+v", got)
	}
	expect(t, e.post("/claims/"+inferred.ID+"/review", e.reviewer, map[string]any{"decision": "REJECTED", "note": "x"}), http.StatusConflict, "review superseded")
	expect(t, e.post("/claims/"+newer.ID+"/retract", e.token, map[string]any{"reason": "analysis flawed"}), http.StatusOK, "retract")
	expect(t, e.post("/claims/"+newer.ID+"/retract", e.token, map[string]any{"reason": "again"}), http.StatusConflict, "retract twice")

	// Queries.
	r = e.do(http.MethodGet, e.base+"/claims?lifecycle=STALE", e.token, "", nil)
	var list struct{ Items []evidence.Claim }
	r.decode(t, &list)
	if len(list.Items) != 1 || list.Items[0].ID != proposal.ID {
		t.Fatalf("stale claims: %d", len(list.Items))
	}
	r = e.do(http.MethodGet, e.base+"/claims?node_id="+e.nodeB, e.token, "", nil)
	r.decode(t, &list)
	if len(list.Items) != 1 || list.Items[0].ID != inferred.ID {
		t.Fatalf("claims about db.links: %d", len(list.Items))
	}
	expect(t, e.do(http.MethodGet, e.base+"/claims", e.otherTok, "", nil), http.StatusNotFound, "other tenant")

	// Graph patches may cite ledger evidence; revoked or unknown evidence is refused.
	cite := func(key, ref string) string {
		op := addNodeOp(ids.New(), key, "cache", "Cache")
		op["evidence_refs"] = []string{ref}
		return patchBody(1, "cite-"+key, op)
	}
	r = e.do(http.MethodPost, e.base+"/patches", e.token, cite("cache.a", "evidence:"+page.ID), nil)
	expect(t, r, http.StatusUnprocessableEntity, "patch citing revoked evidence")
	if !strings.Contains(findingCodes(decodeResult(t, r).ValidationFindings), "evidence_not_usable") {
		t.Fatal("revoked evidence ref must be refused")
	}
	r = e.do(http.MethodPost, e.base+"/patches", e.token, cite("cache.b", "evidence:"+ids.New()), nil)
	if !strings.Contains(findingCodes(decodeResult(t, r).ValidationFindings), "unknown_evidence") {
		t.Fatal("unknown evidence ref must be refused")
	}
	expect(t, e.do(http.MethodPost, e.base+"/patches", e.token, cite("cache.c", "evidence:"+fresh.ID), nil), http.StatusCreated, "patch citing live evidence")

	// The database is the backstop for every rule.
	tx := func(sql string, args ...any) error { return asTenant(e.pool, e.tenantID, sql, args...) }
	expectPG(t, tx(`INSERT INTO claims (tenant_id, model_id, statement, claim_class, confidence_band, graph_version, created_by_type, created_by)
		VALUES ($1, $2, 'bare observation', 'OBSERVED', 'LOW', 1, 'USER', 'u')`, e.tenantID, e.modelID), "23514", "observed claim without evidence")
	expectPG(t, tx(`UPDATE claims SET statement = 'rewritten' WHERE id = $1`, observed.ID), "23001", "rewriting a claim")
	expectPG(t, tx(`UPDATE evidence_records SET excerpt = 'tampered' WHERE id = $1`, fresh.ID), "23001", "tampering with evidence")
	expectPG(t, tx(`UPDATE claims SET review_status = 'ACCEPTED', reviewer = created_by, reviewed_at = NOW() WHERE id = $1`, proposal.ID), "23514", "self review in SQL")
	expectPG(t, tx(`DELETE FROM evidence_records WHERE id = $1`, fresh.ID), "42501", "deleting evidence")
	expectPG(t, tx(`DELETE FROM claim_evidence WHERE claim_id = $1`, observed.ID), "42501", "deleting links")

	for _, typ := range []string{evidence.EventClaimCreated, evidence.EventClaimStale, evidence.EventEvidenceRevoked, evidence.EventClaimReviewed, evidence.EventClaimRevalidated} {
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_type = $1 AND payload->>'model_id' = $2`, typ, e.modelID).Scan(&n); err != nil || n == 0 {
			t.Fatalf("missing %s events (%v)", typ, err)
		}
	}
}

func TestEvidenceRevalidationSweep(t *testing.T) {
	e := newLedgerEnv(t)
	ctx := context.Background()
	actor := evidence.Actor{Type: "USER", ID: e.userID}
	soon := time.Now().Add(1500 * time.Millisecond)

	ev, _, err := e.ledger.CaptureEvidence(ctx, e.tenantID, e.modelID, evidence.EvidenceInput{
		SourceRef: "bench:latency-2026-11", SourceType: "MEASUREMENT", Content: "p99=42ms", TrustLevel: "HIGH", ExpiresAt: &soon}, actor)
	if err != nil {
		t.Fatal(err)
	}
	measured, err := e.ledger.MakeClaim(ctx, e.tenantID, e.modelID, evidence.ClaimInput{Statement: "p99 redirect latency is 42ms", Class: "OBSERVED",
		ConfidenceBand: "HIGH", Evidence: []evidence.EvidenceLink{{EvidenceID: ev.ID, Relation: "SUPPORTS"}}}, actor)
	if err != nil {
		t.Fatal(err)
	}
	as, err := e.ledger.RecordAssumption(ctx, e.tenantID, e.modelID, evidence.AssumptionInput{Statement: "Traffic stays in two regions",
		Rationale: "current customers", RiskIfWrong: "MEDIUM", ExpiresAt: &soon}, actor)
	if err != nil {
		t.Fatal(err)
	}
	dependent, err := e.ledger.MakeClaim(ctx, e.tenantID, e.modelID, evidence.ClaimInput{Statement: "Two-region active/active is enough",
		Class: "PROPOSED", ConfidenceBand: "MEDIUM", AssumptionIDs: []string{as.ID}}, actor)
	if err != nil {
		t.Fatal(err)
	}
	timed, err := e.ledger.MakeClaim(ctx, e.tenantID, e.modelID, evidence.ClaimInput{Statement: "Vendor quote valid", Class: "PROPOSED",
		ConfidenceBand: "LOW", ExpiresAt: &soon}, actor)
	if err != nil {
		t.Fatal(err)
	}
	durable, err := e.ledger.MakeClaim(ctx, e.tenantID, e.modelID, evidence.ClaimInput{Statement: "Use base62 codes", Class: "PROPOSED", ConfidenceBand: "MEDIUM"}, actor)
	if err != nil {
		t.Fatal(err)
	}

	if res, err := e.ledger.Sweep(ctx); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("pre-expiry sweep: %+v", res)
	}
	for _, id := range []string{measured.ID, dependent.ID, timed.ID} {
		if c, _ := e.ledger.GetClaim(ctx, e.tenantID, e.modelID, id); c.Lifecycle != "ACTIVE" {
			t.Fatalf("claim %s stale before expiry", id)
		}
	}

	time.Sleep(time.Until(soon) + 300*time.Millisecond)
	res, err := e.ledger.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.EvidenceExpired < 1 || res.AssumptionsNeedingReview < 1 || res.ClaimsMarkedStale < 3 {
		t.Fatalf("sweep: %+v", res)
	}
	expectStale := map[string]string{measured.ID: "direct evidence", dependent.ID: "assumption", timed.ID: "expired"}
	for id, reason := range expectStale {
		c, err := e.ledger.GetClaim(ctx, e.tenantID, e.modelID, id)
		if err != nil || c.Lifecycle != "STALE" || !strings.Contains(c.StaleReason, reason) {
			t.Fatalf("claim %s: %s %q (want reason containing %q) %v", id[:8], c.Lifecycle, c.StaleReason, reason, err)
		}
	}
	if c, _ := e.ledger.GetClaim(ctx, e.tenantID, e.modelID, durable.ID); c.Lifecycle != "ACTIVE" {
		t.Fatal("claims with nothing expired must stay ACTIVE")
	}
	if got, _ := e.ledger.GetEvidence(ctx, e.tenantID, e.modelID, ev.ID); got.Status != "EXPIRED" {
		t.Fatalf("evidence status: %s", got.Status)
	}
	if list, _ := e.ledger.ListAssumptions(ctx, e.tenantID, e.modelID, "NEEDS_REVALIDATION"); len(list) != 1 {
		t.Fatalf("assumptions needing revalidation: %d", len(list))
	}

	// Idempotent: a second sweep finds nothing new for this model.
	if _, err := e.ledger.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	var events int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_type = 'claim.stale' AND payload->>'model_id' = $1`, e.modelID).Scan(&events)
	if events != 3 {
		t.Fatalf("expected exactly 3 claim.stale events, got %d", events)
	}

	// Re-establish: validate the assumption, then revalidate the dependent claim.
	later := time.Now().Add(time.Hour)
	if _, _, err := e.ledger.SetAssumptionStatus(ctx, e.tenantID, e.modelID, as.ID, evidence.AssumptionStatusInput{
		Status: "VALIDATED", Note: "confirmed with sales", ExpiresAt: &later}, actor); err != nil {
		t.Fatal(err)
	}
	c, err := e.ledger.RevalidateClaim(ctx, e.tenantID, e.modelID, dependent.ID, evidence.RevalidateInput{ExpiresAt: &later}, actor)
	if err != nil || c.Lifecycle != "ACTIVE" {
		t.Fatalf("revalidate dependent: %+v %v", c, err)
	}

	// Revalidating the measured claim still requires unexpired direct evidence.
	if _, err := e.ledger.RevalidateClaim(ctx, e.tenantID, e.modelID, measured.ID, evidence.RevalidateInput{}, actor); err == nil {
		t.Fatal("revalidation on expired evidence must fail")
	}
	var lc string
	if err := postgres.WithTenantTx(ctx, e.pool, e.tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lifecycle FROM claims WHERE id = $1`, measured.ID).Scan(&lc)
	}); err != nil || lc != "STALE" {
		t.Fatalf("failed revalidation must leave the claim STALE: %s %v", lc, err)
	}
	_ = fmt.Sprint()
}
