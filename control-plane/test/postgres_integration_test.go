package test

// Integration tests against a real PostgreSQL. They run only when
// SYSTERA_TEST_DATABASE_URL is set (see `make test-integration`), e.g.
//
//	SYSTERA_TEST_DATABASE_URL=postgres://systera:systera_dev_password@localhost:5432/systera?sslmode=disable

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/auth"
	"github.com/systera/control-plane/internal/observability"
	"github.com/systera/control-plane/internal/platform/httpserver"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/projects"
	"github.com/systera/control-plane/internal/runs"
)

type pgEnv struct {
	client
	srv    *httpserver.Server
	pool   *pgxpool.Pool
	events *outbox.PostgresStore
	stop   context.CancelFunc
}

func testDSN(t *testing.T) string {
	dsn := os.Getenv("SYSTERA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SYSTERA_TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	return dsn
}

// newPGEnv builds an API instance on its own pool and listener, like a separate process.
func newPGEnv(t *testing.T, dsn string) *pgEnv {
	t.Helper()
	ctx := context.Background()
	pool, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	logger := observability.NewLogger("error", "systera-it")
	events := outbox.NewPostgresStore(pool, logger)
	lctx, stop := context.WithCancel(ctx)
	go events.Listen(lctx)
	waitFor(t, 5*time.Second, func() bool { return events.Healthy(ctx) == nil }, "listener connect")

	srv := httpserver.New(httpserver.Options{
		Logger:        logger,
		Projects:      projects.NewPostgresRepository(pool),
		Runs:          runs.NewPostgresRepository(pool),
		Events:        events,
		Authenticator: auth.NewAuthenticator(newVerifier(t), auth.NewPostgresMemberships(pool)),
		Readiness: []httpserver.ReadinessCheck{
			{Name: "postgres", Check: postgres.PingCheck(pool)},
			{Name: "schema", Check: postgres.SchemaCheck(pool)},
			{Name: "event_listener", Check: events.Healthy},
		},
		SSEPollInterval:      time.Hour, // only LISTEN/NOTIFY can wake streams in these tests
		SSEHeartbeatInterval: 100 * time.Millisecond,
	})
	e := &pgEnv{client: client{t: t, handler: srv.Handler()}, srv: srv, pool: pool, events: events, stop: stop}
	t.Cleanup(func() { stop(); pool.Close() })
	return e
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// seedIdentity creates a tenant, a user and a membership; returns tenantID, userID, subject.
func seedIdentity(t *testing.T, pool *pgxpool.Pool, role string) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	suffix := ids.New()[:8]
	subject := "it-" + suffix
	var tenantID, userID string
	if err := pool.QueryRow(ctx, `INSERT INTO tenants (slug, name) VALUES ($1, $1) RETURNING id::text`, "t-"+suffix).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, display_name, external_sub) VALUES ($1, $1, $2) RETURNING id::text`,
		subject+"@example.test", subject).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_memberships (tenant_id, user_id, role) VALUES ($1, $2, $3)`, tenantID, userID, role); err != nil {
		t.Fatal(err)
	}
	return tenantID, userID, subject
}

func TestPostgresEndToEnd(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()

	// Migrations are idempotent and leave the schema current.
	boot, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	for i := 0; i < 2; i++ {
		if _, err := postgres.MigrateUp(ctx, boot); err != nil {
			t.Fatalf("migrate up #%d: %v", i+1, err)
		}
	}
	if err := postgres.SchemaCheck(boot)(ctx); err != nil {
		t.Fatal(err)
	}

	tenantA, userA, subA := seedIdentity(t, boot, "OWNER")
	tenantB, _, subB := seedIdentity(t, boot, "ENGINEER")
	tokA := mintToken(t, subA, "", time.Hour)
	tokB := mintToken(t, subB, "", time.Hour)

	api := newPGEnv(t, dsn)

	// --- Readiness reflects real dependencies.
	expect(t, api.do(http.MethodGet, "/readyz", "", "", nil), http.StatusOK, "readyz")

	// --- Auth: identity is resolved from the database.
	expect(t, api.do(http.MethodGet, "/v1/projects", "", "", map[string]string{"X-Tenant-ID": tenantA, "X-User-ID": userA}), http.StatusUnauthorized, "header-only identity")
	expect(t, api.do(http.MethodGet, "/v1/projects", tokA, "", map[string]string{"X-Tenant-ID": tenantB}), http.StatusForbidden, "foreign tenant")
	if _, err := boot.Exec(ctx, `UPDATE tenants SET status = 'SUSPENDED' WHERE id = $1`, tenantB); err != nil {
		t.Fatal(err)
	}
	expect(t, api.do(http.MethodGet, "/v1/projects", tokB, "", nil), http.StatusForbidden, "suspended tenant")
	if _, err := boot.Exec(ctx, `UPDATE tenants SET status = 'ACTIVE' WHERE id = $1`, tenantB); err != nil {
		t.Fatal(err)
	}

	// --- Projects and runs persist.
	r := api.do(http.MethodPost, "/v1/projects", tokA, `{"name":"URL Shortener","description":"durable"}`, nil)
	expect(t, r, http.StatusCreated, "create project")
	var proj projects.Project
	r.decode(t, &proj)
	if proj.TenantID != tenantA || proj.CreatedBy != userA {
		t.Fatalf("project principal: %+v", proj)
	}
	expect(t, api.do(http.MethodGet, "/v1/projects/"+proj.ID, tokB, "", nil), http.StatusNotFound, "cross-tenant project")
	expect(t, api.do(http.MethodGet, "/v1/projects/not-a-uuid", tokA, "", nil), http.StatusNotFound, "malformed id")

	runsPath := "/v1/projects/" + proj.ID + "/runs"
	key := map[string]string{"Idempotency-Key": "it-" + ids.New()}
	body := `{"objective":"Design a URL shortener","max_budget_usd":12.5,"constraints":{"region":"eu"},"acceptance_criteria":["p99<50ms"]}`
	r = api.do(http.MethodPost, runsPath, tokA, body, key)
	expect(t, r, http.StatusAccepted, "create run")
	var run runs.DesignRun
	r.decode(t, &run)

	r = api.do(http.MethodPost, runsPath, tokA, body, key)
	expect(t, r, http.StatusAccepted, "replay")
	var replay runs.DesignRun
	r.decode(t, &replay)
	if replay.ID != run.ID || r.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay returned %s (replayed=%q)", replay.ID, r.header.Get("Idempotent-Replayed"))
	}
	expect(t, api.do(http.MethodPost, runsPath, tokA, strings.Replace(body, "12.5", "13", 1), key), http.StatusUnprocessableEntity, "idempotency conflict")

	ra := api.do(http.MethodPost, runsPath, tokA, `{"objective":"keyless"}`, nil)
	rb := api.do(http.MethodPost, runsPath, tokA, `{"objective":"keyless"}`, nil)
	expect(t, ra, http.StatusAccepted, "keyless a")
	expect(t, rb, http.StatusAccepted, "keyless b")
	var ka, kb runs.DesignRun
	ra.decode(t, &ka)
	rb.decode(t, &kb)
	if ka.ID == kb.ID {
		t.Fatal("keyless runs must be distinct")
	}

	// --- Durability: a brand-new API instance (new pool, as after a restart) sees it all.
	restarted := newPGEnv(t, dsn)
	r = restarted.do(http.MethodGet, "/v1/runs/"+run.ID, tokA, "", nil)
	expect(t, r, http.StatusOK, "run after restart")
	var reloaded runs.DesignRun
	r.decode(t, &reloaded)
	if reloaded.Objective != run.Objective || reloaded.MaxBudgetUSD != 12.5 || reloaded.Constraints["region"] != "eu" ||
		len(reloaded.AcceptanceCriteria) != 1 || reloaded.CreatedBy != userA || reloaded.IdempotencyKey != key["Idempotency-Key"] {
		t.Fatalf("run not persisted faithfully: %+v", reloaded)
	}
	expect(t, restarted.do(http.MethodGet, "/v1/projects/"+proj.ID, tokA, "", nil), http.StatusOK, "project after restart")
	expect(t, restarted.do(http.MethodPost, runsPath, tokA, body, key), http.StatusAccepted, "replay after restart")

	var auditCount int
	if err := boot.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE resource_id IN ($1, $2)`, proj.ID, run.ID).Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatalf("expected 2 audit events, got %d (%v)", auditCount, err)
	}

	// --- Live SSE driven by LISTEN/NOTIFY across instances.
	ts := httptest.NewServer(api.srv.Handler())
	defer ts.Close()
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	stream := openSSE(t, sctx, ts.URL+"/v1/runs/"+run.ID+"/events", tokA, nil)
	created := nextEvent(t, stream, 3*time.Second)
	if created.Event != runs.EventRunCreated {
		t.Fatalf("backlog: %+v", created)
	}
	// The transition is written by the *other* instance; only a NOTIFY can wake this stream.
	if _, err := runs.NewPostgresRepository(restarted.pool).UpdateState(ctx, tenantA, run.ID, runs.StatePlanning); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, stream, 3*time.Second); ev.Event != runs.EventRunStateChanged {
		t.Fatalf("live event: %+v", ev)
	}
	resume := openSSE(t, sctx, ts.URL+"/v1/runs/"+run.ID+"/events?after="+created.ID, tokA, nil)
	if ev := nextEvent(t, resume, 3*time.Second); ev.Event != runs.EventRunStateChanged {
		t.Fatalf("resume: %+v", ev)
	}
	expect(t, restarted.do(http.MethodPost, "/v1/runs/"+run.ID+"/cancel", tokA, "", nil), http.StatusOK, "cancel")
	for _, s := range []<-chan sseEvent{stream, resume} {
		if ev := nextEvent(t, s, 3*time.Second); ev.Event != runs.EventRunStateChanged {
			t.Fatalf("terminal event: %+v", ev)
		}
		if ev := nextEvent(t, s, 3*time.Second); ev.Event != "stream.end" {
			t.Fatalf("stream end: %+v", ev)
		}
		expectClosed(t, s, 3*time.Second)
	}

	// --- Atomicity: if the outbox insert fails, the run is rolled back.
	if _, err := boot.Exec(ctx, `
		CREATE OR REPLACE FUNCTION it_fail_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.payload->>'objective' = 'explode' THEN RAISE EXCEPTION 'simulated outbox failure'; END IF;
			RETURN NEW;
		END $$;
		DROP TRIGGER IF EXISTS it_fail_outbox ON outbox_events;
		CREATE TRIGGER it_fail_outbox BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION it_fail_outbox();`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = boot.Exec(context.Background(), `DROP TRIGGER IF EXISTS it_fail_outbox ON outbox_events; DROP FUNCTION IF EXISTS it_fail_outbox();`)
	})
	expect(t, api.do(http.MethodPost, runsPath, tokA, `{"objective":"explode"}`, map[string]string{"Idempotency-Key": "boom"}), http.StatusInternalServerError, "outbox failure")
	var orphaned int
	if err := boot.QueryRow(ctx, `SELECT count(*) FROM design_runs WHERE project_id = $1 AND objective = 'explode'`, proj.ID).Scan(&orphaned); err != nil || orphaned != 0 {
		t.Fatalf("run committed without its event: count=%d err=%v", orphaned, err)
	}

	// --- RLS is enforced for the application role, not just by WHERE clauses.
	assertRLS(t, boot, tenantA, tenantB, userA, run.ID)

	// --- Readiness turns 503 when the event listener is gone.
	api.stop()
	waitFor(t, 3*time.Second, func() bool { return api.events.Healthy(ctx) != nil }, "listener down")
	expect(t, api.do(http.MethodGet, "/readyz", "", "", nil), http.StatusServiceUnavailable, "readyz without listener")
}

func assertRLS(t *testing.T, pool *pgxpool.Pool, tenantA, tenantB, userA, runA string) {
	t.Helper()
	ctx := context.Background()
	count := func(tenant, q string, args ...any) int {
		var n int
		err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+postgres.AppRole); err != nil {
				return err
			}
			if tenant != "" {
				if _, err := tx.Exec(ctx, "SELECT set_config('app.current_tenant_id', $1, true)", tenant); err != nil {
					return err
				}
			}
			return tx.QueryRow(ctx, q, args...).Scan(&n)
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(tenantA, `SELECT count(*) FROM design_runs WHERE id = $1`, runA); n != 1 {
		t.Fatalf("own tenant should see run, saw %d", n)
	}
	for name, tenant := range map[string]string{"other tenant": tenantB, "no tenant": ""} {
		if n := count(tenant, `SELECT count(*) FROM design_runs WHERE id = $1`, runA); n != 0 {
			t.Fatalf("%s saw %d foreign runs", name, n)
		}
		if n := count(tenant, `SELECT count(*) FROM outbox_events WHERE run_id = $1`, runA); n != 0 {
			t.Fatalf("%s saw %d foreign events", name, n)
		}
	}
	// Writes into another tenant are refused by WITH CHECK.
	err := postgres.WithTenantTx(ctx, pool, tenantB, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO projects (tenant_id, name, created_by) VALUES ($1, 'x', $2)`, tenantA, userA)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("cross-tenant insert must be rejected by RLS, got %v", err)
	}
}
