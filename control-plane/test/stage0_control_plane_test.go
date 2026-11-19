package test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/systera/control-plane/internal/auth"
	"github.com/systera/control-plane/internal/observability"
	"github.com/systera/control-plane/internal/platform/httpserver"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/projects"
	"github.com/systera/control-plane/internal/runs"
	"github.com/systera/control-plane/internal/tenancy"
)

type memEnv struct {
	client
	srv      *httpserver.Server
	outbox   *outbox.MemoryStore
	runs     *runs.MemoryRepository
	projects *projects.MemoryRepository

	tenantA, tenantB string
	userA, userB     string
	tokA, tokB       string
	tokViewer        string
}

func newMemEnv(t *testing.T, readiness ...httpserver.ReadinessCheck) *memEnv {
	t.Helper()
	out := outbox.NewMemoryStore()
	e := &memEnv{
		outbox:   out,
		runs:     runs.NewMemoryRepository(out),
		projects: projects.NewMemoryRepository(),
		tenantA:  ids.New(), tenantB: ids.New(),
		userA: ids.New(), userB: ids.New(),
	}
	members := auth.NewMemoryMemberships()
	members.Add("alice", auth.Membership{UserID: e.userA, TenantID: e.tenantA, Role: tenancy.RoleOwner})
	members.Add("bob", auth.Membership{UserID: e.userB, TenantID: e.tenantB, Role: tenancy.RoleEngineer})
	members.Add("vera", auth.Membership{UserID: ids.New(), TenantID: e.tenantA, Role: tenancy.RoleViewer})

	if len(readiness) == 0 {
		readiness = []httpserver.ReadinessCheck{{Name: "noop", Check: func(context.Context) error { return nil }}}
	}
	e.srv = httpserver.New(httpserver.Options{
		Logger:               observability.NewLogger("error", "systera-test"),
		Projects:             e.projects,
		Runs:                 e.runs,
		Events:               out,
		Authenticator:        auth.NewAuthenticator(newVerifier(t), members),
		Readiness:            readiness,
		SSEPollInterval:      time.Hour, // prove wake-ups come from notifications, not polling
		SSEHeartbeatInterval: 50 * time.Millisecond,
	})
	e.client = client{t: t, handler: e.srv.Handler()}
	e.tokA = mintToken(t, "alice", "", time.Hour)
	e.tokB = mintToken(t, "bob", "", time.Hour)
	e.tokViewer = mintToken(t, "vera", "", time.Hour)
	return e
}

func (e *memEnv) createProject(t *testing.T) projects.Project {
	t.Helper()
	r := e.do(http.MethodPost, "/v1/projects", e.tokA, `{"name":"URL Shortener","description":"Stage 0 vertical slice"}`, nil)
	expect(t, r, http.StatusCreated, "create project")
	var p projects.Project
	r.decode(t, &p)
	return p
}

func TestHealthz(t *testing.T) {
	e := newMemEnv(t)
	expect(t, e.do(http.MethodGet, "/healthz", "", "", nil), http.StatusOK, "healthz")
}

// Issue: authentication trusted caller-supplied X-Tenant-ID / X-User-ID headers.
func TestAuthenticationIsVerifiedServerSide(t *testing.T) {
	e := newMemEnv(t)

	// Forged identity headers alone are rejected.
	r := e.do(http.MethodPost, "/v1/projects", "", `{"name":"x"}`, map[string]string{"X-Tenant-ID": e.tenantA, "X-User-ID": e.userA})
	expect(t, r, http.StatusUnauthorized, "headers without token")
	if r.header.Get("WWW-Authenticate") == "" {
		t.Fatal("401 must carry WWW-Authenticate")
	}

	cases := map[string]string{
		"garbage":        "not-a-jwt",
		"wrong key":      mintTokenWith(t, otherPrivateKey, auth.Claims{Subject: "alice"}, time.Hour),
		"expired":        mintToken(t, "alice", "", -time.Hour),
		"wrong issuer":   mintTokenWith(t, testPrivateKey, auth.Claims{Subject: "alice", Issuer: "evil"}, time.Hour),
		"wrong audience": mintTokenWith(t, testPrivateKey, auth.Claims{Subject: "alice", Audience: auth.Audience{"other"}}, time.Hour),
		"unknown user":   mintToken(t, "mallory", "", time.Hour),
	}
	for name, tok := range cases {
		r := e.do(http.MethodGet, "/v1/projects", tok, "", nil)
		want := http.StatusUnauthorized
		if name == "unknown user" {
			want = http.StatusForbidden // valid identity, no membership
		}
		expect(t, r, want, name)
	}

	// A valid user cannot select a tenant they do not belong to, by header or by claim.
	expect(t, e.do(http.MethodGet, "/v1/projects", e.tokA, "", map[string]string{"X-Tenant-ID": e.tenantB}), http.StatusForbidden, "header tenant switch")
	expect(t, e.do(http.MethodGet, "/v1/projects", mintToken(t, "alice", e.tenantB, time.Hour), "", nil), http.StatusForbidden, "claim tenant switch")

	// The principal comes from the membership, not from headers: X-User-ID is ignored.
	r = e.do(http.MethodPost, "/v1/projects", e.tokA, `{"name":"Mine"}`, map[string]string{"X-User-ID": e.userB})
	expect(t, r, http.StatusCreated, "create")
	var p projects.Project
	r.decode(t, &p)
	if p.TenantID != e.tenantA || p.CreatedBy != e.userA {
		t.Fatalf("principal not taken from membership: %+v", p)
	}

	// Role comes from the membership too: viewers are read-only, and X-Role is ignored.
	expect(t, e.do(http.MethodPost, "/v1/projects", e.tokViewer, `{"name":"x"}`, map[string]string{"X-Role": "OWNER"}), http.StatusForbidden, "viewer write")
	expect(t, e.do(http.MethodGet, "/v1/projects", e.tokViewer, "", nil), http.StatusOK, "viewer read")
}

func TestTenantIsolation(t *testing.T) {
	e := newMemEnv(t)
	p := e.createProject(t)
	expect(t, e.do(http.MethodGet, "/v1/projects/"+p.ID, e.tokB, "", nil), http.StatusNotFound, "cross-tenant project read")
	expect(t, e.do(http.MethodPost, "/v1/projects/"+p.ID+"/runs", e.tokB, `{"objective":"x"}`, nil), http.StatusNotFound, "cross-tenant run create")

	r := e.do(http.MethodPost, "/v1/projects/"+p.ID+"/runs", e.tokA, `{"objective":"x"}`, nil)
	expect(t, r, http.StatusAccepted, "run create")
	var run runs.DesignRun
	r.decode(t, &run)
	expect(t, e.do(http.MethodGet, "/v1/runs/"+run.ID, e.tokB, "", nil), http.StatusNotFound, "cross-tenant run read")
	expect(t, e.do(http.MethodGet, "/v1/runs/"+run.ID+"/events", e.tokB, "", nil), http.StatusNotFound, "cross-tenant events")
	expect(t, e.do(http.MethodPost, "/v1/runs/"+run.ID+"/cancel", e.tokB, "", nil), http.StatusNotFound, "cross-tenant cancel")
}

// Issue: replaying an Idempotency-Key with a different body returned the existing run.
func TestIdempotencyConflictDetection(t *testing.T) {
	e := newMemEnv(t)
	p := e.createProject(t)
	path := "/v1/projects/" + p.ID + "/runs"
	key := map[string]string{"Idempotency-Key": "idem-url-shortener-1"}
	body := `{"objective":"Design a globally distributed URL shortener","max_budget_usd":10.0,"constraints":{"region":"eu","rps":1000}}`

	r1 := e.do(http.MethodPost, path, e.tokA, body, key)
	expect(t, r1, http.StatusAccepted, "first create")
	var run1 runs.DesignRun
	r1.decode(t, &run1)
	if r1.header.Get("Idempotent-Replayed") != "" {
		t.Fatal("first create must not be marked replayed")
	}

	// Same request (different key order / whitespace) replays.
	r2 := e.do(http.MethodPost, path, e.tokA, `{ "constraints":{"rps":1000,"region":"eu"}, "max_budget_usd":10, "objective":"Design a globally distributed URL shortener" }`, key)
	expect(t, r2, http.StatusAccepted, "replay")
	var run2 runs.DesignRun
	r2.decode(t, &run2)
	if run1.ID != run2.ID || r2.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("expected replay of %s, got %s (replayed=%q)", run1.ID, run2.ID, r2.header.Get("Idempotent-Replayed"))
	}

	// Different body under the same key is a conflict.
	for name, b := range map[string]string{
		"objective":   `{"objective":"Something else","max_budget_usd":10.0,"constraints":{"region":"eu","rps":1000}}`,
		"budget":      `{"objective":"Design a globally distributed URL shortener","max_budget_usd":11.0,"constraints":{"region":"eu","rps":1000}}`,
		"constraints": `{"objective":"Design a globally distributed URL shortener","max_budget_usd":10.0,"constraints":{"region":"us","rps":1000}}`,
	} {
		r := e.do(http.MethodPost, path, e.tokA, b, key)
		expect(t, r, http.StatusUnprocessableEntity, "conflict on "+name)
		var body map[string]string
		r.decode(t, &body)
		if body["code"] != "idempotency_key_conflict" {
			t.Fatalf("conflict code: %v", body)
		}
	}

	events, _ := e.outbox.ListByRun(context.Background(), e.tenantA, run1.ID)
	if len(events) != 1 || events[0].EventType != runs.EventRunCreated {
		t.Fatalf("expected exactly one run.created event, got %d", len(events))
	}

	// Without a key, every request creates a new run.
	ra := e.do(http.MethodPost, path, e.tokA, `{"objective":"no key"}`, nil)
	rb := e.do(http.MethodPost, path, e.tokA, `{"objective":"no key"}`, nil)
	var a, b runs.DesignRun
	ra.decode(t, &a)
	rb.decode(t, &b)
	if a.ID == b.ID {
		t.Fatal("keyless requests must not be deduplicated")
	}
	if a.MaxBudgetUSD != runs.DefaultBudgetUSD {
		t.Fatalf("default budget: %v", a.MaxBudgetUSD)
	}

	expect(t, e.do(http.MethodPost, path, e.tokA, `{"objective":"x","max_budget_usd":-1}`, nil), http.StatusBadRequest, "negative budget")
	expect(t, e.do(http.MethodPost, path, e.tokA, `{"objective":"   "}`, nil), http.StatusBadRequest, "blank objective")
}

// Issue: run creation reported success when the outbox append failed.
func TestRunCreationFailsAtomicallyWhenOutboxFails(t *testing.T) {
	e := newMemEnv(t)
	p := e.createProject(t)

	e.outbox.FailAppends(errors.New("outbox unavailable"))
	r := e.do(http.MethodPost, "/v1/projects/"+p.ID+"/runs", e.tokA, `{"objective":"x"}`, map[string]string{"Idempotency-Key": "k1"})
	expect(t, r, http.StatusInternalServerError, "create with failing outbox")

	// The run was not committed: once the outbox recovers, the same key creates a fresh run
	// with exactly one event.
	e.outbox.FailAppends(nil)
	r = e.do(http.MethodPost, "/v1/projects/"+p.ID+"/runs", e.tokA, `{"objective":"x"}`, map[string]string{"Idempotency-Key": "k1"})
	expect(t, r, http.StatusAccepted, "create after recovery")
	if r.header.Get("Idempotent-Replayed") != "" {
		t.Fatal("failed attempt must not have left a run behind")
	}
	var run runs.DesignRun
	r.decode(t, &run)
	events, _ := e.outbox.ListByRun(context.Background(), e.tenantA, run.ID)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	// State transitions are atomic with their event too.
	e.outbox.FailAppends(errors.New("outbox unavailable"))
	if _, err := e.runs.UpdateState(context.Background(), e.tenantA, run.ID, runs.StatePlanning); err == nil {
		t.Fatal("expected transition to fail with outbox down")
	}
	e.outbox.FailAppends(nil)
	got, _ := e.runs.GetByID(context.Background(), e.tenantA, run.ID)
	if got.State != runs.StateCreated {
		t.Fatalf("state changed despite outbox failure: %s", got.State)
	}
}

// Issue: /readyz returned ready without checking dependencies.
func TestReadinessReflectsDependencies(t *testing.T) {
	dbUp := true
	e := newMemEnv(t,
		httpserver.ReadinessCheck{Name: "postgres", Check: func(context.Context) error {
			if !dbUp {
				return errors.New("connection refused")
			}
			return nil
		}},
		httpserver.ReadinessCheck{Name: "event_listener", Check: func(context.Context) error { return nil }},
	)

	r := e.do(http.MethodGet, "/readyz", "", "", nil)
	expect(t, r, http.StatusOK, "ready")
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	r.decode(t, &body)
	if body.Status != "ready" || body.Checks["postgres"] != "ok" || body.Checks["event_listener"] != "ok" {
		t.Fatalf("unexpected readiness body: %s", r.body)
	}

	dbUp = false
	r = e.do(http.MethodGet, "/readyz", "", "", nil)
	expect(t, r, http.StatusServiceUnavailable, "not ready")
	r.decode(t, &body)
	if body.Status != "not_ready" || body.Checks["postgres"] != "fail" {
		t.Fatalf("unexpected readiness body: %s", r.body)
	}
	expect(t, e.do(http.MethodGet, "/healthz", "", "", nil), http.StatusOK, "liveness unaffected")

	// A server with no checks wired is not ready.
	bare := httpserver.New(httpserver.Options{Logger: observability.NewLogger("error", "t")})
	rec := httptest.NewRecorder()
	bare.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("bare server readyz: %d", rec.Code)
	}
}

// Issue: SSE sent the stored events and ended instead of streaming new ones.
func TestRunEventsStreamLive(t *testing.T) {
	e := newMemEnv(t)
	ts := httptest.NewServer(e.srv.Handler())
	defer ts.Close()

	p := e.createProject(t)
	r := e.do(http.MethodPost, "/v1/projects/"+p.ID+"/runs", e.tokA, `{"objective":"stream me"}`, nil)
	expect(t, r, http.StatusAccepted, "create run")
	var run runs.DesignRun
	r.decode(t, &run)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := openSSE(t, ctx, ts.URL+"/v1/runs/"+run.ID+"/events", e.tokA, nil)

	first := nextEvent(t, stream, 2*time.Second)
	if first.Event != runs.EventRunCreated {
		t.Fatalf("expected backlog run.created, got %+v", first)
	}

	// The connection stays open and delivers events committed after it was opened.
	time.Sleep(100 * time.Millisecond) // let heartbeats flow; stream must not end
	if _, err := e.runs.UpdateState(ctx, e.tenantA, run.ID, runs.StatePlanning); err != nil {
		t.Fatal(err)
	}
	live := nextEvent(t, stream, 2*time.Second)
	if live.Event != runs.EventRunStateChanged || live.ID == first.ID {
		t.Fatalf("expected live run.state_changed, got %+v", live)
	}

	// Resume from Last-Event-ID only replays later events.
	resume := openSSE(t, ctx, ts.URL+"/v1/runs/"+run.ID+"/events", e.tokA, map[string]string{"Last-Event-ID": first.ID})
	if ev := nextEvent(t, resume, 2*time.Second); ev.ID != live.ID {
		t.Fatalf("resume: expected event %s, got %+v", live.ID, ev)
	}

	// Reaching a terminal state ends both streams after delivering the final event.
	expect(t, e.do(http.MethodPost, "/v1/runs/"+run.ID+"/cancel", e.tokA, "", nil), http.StatusOK, "cancel")
	for _, s := range []<-chan sseEvent{stream, resume} {
		if ev := nextEvent(t, s, 2*time.Second); ev.Event != runs.EventRunStateChanged {
			t.Fatalf("expected terminal state change, got %+v", ev)
		}
		if ev := nextEvent(t, s, 2*time.Second); ev.Event != "stream.end" {
			t.Fatalf("expected stream.end, got %+v", ev)
		}
		expectClosed(t, s, 2*time.Second)
	}
	expect(t, e.do(http.MethodPost, "/v1/runs/"+run.ID+"/cancel", e.tokA, "", nil), http.StatusConflict, "cancel twice")

	expect(t, e.do(http.MethodGet, "/v1/runs/"+run.ID+"/events", e.tokA, "", map[string]string{"Last-Event-ID": "abc"}), http.StatusBadRequest, "bad cursor")
}

func TestStateMachine(t *testing.T) {
	e := newMemEnv(t)
	run, _, err := e.runs.CreateIdempotent(context.Background(), runs.DesignRun{TenantID: e.tenantA, ProjectID: ids.New(), Objective: "x", CreatedBy: e.userA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.runs.UpdateState(context.Background(), e.tenantA, run.ID, runs.StatePlanning); err != nil {
		t.Fatalf("CREATED -> PLANNING: %v", err)
	}
	if _, err := e.runs.UpdateState(context.Background(), e.tenantA, run.ID, runs.StateCompleted); !errors.Is(err, runs.ErrInvalidTransition) {
		t.Fatalf("PLANNING -> COMPLETED must fail, got %v", err)
	}
	if _, err := e.runs.UpdateState(context.Background(), e.tenantB, run.ID, runs.StatePlanValidating); !errors.Is(err, runs.ErrNotFound) {
		t.Fatalf("cross-tenant transition must be not found, got %v", err)
	}
}
