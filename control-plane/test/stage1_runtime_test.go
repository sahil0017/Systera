package test

// Stage 1 (Durable Agent Core) integration tests. Require SYSTERA_TEST_DATABASE_URL;
// the NATS round-trip additionally requires SYSTERA_TEST_NATS_URL.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/agent"
	"github.com/systera/control-plane/internal/auth"
	"github.com/systera/control-plane/internal/models"
	"github.com/systera/control-plane/internal/platform/httpserver"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/natsbus"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/projects"
	"github.com/systera/control-plane/internal/runs"
	"github.com/systera/control-plane/internal/runtime"
	"github.com/systera/control-plane/internal/scheduler"
	"github.com/systera/control-plane/internal/tools"
)

const fakeModel = "fake-model"

var fakePricing = models.Pricing{fakeModel: {InputPerMTok: 100, OutputPerMTok: 200}} // $0.02 per 100/50-token call

type s1Env struct {
	client
	pool     *pgxpool.Pool
	store    *scheduler.Store
	logger   *slog.Logger
	tenantID string
	token    string
}

func newS1Env(t *testing.T) *s1Env {
	t.Helper()
	dsn := testDSN(t)
	ctx := context.Background()
	pool, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := postgres.MigrateUp(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// Claims span the whole database, so cancel in-flight work left behind by an aborted
	// earlier test run (tests in this package run sequentially).
	if _, err := pool.Exec(ctx, `
		UPDATE agent_tasks SET state = 'CANCELLED', lease_owner = NULL, lease_token = NULL, lease_expires_at = NULL
		WHERE state IN ('PENDING', 'READY', 'LEASED');
		UPDATE design_runs SET state = 'CANCELLED', reserved_budget_usd = 0 WHERE state = 'RUNNING';`); err != nil {
		t.Fatal(err)
	}
	tenantID, _, subject := seedIdentity(t, pool, "OWNER")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := scheduler.NewStore(pool, logger)
	srv := httpserver.New(httpserver.Options{
		Logger:        logger,
		Projects:      projects.NewPostgresRepository(pool),
		Runs:          runs.NewPostgresRepository(pool),
		Events:        outbox.NewPostgresStore(pool, logger),
		Tasks:         store,
		Authenticator: auth.NewAuthenticator(newVerifier(t), auth.NewPostgresMemberships(pool)),
		Readiness:     []httpserver.ReadinessCheck{{Name: "postgres", Check: postgres.PingCheck(pool)}},
	})
	return &s1Env{
		client: client{t: t, handler: srv.Handler()}, pool: pool, store: store, logger: logger,
		tenantID: tenantID, token: mintToken(t, subject, "", time.Hour),
	}
}

func (e *s1Env) newRun(t *testing.T, budget float64) string {
	t.Helper()
	r := e.do(http.MethodPost, "/v1/projects", e.token, `{"name":"stage1"}`, nil)
	expect(t, r, http.StatusCreated, "project")
	var p projects.Project
	r.decode(t, &p)
	r = e.do(http.MethodPost, "/v1/projects/"+p.ID+"/runs", e.token,
		fmt.Sprintf(`{"objective":"Design a URL shortener","max_budget_usd":%v,"constraints":{"rps":10000}}`, budget), nil)
	expect(t, r, http.StatusAccepted, "run")
	var run runs.DesignRun
	r.decode(t, &run)
	return run.ID
}

func (e *s1Env) plan(t *testing.T, runID string, tasks ...map[string]any) response {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"tasks": tasks})
	return e.do(http.MethodPost, "/v1/runs/"+runID+"/plan", e.token, string(b), nil)
}

func (e *s1Env) run(t *testing.T, runID string) runs.DesignRun {
	t.Helper()
	r := e.do(http.MethodGet, "/v1/runs/"+runID, e.token, "", nil)
	expect(t, r, http.StatusOK, "get run")
	var run runs.DesignRun
	r.decode(t, &run)
	return run
}

func (e *s1Env) tasks(t *testing.T, runID string) map[string]runtime.Task {
	t.Helper()
	r := e.do(http.MethodGet, "/v1/runs/"+runID+"/tasks", e.token, "", nil)
	expect(t, r, http.StatusOK, "list tasks")
	var body struct{ Items []runtime.Task }
	r.decode(t, &body)
	out := map[string]runtime.Task{}
	for _, tk := range body.Items {
		out[tk.Key] = tk
	}
	return out
}

func task(key string, deps ...string) map[string]any {
	return map[string]any{
		"key": key, "agent_type": "requirements_analyst", "objective": "analyse " + key,
		"depends_on": deps, "budget_usd": 0.5, "granted_capabilities": []string{"run.read", "task.read"},
	}
}

func (e *s1Env) newRunner(t *testing.T, worker string, p models.Provider, ttl time.Duration) *agent.Runner {
	t.Helper()
	reg := tools.NewRegistry()
	if err := agent.RegisterBuiltinTools(reg); err != nil {
		t.Fatal(err)
	}
	return agent.NewRunner(agent.Config{WorkerID: worker, AgentTypes: []string{"requirements_analyst"}, LeaseTTL: ttl},
		e.store, agent.NewPostgresCatalog(e.pool), models.NewGateway(p, fakePricing, fakeModel),
		tools.NewGateway(reg, e.logger), e.logger)
}

// envelopeOf extracts the task envelope the runner sent to the model.
func envelopeOf(msgs []models.Message) agent.TaskEnvelope {
	content := msgs[1].Content
	start := strings.LastIndex(content, "```json\n")
	end := strings.LastIndex(content, "\n```")
	var env agent.TaskEnvelope
	if start >= 0 && end > start {
		_ = json.Unmarshal([]byte(content[start+len("```json\n"):end]), &env)
	}
	return env
}

func final(summary string, output map[string]any) string {
	b, _ := json.Marshal(map[string]any{"action": "final", "result": map[string]any{
		"status": "SUCCEEDED", "summary": summary, "output": output, "confidence": 0.8,
	}})
	return string(b)
}

func usageResp(content string) models.Response {
	return models.Response{Content: content, Usage: models.Usage{PromptTokens: 100, CompletionTokens: 50}}
}

func dbCount(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---------------------------------------------------------------------------

func TestStage1DAGExecutesWithConcurrentWorkers(t *testing.T) {
	e := newS1Env(t)
	runID := e.newRun(t, 5)

	// Invalid plans are rejected before anything is persisted.
	r := e.plan(t, runID, task("a", "c"), task("b", "a"), task("c", "b"))
	expect(t, r, http.StatusBadRequest, "cyclic plan")
	if !strings.Contains(string(r.body), "cycle") {
		t.Fatalf("cycle not reported: %s", r.body)
	}
	bad := task("x")
	bad["agent_type"] = "no_such_agent"
	expect(t, e.plan(t, runID, bad), http.StatusBadRequest, "unknown agent")
	if e.run(t, runID).State != runs.StateCreated {
		t.Fatal("rejected plan must not change the run")
	}

	// Diamond: a -> {b, c} -> d. d uses a tool to read upstream results.
	d := task("d", "b", "c")
	d["max_tool_calls"] = 2
	r = e.plan(t, runID, task("a"), task("b", "a"), task("c", "a"), d)
	expect(t, r, http.StatusCreated, "plan")
	expect(t, e.plan(t, runID, task("z")), http.StatusConflict, "second plan")
	if st := e.run(t, runID).State; st != runs.StateRunning {
		t.Fatalf("run should be RUNNING after planning, got %s", st)
	}
	ts := e.tasks(t, runID)
	if ts["a"].State != runtime.TaskReady || ts["b"].State != runtime.TaskPending || len(ts["d"].DependsOn) != 2 {
		t.Fatalf("initial task states: %+v", ts)
	}

	var mu sync.Mutex
	var order []string
	var dSawUpstream bool
	provider := models.FuncProvider(func(_ context.Context, req models.Request) (models.Response, error) {
		env := envelopeOf(req.Messages)
		if env.SchemaVersion != "1.0" || env.RunID != runID || env.TenantID != e.tenantID || env.Budget.MaxCostUSD != 0.5 {
			return models.Response{}, fmt.Errorf("bad envelope: %+v", env)
		}
		key := strings.TrimPrefix(env.Objective, "analyse ")
		if key == "d" && len(req.Messages) == 2 {
			return usageResp(`{"action":"tool_calls","tool_calls":[{"name":"task.dependency_results","arguments":{}},{"name":"fs.write","arguments":{}}]}`), nil
		}
		if key == "d" {
			last := req.Messages[len(req.Messages)-1].Content
			dSawUpstream = strings.Contains(last, `"key":"b"`) && strings.Contains(last, `"key":"c"`) && strings.Contains(last, "unknown tool")
		}
		mu.Lock()
		order = append(order, key)
		mu.Unlock()
		return usageResp(final("requirements for "+key, map[string]any{"task": key})), nil
	})

	// Two workers race for tasks; SKIP LOCKED must hand each task to exactly one.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, w := range []string{"worker-1", "worker-2"} {
		runner := e.newRunner(t, w, provider, 10*time.Second)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				claimed, err := runner.RunOnce(ctx)
				if err != nil {
					t.Errorf("runner: %v", err)
					return
				}
				if !claimed {
					if st := e.run(t, runID).State; st != runs.StateRunning {
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
		}()
	}
	wg.Wait()

	run := e.run(t, runID)
	if run.State != runs.StateEvaluating {
		t.Fatalf("run should be EVALUATING once every task succeeded, got %s (%s)", run.State, run.FailureReason)
	}
	if len(order) != 4 || order[0] != "a" || order[3] != "d" {
		t.Fatalf("execution order violates the DAG: %v", order)
	}
	if !dSawUpstream {
		t.Fatal("task d did not receive upstream results via the tool gateway (or the disallowed tool was not rejected)")
	}
	// 5 model calls at $0.02 each.
	if diff := run.SpentBudgetUSD - 0.10; diff > 1e-6 || diff < -1e-6 || run.ReservedBudgetUSD != 0 {
		t.Fatalf("budget: spent=%v reserved=%v", run.SpentBudgetUSD, run.ReservedBudgetUSD)
	}
	ts = e.tasks(t, runID)
	for k, tk := range ts {
		if tk.State != runtime.TaskSucceeded || tk.AttemptCount != 1 || tk.Result["schema_version"] != "1.0" || tk.Result["status"] != "SUCCEEDED" {
			t.Fatalf("task %s: %+v", k, tk)
		}
	}
	if u := ts["d"].Result["usage"].(map[string]any); u["model_calls"] != 2.0 || u["tool_calls"] != 1.0 {
		t.Fatalf("task d usage: %v", u)
	}
	if n := dbCount(t, e.pool, `SELECT count(*) FROM task_attempts a JOIN agent_tasks t ON t.id = a.task_id WHERE t.run_id = $1 AND a.state = 'SUCCEEDED'`, runID); n != 4 {
		t.Fatalf("expected 4 succeeded attempts, got %d", n)
	}
	if n := dbCount(t, e.pool, `SELECT count(*) FROM task_checkpoints c JOIN agent_tasks t ON t.id = c.task_id WHERE t.run_id = $1 AND c.kind = 'RESULT'`, runID); n != 4 {
		t.Fatalf("expected 4 RESULT checkpoints, got %d", n)
	}
	for _, typ := range []string{"run.plan_accepted", "task.ready", "task.leased", "task.succeeded"} {
		if n := dbCount(t, e.pool, `SELECT count(*) FROM outbox_events WHERE run_id = $1 AND event_type = $2`, runID, typ); n == 0 {
			t.Fatalf("missing %s events", typ)
		}
	}

	t.Run("relay publishes every event exactly once", func(t *testing.T) { testRelay(t, e, runID) })
}

type capturePublisher struct {
	mu       sync.Mutex
	subjects []string
	ids      map[string]int
	failNext bool
}

func (c *capturePublisher) Publish(_ context.Context, subject, msgID string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failNext {
		c.failNext = false
		return errors.New("broker unavailable")
	}
	var w outbox.WireEnvelope
	if err := json.Unmarshal(data, &w); err != nil || w.EventID != msgID || strings.Contains(string(data), `"seq"`) {
		return fmt.Errorf("bad wire envelope: %s", data)
	}
	c.subjects = append(c.subjects, subject)
	c.ids[msgID]++
	return nil
}

func testRelay(t *testing.T, e *s1Env, runID string) {
	ctx := context.Background()
	pub := &capturePublisher{ids: map[string]int{}, failNext: true}
	relay := outbox.NewRelay(e.pool, pub, e.logger, outbox.RelayOptions{BatchSize: 50})

	// A broker failure leaves the event unpublished with the error recorded.
	if _, err := relay.RunOnce(ctx); err == nil {
		t.Fatal("expected publish error to surface")
	}
	if n := dbCount(t, e.pool, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL AND last_error IS NOT NULL`); n == 0 {
		t.Fatal("failed publish was not recorded")
	}
	for {
		n, err := relay.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if lag, _ := relay.Lag(ctx); lag != 0 {
		t.Fatalf("relay lag %d", lag)
	}
	runEvents := dbCount(t, e.pool, `SELECT count(*) FROM outbox_events WHERE run_id = $1`, runID)
	got := 0
	for _, s := range pub.subjects {
		if strings.HasPrefix(s, "systera.events.task.") || strings.HasPrefix(s, "systera.events.run.") {
			got++
		}
	}
	if got < runEvents {
		t.Fatalf("published %d run/task events, outbox has %d for the run", got, runEvents)
	}
	for id, n := range pub.ids {
		if n != 1 {
			t.Fatalf("event %s published %d times", id, n)
		}
	}

	natsURL := os.Getenv("SYSTERA_TEST_NATS_URL")
	if natsURL == "" {
		t.Log("SYSTERA_TEST_NATS_URL not set; skipping JetStream round-trip")
		return
	}
	stream := "SYSTERA_IT_" + strings.ReplaceAll(runID[:8], "-", "")
	prefix := "systera.it" + runID[:8]
	bus, err := natsbus.Connect(ctx, natsURL, "systera-it", natsbus.StreamConfig{Name: stream, SubjectPrefix: prefix}, e.logger)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	defer func() { _ = bus.JetStream().DeleteStream(context.Background(), stream) }()
	if _, err := e.pool.Exec(ctx, `UPDATE outbox_events SET published_at = NULL WHERE run_id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	natsRelay := outbox.NewRelay(e.pool, bus, e.logger, outbox.RelayOptions{SubjectPrefix: prefix, BatchSize: 500})
	if _, err := natsRelay.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// Re-relaying the same events (e.g. after a crash before commit) is de-duplicated by event_id.
	if _, err := e.pool.Exec(ctx, `UPDATE outbox_events SET published_at = NULL WHERE run_id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := natsRelay.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := bus.JetStream().Stream(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if int(info.State.Msgs) != runEvents {
		t.Fatalf("JetStream holds %d messages, want %d (dedup by Nats-Msg-Id)", info.State.Msgs, runEvents)
	}
	msg, err := s.GetLastMsgForSubject(ctx, prefix+".run.state_changed")
	if err != nil {
		t.Fatal(err)
	}
	var w outbox.WireEnvelope
	if err := json.Unmarshal(msg.Data, &w); err != nil || w.RunID == nil || *w.RunID != runID || w.Payload["to"] != "EVALUATING" {
		t.Fatalf("last run.state_changed on JetStream: %s", msg.Data)
	}
}

func TestStage1LeaseExpiryReclaimAndFencing(t *testing.T) {
	e := newS1Env(t)
	ctx := context.Background()
	runID := e.newRun(t, 5)
	solo := task("solo")
	solo["max_attempts"] = 2
	expect(t, e.plan(t, runID, solo), http.StatusCreated, "plan")

	claim := func(worker string) *runtime.Lease {
		l, err := e.store.Claim(ctx, scheduler.ClaimRequest{WorkerID: worker, AgentTypes: []string{"requirements_analyst"}, LeaseTTL: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return l
	}

	first := claim("dead-worker")
	if first == nil || first.AttemptNumber != 1 || first.LastCheckpoint != nil {
		t.Fatalf("first lease: %+v", first)
	}
	if again := claim("other"); again != nil {
		t.Fatal("a leased task must not be claimable")
	}
	if err := e.store.Checkpoint(ctx, first, map[string]any{"step": "half-way"}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Heartbeat(ctx, first, time.Second); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1200 * time.Millisecond) // worker "dies": no more heartbeats
	if n, err := e.store.ReclaimExpired(ctx, 10); err != nil || n != 1 {
		t.Fatalf("reclaim: n=%d err=%v", n, err)
	}
	tk := e.tasks(t, runID)["solo"]
	if tk.State != runtime.TaskReady || tk.AttemptCount != 1 || !strings.Contains(tk.LastError, "lease expired") {
		t.Fatalf("after reclaim: %+v", tk)
	}
	if r := e.run(t, runID); r.ReservedBudgetUSD != 0 {
		t.Fatalf("reservation not released: %v", r.ReservedBudgetUSD)
	}

	// The stale holder is fenced off.
	if err := e.store.Heartbeat(ctx, first, time.Second); !errors.Is(err, scheduler.ErrLeaseLost) {
		t.Fatalf("stale heartbeat: %v", err)
	}
	if err := e.store.Complete(ctx, first, map[string]any{}, runtime.Usage{}); !errors.Is(err, scheduler.ErrLeaseLost) {
		t.Fatalf("stale complete: %v", err)
	}
	if err := e.store.Checkpoint(ctx, first, map[string]any{}); !errors.Is(err, scheduler.ErrLeaseLost) {
		t.Fatalf("stale checkpoint: %v", err)
	}

	// Retry is delayed by backoff, then resumes with the earlier checkpoint.
	if l := claim("w2"); l != nil {
		t.Fatal("task must not be claimable during retry backoff")
	}
	var second *runtime.Lease
	waitFor(t, 5*time.Second, func() bool { second = claim("w2"); return second != nil }, "retry to become claimable")
	if second.AttemptNumber != 2 || second.LastCheckpoint == nil || second.LastCheckpoint.State["step"] != "half-way" {
		t.Fatalf("second lease: %+v / %+v", second, second.LastCheckpoint)
	}

	time.Sleep(1200 * time.Millisecond)
	if n, err := e.store.ReclaimExpired(ctx, 10); err != nil || n != 1 {
		t.Fatalf("second reclaim: n=%d err=%v", n, err)
	}
	if tk := e.tasks(t, runID)["solo"]; tk.State != runtime.TaskFailed {
		t.Fatalf("attempts exhausted: task should be FAILED, got %s", tk.State)
	}
	run := e.run(t, runID)
	if run.State != runs.StateFailed || run.FailureReason != "task_failed:solo" {
		t.Fatalf("run: %s %q", run.State, run.FailureReason)
	}
	if n := dbCount(t, e.pool, `SELECT count(*) FROM task_attempts WHERE task_id = $1 AND state = 'EXPIRED'`, first.Task.ID); n != 2 {
		t.Fatalf("expected 2 EXPIRED attempts, got %d", n)
	}
}

func TestStage1BudgetEnforcement(t *testing.T) {
	e := newS1Env(t)
	ctx := context.Background()
	claim := func() *runtime.Lease {
		l, err := e.store.Claim(ctx, scheduler.ClaimRequest{WorkerID: "w", AgentTypes: []string{"requirements_analyst"}, LeaseTTL: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		return l
	}

	t.Run("reservations block over-commitment and starved runs fail", func(t *testing.T) {
		runID := e.newRun(t, 1.0)
		x, y := task("x"), task("y")
		x["budget_usd"], y["budget_usd"] = 0.6, 0.6
		expect(t, e.plan(t, runID, x, y), http.StatusCreated, "plan")

		first := claim()
		if first == nil {
			t.Fatal("first task should be claimable")
		}
		if r := e.run(t, runID); r.ReservedBudgetUSD != 0.6 {
			t.Fatalf("reserved %v", r.ReservedBudgetUSD)
		}
		if l := claim(); l != nil {
			t.Fatal("second task must not be claimable: 0.6 reserved + 0.6 > 1.0")
		}
		if err := e.store.Complete(ctx, first, map[string]any{"ok": true}, runtime.Usage{CostUSD: 0.5}); err != nil {
			t.Fatal(err)
		}
		if l := claim(); l != nil {
			t.Fatal("0.5 spent + 0.6 > 1.0: still not claimable")
		}
		if n, err := e.store.FailStarvedRuns(ctx); err != nil || n < 1 {
			t.Fatalf("starved runs: n=%d err=%v", n, err)
		}
		run := e.run(t, runID)
		if run.State != runs.StateFailed || run.FailureReason != scheduler.FailureReasonBudgetExhausted || run.SpentBudgetUSD != 0.5 {
			t.Fatalf("run: %+v", run)
		}
		left := first.Task.Key
		for k, tk := range e.tasks(t, runID) {
			if k != left && tk.State != runtime.TaskCancelled {
				t.Fatalf("remaining task %s should be CANCELLED, got %s", k, tk.State)
			}
		}
	})

	t.Run("actual overspend fails the run", func(t *testing.T) {
		runID := e.newRun(t, 1.0)
		z := task("z")
		z["budget_usd"] = 1.0
		expect(t, e.plan(t, runID, z, task("after", "z")), http.StatusCreated, "plan")
		l := claim()
		if err := e.store.Complete(ctx, l, map[string]any{}, runtime.Usage{CostUSD: 1.5}); err != nil {
			t.Fatal(err)
		}
		run := e.run(t, runID)
		if run.State != runs.StateFailed || run.FailureReason != scheduler.FailureReasonBudgetExceeded {
			t.Fatalf("run: %s %q", run.State, run.FailureReason)
		}
		if tk := e.tasks(t, runID)["after"]; tk.State != runtime.TaskCancelled {
			t.Fatalf("downstream task: %s", tk.State)
		}
	})

	t.Run("gateway refuses calls the task budget cannot cover", func(t *testing.T) {
		runID := e.newRun(t, 1.0)
		tiny := task("tiny")
		tiny["budget_usd"] = 0.000001
		expect(t, e.plan(t, runID, tiny), http.StatusCreated, "plan")
		called := false
		runner := e.newRunner(t, "w", models.FuncProvider(func(context.Context, models.Request) (models.Response, error) {
			called = true
			return usageResp(final("x", map[string]any{})), nil
		}), time.Minute)
		if _, err := runner.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if called {
			t.Fatal("provider must not be called when the budget cannot cover a minimal completion")
		}
		tk := e.tasks(t, runID)["tiny"]
		if tk.State != runtime.TaskFailed || !strings.Contains(tk.LastError, "budget") {
			t.Fatalf("task: %s %q", tk.State, tk.LastError)
		}
	})
}

func TestStage1CancelAndFailureClassification(t *testing.T) {
	e := newS1Env(t)
	ctx := context.Background()

	t.Run("cancelling a run fences off in-flight leases", func(t *testing.T) {
		runID := e.newRun(t, 5)
		expect(t, e.plan(t, runID, task("one"), task("two", "one")), http.StatusCreated, "plan")
		l, err := e.store.Claim(ctx, scheduler.ClaimRequest{WorkerID: "w", AgentTypes: []string{"requirements_analyst"}, LeaseTTL: time.Minute})
		if err != nil || l == nil {
			t.Fatalf("claim: %v", err)
		}
		expect(t, e.do(http.MethodPost, "/v1/runs/"+runID+"/cancel", e.token, "", nil), http.StatusOK, "cancel")
		if err := e.store.Complete(ctx, l, map[string]any{}, runtime.Usage{}); !errors.Is(err, scheduler.ErrLeaseLost) {
			t.Fatalf("complete after cancel: %v", err)
		}
		for k, tk := range e.tasks(t, runID) {
			if tk.State != runtime.TaskCancelled {
				t.Fatalf("task %s: %s", k, tk.State)
			}
		}
		if r := e.run(t, runID); r.ReservedBudgetUSD != 0 || r.State != runs.StateCancelled {
			t.Fatalf("run: %+v", r)
		}
	})

	t.Run("malformed agent output is retried; provider 4xx is not", func(t *testing.T) {
		provider := models.FuncProvider(func(_ context.Context, req models.Request) (models.Response, error) {
			if strings.HasSuffix(envelopeOf(req.Messages).Objective, "broken") {
				return models.Response{}, &models.ProviderError{StatusCode: 400, Message: "bad request"}
			}
			return usageResp("this is not json"), nil
		})
		runner := e.newRunner(t, "w", provider, time.Minute)

		flakyRun := e.newRun(t, 5)
		expect(t, e.plan(t, flakyRun, task("flaky")), http.StatusCreated, "plan flaky")
		if _, err := runner.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		f := e.tasks(t, flakyRun)["flaky"]
		if f.State != runtime.TaskReady || f.AttemptCount != 1 || !strings.Contains(f.LastError, "invalid response twice") {
			t.Fatalf("flaky should be scheduled for retry: %s attempts=%d err=%q", f.State, f.AttemptCount, f.LastError)
		}
		assertFailedResult(t, f, "invalid_agent_response")
		if run := e.run(t, flakyRun); run.State != runs.StateRunning || run.SpentBudgetUSD < 0.039 {
			t.Fatalf("flaky run should keep running and be charged for both calls: %+v", run)
		}
		expect(t, e.do(http.MethodPost, "/v1/runs/"+flakyRun+"/cancel", e.token, "", nil), http.StatusOK, "cancel flaky run")

		brokenRun := e.newRun(t, 5)
		expect(t, e.plan(t, brokenRun, task("broken")), http.StatusCreated, "plan broken")
		if _, err := runner.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if b := e.tasks(t, brokenRun)["broken"]; b.State != runtime.TaskFailed || b.AttemptCount != 1 || !strings.Contains(b.LastError, "status 400") {
			t.Fatalf("broken: %s attempts=%d %q", b.State, b.AttemptCount, b.LastError)
		}
		assertFailedResult(t, e.tasks(t, brokenRun)["broken"], "provider_error")
		if run := e.run(t, brokenRun); run.State != runs.StateFailed || run.FailureReason != "task_failed:broken" {
			t.Fatalf("run: %s %q", run.State, run.FailureReason)
		}
	})

	t.Run("an agent-declared failure keeps the agent's full result", func(t *testing.T) {
		runner := e.newRunner(t, "w", models.FuncProvider(func(context.Context, models.Request) (models.Response, error) {
			return usageResp(`{"action":"final","result":{"status":"FAILED","summary":"Cannot size storage without retention policy",
				"output":{"partial":{"read_write_ratio":"100:1"}},"open_questions":["How long are links retained?"],
				"error":{"code":"missing_input","message":"retention policy unknown"}}}`), nil
		}), time.Minute)
		runID := e.newRun(t, 5)
		expect(t, e.plan(t, runID, task("declared")), http.StatusCreated, "plan")
		if _, err := runner.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		tk := e.tasks(t, runID)["declared"]
		if tk.State != runtime.TaskFailed || tk.AttemptCount != 1 {
			t.Fatalf("declared failure must be final: %s attempts=%d", tk.State, tk.AttemptCount)
		}
		assertFailedResult(t, tk, "missing_input")
		if tk.Result["summary"] != "Cannot size storage without retention policy" ||
			tk.Result["output"].(map[string]any)["partial"] == nil || len(tk.Result["open_questions"].([]any)) != 1 {
			t.Fatalf("agent's own result not preserved: %v", tk.Result)
		}
		if n := dbCount(t, e.pool, `SELECT count(*) FROM task_checkpoints WHERE task_id = $1 AND kind = 'RESULT'`, tk.ID); n != 1 {
			t.Fatalf("expected a RESULT checkpoint for the failed attempt, got %d", n)
		}
	})
}

// assertFailedResult checks a task carries a contract-shaped FAILED AgentResult.
func assertFailedResult(t *testing.T, tk runtime.Task, code string) {
	t.Helper()
	r := tk.Result
	errObj, _ := r["error"].(map[string]any)
	usage, _ := r["usage"].(map[string]any)
	if r["schema_version"] != "1.0" || r["status"] != "FAILED" || r["task_id"] != tk.ID || r["attempt"] != float64(tk.AttemptCount) ||
		r["summary"] == "" || errObj["code"] != code || usage == nil {
		t.Fatalf("task %s: expected FAILED AgentResult with code %q, got %v", tk.Key, code, r)
	}
}

type orderingPublisher struct {
	mu   sync.Mutex
	runs map[string][]int // run_id -> payload "i" values in arrival order
}

func (o *orderingPublisher) Publish(_ context.Context, _, _ string, data []byte) error {
	var w outbox.WireEnvelope
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	if w.EventType == "test.ordering" && w.RunID != nil {
		time.Sleep(time.Duration(len(data)%3) * time.Millisecond) // widen interleavings
		o.mu.Lock()
		o.runs[*w.RunID] = append(o.runs[*w.RunID], int(w.Payload["i"].(float64)))
		o.mu.Unlock()
	}
	return nil
}

// Several relay replicas racing over the outbox must deliver each run's events exactly
// once and in commit order.
func TestStage1RelayReplicasPreservePerRunOrder(t *testing.T) {
	e := newS1Env(t)
	ctx := context.Background()
	const runCount, perRun = 8, 60
	tenant := e.tenantID
	runIDs := make([]string, runCount)
	for i := range runIDs {
		runIDs[i] = ids.New()
	}
	// One statement: seq increases with i, events of all runs interleaved.
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO outbox_events (event_type, tenant_id, run_id, correlation_id, payload)
		SELECT 'test.ordering', $1, ($2::uuid[])[1 + (g % $3)], gen_random_uuid(), jsonb_build_object('i', g)
		FROM generate_series(0, $4 - 1) AS g`, tenant, runIDs, runCount, runCount*perRun); err != nil {
		t.Fatal(err)
	}

	pub := &orderingPublisher{runs: map[string][]int{}}
	var wg sync.WaitGroup
	deadline := time.Now().Add(30 * time.Second)
	for r := 0; r < 4; r++ {
		relay := outbox.NewRelay(e.pool, pub, e.logger, outbox.RelayOptions{BatchSize: 7, Partitions: 4})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				if _, err := relay.RunOnce(ctx); err != nil {
					t.Errorf("relay: %v", err)
					return
				}
				if dbCount(t, e.pool, `SELECT count(*) FROM outbox_events WHERE event_type = 'test.ordering' AND run_id = ANY($1) AND published_at IS NULL`, runIDs) == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()

	total := 0
	for _, id := range runIDs {
		got := pub.runs[id]
		total += len(got)
		for k := 1; k < len(got); k++ {
			if got[k] <= got[k-1] {
				t.Fatalf("run %s delivered out of order or twice: %v", id[:8], got)
			}
		}
	}
	if total != runCount*perRun {
		t.Fatalf("delivered %d events, want %d", total, runCount*perRun)
	}
}
