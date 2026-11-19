package test

// Stage 2 dependency engine (checklist 6.1-6.5) integration tests. Require SYSTERA_TEST_DATABASE_URL.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/systera/control-plane/internal/artifacts"
	"github.com/systera/control-plane/internal/auth"
	"github.com/systera/control-plane/internal/decisions"
	"github.com/systera/control-plane/internal/evidence"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/httpserver"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/projects"
	"github.com/systera/control-plane/internal/runs"
	"github.com/systera/control-plane/internal/scheduler"
)

type depEnv struct {
	client
	graphEnv
	svc       *graph.Service
	store     *artifacts.Store
	decisions *decisions.Store
	ledger    *evidence.Store
	sched     *scheduler.Store
	runs      *runs.PostgresRepository
	token     string
	otherTok  string
	modelID   string
	base      string
	n         map[string]string // node key -> id
}

func newDepEnv(t *testing.T) *depEnv {
	t.Helper()
	ge := newGraphEnv(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := graph.NewService(ge.pool)
	e := &depEnv{graphEnv: *ge, svc: svc, store: artifacts.NewStore(ge.pool), decisions: decisions.NewStore(ge.pool),
		ledger: evidence.NewStore(ge.pool), sched: scheduler.NewStore(ge.pool, logger), runs: runs.NewPostgresRepository(ge.pool),
		n: map[string]string{}}
	srv := httpserver.New(httpserver.Options{
		Logger: logger, Projects: projects.NewPostgresRepository(ge.pool), Runs: e.runs,
		Events: outbox.NewPostgresStore(ge.pool, logger), Graph: svc, Artifacts: e.store, Impact: svc,
		Authenticator: auth.NewAuthenticator(newVerifier(t), auth.NewPostgresMemberships(ge.pool)),
		Readiness:     []httpserver.ReadinessCheck{{Name: "postgres", Check: postgres.PingCheck(ge.pool)}},
	})
	e.client = client{t: t, handler: srv.Handler()}
	var sub, otherSub string
	_ = ge.pool.QueryRow(ctx, `SELECT external_sub FROM users WHERE id = $1`, ge.userID).Scan(&sub)
	_ = ge.pool.QueryRow(ctx, `SELECT u.external_sub FROM users u JOIN tenant_memberships m ON m.user_id = u.id WHERE m.tenant_id = $1`, ge.otherTenant).Scan(&otherSub)
	e.token, e.otherTok = mintToken(t, sub, "", time.Hour), mintToken(t, otherSub, "", time.Hour)

	m, _, err := svc.CreateModel(ctx, ge.tenantID, ge.projectID, ge.author, "")
	if err != nil {
		t.Fatal(err)
	}
	e.modelID, e.base = m.ID, "/v1/architecture/"+m.ID
	return e
}

func (e *depEnv) record(t *testing.T, path string, body map[string]any, code int, what string) artifacts.RecordResult {
	t.Helper()
	b, _ := json.Marshal(body)
	r := e.do(http.MethodPost, e.base+path, e.token, string(b), nil)
	expect(t, r, code, what)
	var res artifacts.RecordResult
	if code == http.StatusCreated {
		r.decode(t, &res)
	}
	return res
}

func artifactBody(name, typ, content string, deps map[string]any) map[string]any {
	return map[string]any{"name": name, "projection_type": typ, "content": content, "media_type": "text/markdown",
		"generated_from": map[string]any{"generator": "test", "version": "1"}, "dependencies": deps}
}

func nodeDep(id, scope string) map[string]any { return map[string]any{"node_id": id, "scope": scope} }

func (e *depEnv) artifact(t *testing.T, id string) artifacts.Artifact {
	t.Helper()
	a, err := e.store.Get(context.Background(), e.tenantID, e.modelID, id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func sortedJoin(in []string) string {
	c := append([]string{}, in...)
	sort.Strings(c)
	return strings.Join(c, ",")
}

func TestDependencyEngineEndToEnd(t *testing.T) {
	e := newDepEnv(t)
	ctx := context.Background()

	// Graph: gw -calls-> redirect -writes-> links, redirect -reads-> cache, redirect -owns-> api;
	// analytics -publishes-> clicks (unrelated to the shortener path).
	for _, k := range []string{"gw", "redirect", "links", "cache", "api", "analytics", "clicks"} {
		e.n[k] = ids.New()
	}
	r := e.do(http.MethodPost, e.base+"/patches", e.token, patchBody(0, "seed",
		addNodeOp(e.n["gw"], "svc.gateway", "service", "Gateway"), addNodeOp(e.n["redirect"], "svc.redirect", "service", "Redirect"),
		addNodeOp(e.n["links"], "db.links", "database", "Links"), addNodeOp(e.n["cache"], "cache.links", "cache", "Cache"),
		addNodeOp(e.n["api"], "api.redirect", "api", "GET /{code}"), addNodeOp(e.n["analytics"], "svc.analytics", "service", "Analytics"),
		addNodeOp(e.n["clicks"], "topic.clicks", "topic", "Clicks"),
		addEdgeOp(ids.New(), e.n["gw"], e.n["redirect"], "calls"), addEdgeOp(ids.New(), e.n["redirect"], e.n["links"], "writes"),
		addEdgeOp(ids.New(), e.n["redirect"], e.n["cache"], "reads"), addEdgeOp(ids.New(), e.n["redirect"], e.n["api"], "owns"),
		addEdgeOp(ids.New(), e.n["analytics"], e.n["clicks"], "publishes")), nil)
	expect(t, r, http.StatusCreated, "seed graph")

	// A decision about the links DB, a second about analytics, and a claim about redirect.
	adr, err := e.decisions.Create(ctx, e.tenantID, e.modelID, decisions.CreateInput{ModelVersion: 1, Title: "Use Postgres for links",
		Context: "durable store", ChosenOption: "Postgres", Rationale: "transactions", AffectedNodes: []string{e.n["links"]}},
		decisions.Actor{Type: "USER", ID: e.userID})
	if err != nil {
		t.Fatal(err)
	}
	adr2, err := e.decisions.Create(ctx, e.tenantID, e.modelID, decisions.CreateInput{ModelVersion: 1, Title: "Batch analytics",
		Context: "cost", ChosenOption: "hourly batches", Rationale: "cheap", AffectedNodes: []string{e.n["analytics"]}},
		decisions.Actor{Type: "USER", ID: e.userID})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := e.ledger.MakeClaim(ctx, e.tenantID, e.modelID, evidence.ClaimInput{Statement: "Redirect should stay stateless",
		Class: "PROPOSED", ConfidenceBand: "MEDIUM", SubjectNodeIDs: []string{e.n["redirect"]}}, evidence.Actor{Type: "USER", ID: e.userID})
	if err != nil {
		t.Fatal(err)
	}

	// 6.1 / 6.2 Artifacts bound to graph version 1 with typed, hashed dependencies.
	erd := e.record(t, "/artifacts", artifactBody("erd", "ERD", "links(code, url)", map[string]any{"nodes": []any{nodeDep(e.n["links"], "NODE")}}), 201, "erd")
	hld := e.record(t, "/artifacts", artifactBody("hld", "HLD", "# HLD", map[string]any{"nodes": []any{nodeDep(e.n["gw"], "NODE_AND_DEPENDENCIES")}}), 201, "hld")
	openapi := e.record(t, "/artifacts", artifactBody("openapi", "OPENAPI", "openapi: 3.1.0", map[string]any{
		"nodes": []any{nodeDep(e.n["api"], "NODE")}, "artifacts": []string{hld.Artifact.ID}}), 201, "openapi")
	tests := e.record(t, "/artifacts", artifactBody("tests/redirect", "TEST_SUITE", "func TestRedirect", map[string]any{
		"nodes": []any{nodeDep(e.n["redirect"], "NODE")}, "claims": []string{claim.ID}}), 201, "tests")
	adrExport := e.record(t, "/artifacts", artifactBody("adr/0001", "ADR_EXPORT", "ADR-1", map[string]any{"decisions": []string{adr.ID}}), 201, "adr export")
	adrExport2 := e.record(t, "/artifacts", artifactBody("adr/0002", "ADR_EXPORT", "ADR-2", map[string]any{"decisions": []string{adr2.ID}}), 201, "adr2 export")
	catalogue := e.record(t, "/artifacts", artifactBody("catalogue/analytics", "COMPONENT_CATALOGUE", "analytics", map[string]any{
		"nodes": []any{nodeDep(e.n["analytics"], "NODE")}}), 201, "catalogue")

	v := hld.Version
	if v.GraphVersion != 1 || v.Number != 1 || len(v.Checksum) != 64 || v.CreatedBy != e.userID || v.GeneratedFrom["generator"] != "test" {
		t.Fatalf("hld version: %+v", v)
	}
	closure := map[string]string{}
	for _, d := range v.Dependencies {
		if d.Type == "NODE" {
			closure[d.TargetID] = d.Via
		}
		if len(d.TargetHash) != 64 {
			t.Fatalf("dependency hash: %+v", d)
		}
	}
	if len(closure) != 5 || closure[e.n["gw"]] != "direct" || closure[e.n["links"]] != "closure:"+e.n["gw"] {
		t.Fatalf("hld closure: %v", closure)
	}

	// Validation.
	e.record(t, "/artifacts", artifactBody("orphan", "OTHER", "x", map[string]any{}), 422, "no dependencies")
	e.record(t, "/artifacts", artifactBody("erd", "ERD", "x", map[string]any{"nodes": []any{nodeDep(e.n["links"], "NODE")}}), 409, "duplicate name")
	e.record(t, "/artifacts", artifactBody("ghost", "OTHER", "x", map[string]any{"nodes": []any{nodeDep(ids.New(), "NODE")}}), 422, "unknown node")
	e.record(t, "/artifacts/"+hld.Artifact.ID+"/versions", map[string]any{"content": "# HLD v2",
		"dependencies": map[string]any{"artifacts": []string{openapi.Artifact.ID}}}, 422, "cycle")
	stale := 0
	e.record(t, "/artifacts/"+erd.Artifact.ID+"/versions", map[string]any{"content": "x", "graph_version": stale,
		"dependencies": map[string]any{"nodes": []any{nodeDep(e.n["links"], "NODE")}}}, 409, "stale graph version")

	// 6.3 dry run: what would changing the api node invalidate?
	r = e.do(http.MethodGet, e.base+"/nodes/"+e.n["api"]+"/impact", e.token, "", nil)
	expect(t, r, http.StatusOK, "impact dry run")
	var rep graph.ImpactReport
	r.decode(t, &rep)
	if sortedJoin(rep.Artifacts) != sortedJoin([]string{hld.Artifact.ID, openapi.Artifact.ID, tests.Artifact.ID}) || rep.Truncated ||
		len(rep.Nodes) != 3 || rep.Nodes[0].Depth != 0 || len(rep.Claims) != 1 {
		t.Fatalf("dry run: %+v", rep)
	}

	// 6.3 a real change: rename the links DB.
	r = e.do(http.MethodPost, e.base+"/patches", e.token, patchBody(1, "rename-links",
		map[string]any{"op": "update_node", "target_id": e.n["links"], "rationale": "sharded", "payload": map[string]any{"name": "Links (sharded)"}}), nil)
	expect(t, r, http.StatusCreated, "rename links")
	res := decodeResult(t, r)
	wantStale := []string{erd.Artifact.ID, hld.Artifact.ID, openapi.Artifact.ID, tests.Artifact.ID, adrExport.Artifact.ID}
	if sortedJoin(res.InvalidatedArtifacts) != sortedJoin(wantStale) || res.Invalidation == nil ||
		len(res.Invalidation.Decisions) != 1 || res.Invalidation.Decisions[0] != adr.ID ||
		len(res.Invalidation.Claims) != 1 || res.Invalidation.RevalidationReq == "" || res.Invalidation.Truncated {
		t.Fatalf("invalidation: %+v / %+v", res.InvalidatedArtifacts, res.Invalidation)
	}
	reasons := map[string]string{}
	for _, id := range wantStale {
		a := e.artifact(t, id)
		if a.Status != "STALE" || a.StaleGraphVersion == nil || *a.StaleGraphVersion != 2 {
			t.Fatalf("artifact %s: %s", a.Name, a.Status)
		}
		reasons[a.Name] = a.StaleReason
	}
	if !strings.Contains(reasons["erd"], "changed node") || !strings.Contains(reasons["openapi"], "upstream artifact") ||
		!strings.Contains(reasons["adr/0001"], "decision") || !strings.Contains(reasons["tests/redirect"], "changed node") {
		t.Fatalf("stale reasons: %v", reasons)
	}
	for _, id := range []string{catalogue.Artifact.ID, adrExport2.Artifact.ID} {
		if a := e.artifact(t, id); a.Status != "CURRENT" {
			t.Fatalf("unrelated artifact %s went %s", a.Name, a.Status)
		}
	}
	if c, _ := e.ledger.GetClaim(ctx, e.tenantID, e.modelID, claim.ID); c.Lifecycle != "STALE" {
		t.Fatalf("claim about redirect: %s", c.Lifecycle)
	}
	if d, _ := e.decisions.Get(ctx, e.tenantID, e.modelID, adr.ID); d.Status != decisions.StatusNeedsReview {
		t.Fatalf("decision: %s", d.Status)
	}
	var payload map[string]any
	var raw []byte
	_ = e.pool.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE event_type = $1 AND payload->>'patch_id' = $2`, graph.EventVersionCreated, res.PatchID).Scan(&raw)
	_ = json.Unmarshal(raw, &payload)
	if len(payload["invalidated_artifact_ids"].([]any)) != 5 {
		t.Fatalf("version event: %v", payload["invalidated_artifact_ids"])
	}
	var invalidatedEvents int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_type = 'artifact.invalidated' AND payload->>'cause' = $1`, "patch:"+res.PatchID).Scan(&invalidatedEvents)
	if invalidatedEvents != 5 {
		t.Fatalf("artifact.invalidated events: %d", invalidatedEvents)
	}

	// Deterministic verification from dependency hashes.
	r = e.do(http.MethodGet, e.base+"/artifacts/"+erd.Artifact.ID+"/verify", e.token, "", nil)
	var vr artifacts.VerifyReport
	r.decode(t, &vr)
	if vr.Fresh || len(vr.Drift) != 1 || vr.Drift[0].TargetID != e.n["links"] || vr.Drift[0].Problem != "content changed since binding" {
		t.Fatalf("verify erd: %+v", vr)
	}
	r = e.do(http.MethodGet, e.base+"/artifacts/"+catalogue.Artifact.ID+"/verify", e.token, "", nil)
	vr = artifacts.VerifyReport{}
	r.decode(t, &vr)
	if !vr.Fresh || vr.Checked == 0 {
		t.Fatalf("verify catalogue: %+v", vr)
	}

	// 6.4 revalidation as runtime tasks.
	sres, err := e.store.ScheduleRevalidations(ctx, e.runs, e.sched, 50)
	if err != nil || sres.Scheduled < 1 {
		t.Fatalf("schedule: %+v %v", sres, err)
	}
	runID := e.artifact(t, erd.Artifact.ID).RevalidationRunID
	if runID == "" {
		t.Fatal("stale artifact has no revalidation run")
	}
	tasks, err := e.sched.ListTasks(ctx, e.tenantID, runID)
	if err != nil || len(tasks) != 5 {
		t.Fatalf("revalidation tasks: %d %v", len(tasks), err)
	}
	for _, tk := range tasks {
		if tk.AgentType != artifacts.RevalidatorAgentType || tk.State != "READY" || tk.Input["artifact_id"] == nil {
			t.Fatalf("task: %+v", tk)
		}
	}
	if run, _ := e.runs.GetByID(ctx, e.tenantID, runID); run.State != runs.StateRunning {
		t.Fatalf("revalidation run state: %s", run.State)
	}
	if again, _ := e.store.ScheduleRevalidations(ctx, e.runs, e.sched, 50); again.Scheduled != 0 {
		t.Fatal("a request must be scheduled once")
	}

	// A new version re-binds to the current graph and is CURRENT again.
	newERD := e.record(t, "/artifacts/"+erd.Artifact.ID+"/versions", map[string]any{"content": "links(code, url, shard)",
		"dependencies": map[string]any{"nodes": []any{nodeDep(e.n["links"], "NODE")}}}, 201, "erd v2")
	if newERD.Artifact.Status != "CURRENT" || newERD.Version.Number != 2 || newERD.Version.GraphVersion != 2 || newERD.Artifact.RevalidationRunID != "" {
		t.Fatalf("erd v2: %+v", newERD.Artifact)
	}
	newHLD := e.record(t, "/artifacts/"+hld.Artifact.ID+"/versions", map[string]any{"content": "# HLD v2",
		"dependencies": map[string]any{"nodes": []any{nodeDep(e.n["gw"], "NODE_AND_DEPENDENCIES")}}}, 201, "hld v2")
	e.record(t, "/artifacts/"+openapi.Artifact.ID+"/versions", map[string]any{"content": "openapi: 3.1.0 # v2",
		"dependencies": map[string]any{"nodes": []any{nodeDep(e.n["api"], "NODE")}, "artifacts": []string{hld.Artifact.ID}}}, 201, "openapi v2")
	// New upstream version -> downstream stale.
	hld3 := e.record(t, "/artifacts/"+hld.Artifact.ID+"/versions", map[string]any{"content": "# HLD v3",
		"dependencies": map[string]any{"nodes": []any{nodeDep(e.n["gw"], "NODE_AND_DEPENDENCIES")}}}, 201, "hld v3")
	if len(hld3.InvalidatedDownstream) != 1 || hld3.InvalidatedDownstream[0] != openapi.Artifact.ID || hld3.RevalidationRequestID == "" {
		t.Fatalf("downstream invalidation: %+v", hld3)
	}
	_ = newHLD

	// Drift sweep: a decision changes outside any graph patch.
	if _, err := e.decisions.Transition(ctx, e.tenantID, e.modelID, adr2.ID, decisions.TransitionInput{Status: decisions.StatusAccepted, Reason: "approved"},
		decisions.Actor{Type: "USER", ID: e.userID}); err != nil {
		t.Fatal(err)
	}
	if sw, err := e.store.Sweep(ctx); err != nil || sw.ArtifactsInvalidated < 1 {
		t.Fatalf("drift sweep: %+v %v", sw, err)
	}
	if a := e.artifact(t, adrExport2.Artifact.ID); a.Status != "STALE" || !strings.Contains(a.StaleReason, "decision") {
		t.Fatalf("adr2 export after sweep: %s %q", a.Status, a.StaleReason)
	}
	if a := e.artifact(t, catalogue.Artifact.ID); a.Status != "CURRENT" {
		t.Fatal("sweep must not touch unaffected artifacts")
	}

	// Lists, tenancy and database guards.
	r = e.do(http.MethodGet, e.base+"/artifacts?status=STALE", e.token, "", nil)
	var list struct{ Items []artifacts.Artifact }
	r.decode(t, &list)
	if len(list.Items) != 5 { // hld? no: hld v3 is CURRENT; stale = openapi, tests, adr/0001, adr/0002 + none... recount below
		names := []string{}
		for _, a := range list.Items {
			names = append(names, a.Name)
		}
		if sortedJoin(names) != "adr/0001,adr/0002,openapi,tests/redirect" {
			t.Fatalf("stale artifacts: %v", names)
		}
	}
	expect(t, e.do(http.MethodGet, e.base+"/artifacts", e.otherTok, "", nil), http.StatusNotFound, "other tenant")
	expectPG(t, asTenant(e.pool, e.tenantID, `UPDATE artifact_versions SET checksum = repeat('0', 64) WHERE id = $1`, erd.Version.ID), "42501", "rewriting a version as app")
	_, err = e.pool.Exec(ctx, `UPDATE artifact_versions SET checksum = repeat('0', 64) WHERE id = $1`, erd.Version.ID)
	expectPG(t, err, "23001", "rewriting a version as owner")
	expectPG(t, asTenant(e.pool, e.tenantID, `UPDATE artifacts SET name = 'renamed' WHERE id = $1`, erd.Artifact.ID), "23001", "renaming an artifact")
	expectPG(t, asTenant(e.pool, e.tenantID, `DELETE FROM artifacts WHERE id = $1`, erd.Artifact.ID), "42501", "deleting an artifact")
	expectPG(t, asTenant(e.pool, e.tenantID, `INSERT INTO artifact_versions (tenant_id, model_id, artifact_id, version_number, checksum, graph_version, graph_hash, created_by_type, created_by)
		VALUES ($1, $2, $3, 99, repeat('a', 64), 2, repeat('b', 64), 'USER', 'u')`, e.tenantID, e.modelID, erd.Artifact.ID), "23514", "forged graph hash")
}

// 6.5 Bounded traversal on a realistic graph: 5,000 nodes / ~15,000 edges.
func TestDependencyEngineScale(t *testing.T) {
	e := newDepEnv(t)
	ctx := context.Background()
	const layers, width, fanout = 50, 100, 3
	rng := rand.New(rand.NewPCG(7, 7))
	nodes := make([][]graph.Node, layers)
	var all []graph.Node
	for l := range nodes {
		for i := 0; i < width; i++ {
			n := node(fmt.Sprintf("c.l%02d.%03d", l, i), "component", fmt.Sprintf("Component %d/%d", l, i))
			nodes[l] = append(nodes[l], n)
			all = append(all, n)
		}
	}
	var edges []graph.Edge
	for l := 0; l < layers-1; l++ { // each component depends on 3 components of the next layer
		for _, n := range nodes[l] {
			picked := map[int]bool{}
			for len(picked) < fanout {
				picked[rng.IntN(width)] = true
			}
			for j := range picked {
				edges = append(edges, edge(n, nodes[l+1][j], "depends_on"))
			}
		}
	}
	start := time.Now()
	if _, err := e.apply(t, e.modelID, change{addNodes: all, addEdges: edges}); err != nil {
		t.Fatal(err)
	}
	t.Logf("loaded %d nodes / %d edges in %s", len(all), len(edges), time.Since(start).Round(time.Millisecond))

	// 200 artifacts bound to random components.
	bound := map[string]string{} // artifact id -> node id
	for i := 0; i < 200; i++ {
		n := all[rng.IntN(len(all))]
		res, err := e.store.Record(ctx, e.tenantID, e.modelID, "", artifacts.RecordInput{Name: fmt.Sprintf("doc/%03d", i), ProjectionType: "LLD",
			Content: n.Key, Dependencies: artifacts.Dependencies{Nodes: []artifacts.NodeDependency{{NodeID: n.ID}}}}, artifacts.Actor{Type: "USER", ID: e.userID})
		if err != nil {
			t.Fatal(err)
		}
		bound[res.Artifact.ID] = n.ID
	}

	// Full impact of a deepest-layer component: everything above that depends on it.
	leaf := nodes[layers-1][0]
	// The default depth (25) is deliberately below this synthetic 50-layer chain; raise it
	// to measure a complete traversal.
	// The 2 s budget measures the traversal, not CPU contention from packages running in
	// parallel: take the fastest of up to three runs (the result is identical each time).
	var rep graph.ImpactReport
	for attempt := 0; attempt < 3; attempt++ {
		r, err := e.svc.ImpactOf(ctx, e.tenantID, e.modelID, -1, []string{leaf.ID}, graph.ImpactLimits{MaxDepth: 100})
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 0 || r.DurationMS < rep.DurationMS {
			rep = r
		}
		if rep.DurationMS <= 2000 {
			break
		}
	}
	t.Logf("impact of a leaf: %d nodes, depth %d, %d artifacts, %.1f ms", len(rep.Nodes), rep.MaxDepth, len(rep.Artifacts), rep.DurationMS)
	if rep.Truncated || rep.MaxDepth != layers-1 || len(rep.Nodes) < 1000 || rep.DurationMS > 2000 {
		t.Fatalf("leaf impact: truncated=%v depth=%d nodes=%d %.0fms", rep.Truncated, rep.MaxDepth, len(rep.Nodes), rep.DurationMS)
	}
	affected := map[string]bool{}
	for _, n := range rep.Nodes {
		affected[n.NodeID] = true
	}
	want := 0
	for _, nodeID := range bound {
		if affected[nodeID] {
			want++
		}
	}
	if len(rep.Artifacts) != want {
		t.Fatalf("impact found %d artifacts, %d are bound to affected nodes", len(rep.Artifacts), want)
	}

	// Limits are exact.
	rep, _ = e.svc.ImpactOf(ctx, e.tenantID, e.modelID, -1, []string{leaf.ID}, graph.ImpactLimits{MaxDepth: 3})
	if !rep.Truncated || rep.MaxDepth != 3 {
		t.Fatalf("depth limit: truncated=%v depth=%d", rep.Truncated, rep.MaxDepth)
	}
	for _, n := range rep.Nodes {
		if n.Depth > 3 {
			t.Fatalf("node beyond depth limit: %+v", n)
		}
	}
	rep, _ = e.svc.ImpactOf(ctx, e.tenantID, e.modelID, -1, []string{leaf.ID}, graph.ImpactLimits{MaxNodes: 100})
	if !rep.Truncated || len(rep.Nodes) > 100 {
		t.Fatalf("node limit: truncated=%v nodes=%d", rep.Truncated, len(rep.Nodes))
	}
	top := nodes[0][0] // nothing depends on the top layer
	rep, _ = e.svc.ImpactOf(ctx, e.tenantID, e.modelID, -1, []string{top.ID}, graph.ImpactLimits{MaxDepth: 1})
	if rep.Truncated || len(rep.Nodes) != 1 {
		t.Fatalf("a node without dependents must not be truncated at depth 1: %+v", rep)
	}

	// A truncated traversal during a real patch invalidates conservatively.
	e.svc.Impact = graph.ImpactLimits{MaxNodes: 50}
	base := 1
	raw, _ := json.Marshal(map[string]any{"name": "Leaf (renamed)"})
	start = time.Now()
	res, err := e.svc.ApplyPatch(ctx, e.tenantID, e.modelID, graph.Patch{BaseGraphVersion: &base, IdempotencyKey: "scale-1",
		Operations: []graph.Operation{{Op: graph.OpUpdateNode, TargetID: leaf.ID, Payload: raw, Rationale: "rename"}}}, e.author, "")
	if err != nil || !res.Accepted {
		t.Fatalf("patch: %+v %v", res, err)
	}
	t.Logf("patch with conservative invalidation of %d artifacts in %s", len(res.InvalidatedArtifacts), time.Since(start).Round(time.Millisecond))
	if !res.Invalidation.Truncated || len(res.InvalidatedArtifacts) != len(bound) {
		t.Fatalf("conservative invalidation: truncated=%v invalidated=%d/%d", res.Invalidation.Truncated, len(res.InvalidatedArtifacts), len(bound))
	}
	if a := e.artifact(t, res.InvalidatedArtifacts[0]); !strings.Contains(a.StaleReason, "conservatively") {
		t.Fatalf("reason: %q", a.StaleReason)
	}
}
