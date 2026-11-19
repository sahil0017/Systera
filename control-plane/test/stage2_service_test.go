package test

// Stage 2.2 (graph service + API) integration tests. Require SYSTERA_TEST_DATABASE_URL.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/systera/control-plane/internal/auth"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/httpserver"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/projects"
	"github.com/systera/control-plane/internal/runs"
	"github.com/systera/control-plane/internal/scheduler"
)

type gsEnv struct {
	client
	graphEnv
	svc       *graph.Service
	token     string
	viewer    string
	otherTok  string
	projectID string
}

func newGSEnv(t *testing.T) *gsEnv {
	t.Helper()
	ge := newGraphEnv(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := graph.NewService(ge.pool)
	srv := httpserver.New(httpserver.Options{
		Logger:        logger,
		Projects:      projects.NewPostgresRepository(ge.pool),
		Runs:          runs.NewPostgresRepository(ge.pool),
		Events:        outbox.NewPostgresStore(ge.pool, logger),
		Tasks:         scheduler.NewStore(ge.pool, logger),
		Graph:         svc,
		Authenticator: auth.NewAuthenticator(newVerifier(t), auth.NewPostgresMemberships(ge.pool)),
		Readiness:     []httpserver.ReadinessCheck{{Name: "postgres", Check: postgres.PingCheck(ge.pool)}},
	})
	var subject, otherSubject string
	if err := ge.pool.QueryRow(ctx, `SELECT external_sub FROM users WHERE id = $1`, ge.userID).Scan(&subject); err != nil {
		t.Fatal(err)
	}
	if err := ge.pool.QueryRow(ctx, `SELECT u.external_sub FROM users u JOIN tenant_memberships m ON m.user_id = u.id WHERE m.tenant_id = $1`, ge.otherTenant).Scan(&otherSubject); err != nil {
		t.Fatal(err)
	}
	viewerSub := "viewer-" + ids.New()[:8]
	var viewerID string
	if err := ge.pool.QueryRow(ctx, `INSERT INTO users (email, display_name, external_sub) VALUES ($1, $1, $1) RETURNING id::text`, viewerSub).Scan(&viewerID); err != nil {
		t.Fatal(err)
	}
	if _, err := ge.pool.Exec(ctx, `INSERT INTO tenant_memberships (tenant_id, user_id, role) VALUES ($1, $2, 'VIEWER')`, ge.tenantID, viewerID); err != nil {
		t.Fatal(err)
	}
	return &gsEnv{
		client: client{t: t, handler: srv.Handler()}, graphEnv: *ge, svc: svc, projectID: ge.projectID,
		token: mintToken(t, subject, "", time.Hour), viewer: mintToken(t, viewerSub, "", time.Hour),
		otherTok: mintToken(t, otherSubject, "", time.Hour),
	}
}

func addNodeOp(id, key, typ, name string) map[string]any {
	return map[string]any{"op": "add_node", "target_id": id, "rationale": "needed for " + key,
		"payload": map[string]any{"key": key, "type": typ, "name": name}}
}

func addEdgeOp(id, src, dst, rel string) map[string]any {
	return map[string]any{"op": "add_edge", "target_id": id, "rationale": "flow " + rel, "evidence_refs": []string{"req:FR-1"},
		"payload": map[string]any{"source_node_id": src, "target_node_id": dst, "relation_type": rel}}
}

func patchBody(base int, key string, ops ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"base_graph_version": base, "idempotency_key": key, "operations": ops})
	return string(b)
}

func decodeResult(t *testing.T, r response) graph.Result {
	t.Helper()
	var res graph.Result
	r.decode(t, &res)
	return res
}

func findingCodes(fs []graph.Finding) string {
	var c []string
	for _, f := range fs {
		if f.Severity == "ERROR" {
			c = append(c, f.Code)
		}
	}
	return strings.Join(c, ",")
}

func impactedKeys(cs []graph.ImpactedComponent) string {
	var k []string
	for _, c := range cs {
		k = append(k, c.Key)
	}
	return strings.Join(k, ",")
}

func TestGraphPatchAPIEndToEnd(t *testing.T) {
	e := newGSEnv(t)
	ctx := context.Background()

	// Model.
	r := e.do(http.MethodPost, "/v1/projects/"+e.projectID+"/architecture", e.token, "", nil)
	expect(t, r, http.StatusCreated, "create model")
	var created struct {
		Model   graph.Model
		Version graph.Version
	}
	r.decode(t, &created)
	m := created.Model
	expect(t, e.do(http.MethodPost, "/v1/projects/"+e.projectID+"/architecture", e.token, "", nil), http.StatusConflict, "second model")
	expect(t, e.do(http.MethodGet, "/v1/projects/"+e.projectID+"/architecture", e.viewer, "", nil), http.StatusOK, "viewer reads model")
	base := "/v1/architecture/" + m.ID

	// v1: URL shortener core (5 nodes, 4 edges), in one patch with forward references.
	gw, redirect, links, cache, api := ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
	eCalls, eWrites, eReads, eOwns := ids.New(), ids.New(), ids.New(), ids.New()
	p1 := patchBody(0, "p1",
		addNodeOp(gw, "svc.gateway", "service", "API Gateway"), addNodeOp(redirect, "svc.redirect", "service", "Redirect"),
		addNodeOp(links, "db.links", "database", "Links DB"), addNodeOp(cache, "cache.links", "cache", "Links Cache"),
		addNodeOp(api, "api.redirect", "api", "GET /{code}"),
		addEdgeOp(eCalls, gw, redirect, "calls"), addEdgeOp(eWrites, redirect, links, "writes"),
		addEdgeOp(eReads, redirect, cache, "reads"), addEdgeOp(eOwns, redirect, api, "owns"))
	r = e.do(http.MethodPost, base+"/patches", e.token, p1, nil)
	expect(t, r, http.StatusCreated, "patch 1")
	res1 := decodeResult(t, r)
	if !res1.Accepted || *res1.NewGraphVersion != 1 || len(res1.Changes.NodesAdded) != 5 || len(res1.Changes.EdgesAdded) != 4 ||
		r.header.Get("Location") != base+"/versions/1" || len(res1.ImpactedComponents) != 5 {
		t.Fatalf("patch 1 result: %+v", res1)
	}

	// 2.8 idempotency: same body replays; different body under the same key conflicts.
	r = e.do(http.MethodPost, base+"/patches", e.token, p1, nil)
	expect(t, r, http.StatusCreated, "replay")
	if rr := decodeResult(t, r); rr.PatchID != res1.PatchID || r.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %+v", rr)
	}
	expect(t, e.do(http.MethodPost, base+"/patches", e.token, patchBody(0, "p1", addNodeOp(ids.New(), "svc.x", "service", "X")), nil),
		http.StatusUnprocessableEntity, "key reuse with a different body")

	// 2.7 one immutable version whose stored hash matches its content.
	r = e.do(http.MethodGet, base+"/versions/current", e.viewer, "", nil)
	expect(t, r, http.StatusOK, "current version")
	var g1 graph.Graph
	r.decode(t, &g1)
	if g1.Version.Number != 1 || g1.Version.GraphHash != res1.GraphHash || r.header.Get("ETag") != `"`+res1.GraphHash+`"` ||
		len(g1.Nodes) != 5 || g1.Version.PatchID != res1.PatchID {
		t.Fatalf("version 1: %+v", g1.Version)
	}

	// 2.9 App. D event in the outbox.
	var payload map[string]any
	var raw []byte
	if err := e.pool.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE event_type = $1 AND payload->>'patch_id' = $2`,
		graph.EventVersionCreated, res1.PatchID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &payload)
	if payload["graph_version"] != 1.0 || payload["previous_version"] != 0.0 || payload["model_id"] != m.ID ||
		len(payload["changed_node_ids"].([]any)) != 5 || payload["invalidated_artifact_ids"] == nil {
		t.Fatalf("event payload: %v", payload)
	}

	// v2: rename redirect, remove cache (cascading its edge).
	p2 := patchBody(1, "p2",
		map[string]any{"op": "update_node", "target_id": redirect, "rationale": "language decided",
			"payload": map[string]any{"name": "Redirect (Go)", "properties": map[string]any{"language": "go"}}},
		map[string]any{"op": "remove_node", "target_id": cache, "rationale": "cache deferred", "payload": map[string]any{"cascade_edges": true}})
	r = e.do(http.MethodPost, base+"/patches", e.token, p2, nil)
	expect(t, r, http.StatusCreated, "patch 2")
	res2 := decodeResult(t, r)
	if *res2.NewGraphVersion != 2 || len(res2.Changes.EdgesRemoved) != 1 || res2.Changes.EdgesRemoved[0] != eReads ||
		impactedKeys(res2.ImpactedComponents) != "cache.links,svc.gateway,svc.redirect" {
		t.Fatalf("patch 2: changes=%+v impacted=%s", res2.Changes, impactedKeys(res2.ImpactedComponents))
	}

	// 2.6 optimistic concurrency: a patch on base 1 that touches the removed cache.
	p3 := patchBody(1, "p3",
		map[string]any{"op": "update_node", "target_id": cache, "rationale": "bigger cache", "payload": map[string]any{"name": "Big Cache"}},
		map[string]any{"op": "update_node", "target_id": gw, "rationale": "rename", "payload": map[string]any{"name": "Edge Gateway"}})
	r = e.do(http.MethodPost, base+"/patches", e.token, p3, nil)
	expect(t, r, http.StatusConflict, "stale patch")
	res3 := decodeResult(t, r)
	if res3.Status != graph.PatchConflict || res3.CurrentGraphVersion != 2 || len(res3.Conflicts) != 1 ||
		res3.Conflicts[0].TargetID != cache || res3.Conflicts[0].Reason != "removed_since_base" || res3.Conflicts[0].ChangedInVersion != 2 ||
		res3.ChangesSinceBase == nil || len(res3.ChangesSinceBase.NodesRemoved) != 1 || len(res3.ChangesSinceBase.NodesUpdated) != 1 {
		t.Fatalf("conflict result: %+v", res3)
	}

	// 2.5 validation: incompatible edge, bad type, dangling reference -> 422, recorded.
	p4 := patchBody(2, "p4",
		addEdgeOp(ids.New(), links, gw, "calls"),
		addNodeOp(ids.New(), "x.y", "spaceship", "X"),
		addEdgeOp(ids.New(), gw, ids.New(), "calls"))
	r = e.do(http.MethodPost, base+"/patches", e.token, p4, nil)
	expect(t, r, http.StatusUnprocessableEntity, "invalid patch")
	res4 := decodeResult(t, r)
	if codes := findingCodes(res4.ValidationFindings); codes != "incompatible_edge,invalid_node_type,unknown_endpoint" {
		t.Fatalf("findings: %s", codes)
	}
	r = e.do(http.MethodGet, base+"/patches/"+res4.PatchID, e.viewer, "", nil)
	expect(t, r, http.StatusOK, "get rejected patch")
	if decodeResult(t, r).Status != graph.PatchRejected {
		t.Fatal("rejected patch not recorded")
	}
	r = e.do(http.MethodPost, base+"/patches", e.token, patchBody(2, "p5",
		map[string]any{"op": "update_node", "target_id": gw, "rationale": "noop", "payload": map[string]any{"name": "API Gateway"}}), nil)
	expect(t, r, http.StatusUnprocessableEntity, "no-op patch")
	if !strings.Contains(findingCodes(decodeResult(t, r).ValidationFindings), "no_effect") {
		t.Fatal("no-op patch must be rejected with no_effect")
	}
	expect(t, e.do(http.MethodPost, base+"/patches", e.token, `{"base_graph_version":2,"idempotency_key":"p6","operations":[],"oops":1}`, nil),
		http.StatusBadRequest, "malformed patch")
	if vs, _ := e.svc.Versions(ctx, e.tenantID, m.ID, 10); len(vs) != 3 {
		t.Fatalf("rejected and conflicting patches must not create versions: %d versions", len(vs))
	}

	// 2.10 diff.
	r = e.do(http.MethodGet, base+"/diff?from=1&to=current", e.viewer, "", nil)
	expect(t, r, http.StatusOK, "diff")
	var d graph.Diff
	r.decode(t, &d)
	if len(d.Nodes.Removed) != 1 || d.Nodes.Removed[0].ID != cache || len(d.Nodes.Modified) != 1 ||
		strings.Join(d.Nodes.Modified[0].ChangedFields, ",") != "name,properties" || len(d.Edges.Removed) != 1 || len(d.Nodes.Added) != 0 {
		t.Fatalf("diff 1..2: %+v", d)
	}
	expect(t, e.do(http.MethodGet, base+"/diff?to=2", e.viewer, "", nil), http.StatusBadRequest, "diff without from")

	// 2.10 neighbourhood.
	r = e.do(http.MethodGet, base+"/nodes/"+redirect+"/neighbourhood?depth=1", e.viewer, "", nil)
	expect(t, r, http.StatusOK, "neighbourhood")
	var nb graph.Neighbourhood
	r.decode(t, &nb)
	if len(nb.Nodes) != 4 || nb.Nodes[0].ID != redirect || nb.Nodes[0].Depth != 0 || len(nb.Edges) != 3 {
		t.Fatalf("neighbourhood: %d nodes %d edges", len(nb.Nodes), len(nb.Edges))
	}
	r = e.do(http.MethodGet, base+"/nodes/"+redirect+"/neighbourhood?direction=in&version=1", e.viewer, "", nil)
	r.decode(t, &nb)
	if len(nb.Nodes) != 2 || nb.Version != 1 {
		t.Fatalf("inbound neighbourhood at v1: %+v", nb.Nodes)
	}
	r = e.do(http.MethodGet, base+"/nodes/"+gw+"/neighbourhood?depth=2&relation=calls", e.viewer, "", nil)
	r.decode(t, &nb)
	if len(nb.Nodes) != 2 {
		t.Fatalf("calls-only neighbourhood: %+v", nb.Nodes)
	}
	expect(t, e.do(http.MethodGet, base+"/nodes/"+cache+"/neighbourhood", e.viewer, "", nil), http.StatusNotFound, "removed node")
	expect(t, e.do(http.MethodGet, base+"/nodes/"+gw+"/neighbourhood?depth=9", e.viewer, "", nil), http.StatusBadRequest, "depth limit")

	// Authorization and tenancy.
	expect(t, e.do(http.MethodPost, base+"/patches", e.viewer, patchBody(2, "v1", addNodeOp(ids.New(), "svc.v", "service", "V")), nil), http.StatusForbidden, "viewer patch")
	expect(t, e.do(http.MethodGet, base+"/versions/current", e.otherTok, "", nil), http.StatusNotFound, "other tenant read")
	expect(t, e.do(http.MethodGet, base+"/versions", e.otherTok, "", nil), http.StatusNotFound, "other tenant versions")
	expect(t, e.do(http.MethodPost, base+"/patches", e.otherTok, patchBody(2, "x1", addNodeOp(ids.New(), "svc.x", "service", "X")), nil), http.StatusNotFound, "other tenant patch")

	// Provenance: run_id must belong to the model's project.
	var foreignRun string
	if err := e.pool.QueryRow(ctx, `INSERT INTO design_runs (tenant_id, project_id, objective, created_by)
		VALUES ($1, $2, 'x', (SELECT user_id FROM tenant_memberships WHERE tenant_id = $1 LIMIT 1)) RETURNING id::text`,
		e.otherTenant, e.otherProjectID).Scan(&foreignRun); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"base_graph_version": 2, "idempotency_key": "prov", "run_id": foreignRun,
		"operations": []any{addNodeOp(ids.New(), "svc.p", "service", "P")}})
	r = e.do(http.MethodPost, base+"/patches", e.token, string(b), nil)
	expect(t, r, http.StatusUnprocessableEntity, "foreign run")
	if !strings.Contains(findingCodes(decodeResult(t, r).ValidationFindings), "unknown_run") {
		t.Fatal("foreign run_id must be rejected")
	}

	// 2.5 agents (or any non-graph-service code) cannot write graph tables directly.
	for _, role := range []string{postgres.AppRole, postgres.SystemRole} {
		err := pgx.BeginTxFunc(ctx, e.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT set_config('app.current_tenant_id', $1, true)", e.tenantID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO architecture_nodes (tenant_id, model_id, node_id, node_key, node_type, name, valid_from_version)
				VALUES ($1, $2, gen_random_uuid(), 'svc.direct', 'service', 'Direct', 2)`, e.tenantID, m.ID)
			return err
		})
		expectPG(t, err, "42501", role+" writing graph rows directly")
	}
}

func TestGraphPatchConcurrency(t *testing.T) {
	e := newGSEnv(t)
	ctx := context.Background()
	m, _, err := e.svc.CreateModel(ctx, e.tenantID, e.projectID, e.author, "")
	if err != nil {
		t.Fatal(err)
	}
	patch := func(key string, base int, ops ...graph.Operation) graph.Patch {
		return graph.Patch{BaseGraphVersion: &base, IdempotencyKey: key, Operations: ops}
	}
	add := func(key string) graph.Operation {
		raw, _ := json.Marshal(graph.AddNodePayload{Key: key, Type: "service", Name: key})
		return graph.Operation{Op: graph.OpAddNode, TargetID: ids.New(), Payload: raw, Rationale: "race"}
	}

	// Eight writers race on the same base: exactly one wins, the rest get CONFLICT.
	const writers = 8
	results := make([]graph.Result, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := e.svc.ApplyPatch(ctx, e.tenantID, m.ID, patch(fmt.Sprintf("race-%d", i), 0, add(fmt.Sprintf("svc.r%d", i))), e.author, "")
			if err != nil {
				t.Errorf("writer %d: %v", i, err)
			}
			results[i] = res
		}(i)
	}
	wg.Wait()
	accepted, conflicts := 0, 0
	for _, r := range results {
		switch r.Status {
		case graph.PatchAccepted:
			accepted++
		case graph.PatchConflict:
			conflicts++
			if r.CurrentGraphVersion != 1 || r.ChangesSinceBase == nil || len(r.ChangesSinceBase.NodesAdded) != 1 || len(r.Conflicts) != 0 {
				t.Errorf("conflict detail: %+v", r)
			}
		}
	}
	if accepted != 1 || conflicts != writers-1 {
		t.Fatalf("accepted=%d conflicts=%d", accepted, conflicts)
	}

	// The same idempotent request sent concurrently yields one version and one patch.
	same := patch("same-key", 1, add("svc.same"))
	var ids2 sync.Map
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := e.svc.ApplyPatch(ctx, e.tenantID, m.ID, same, e.author, "")
			if err != nil || res.Status != graph.PatchAccepted {
				t.Errorf("idempotent submit: %+v %v", res, err)
				return
			}
			ids2.Store(res.PatchID, true)
		}()
	}
	wg.Wait()
	n := 0
	ids2.Range(func(_, _ any) bool { n++; return true })
	if vs, _ := e.svc.Versions(ctx, e.tenantID, m.ID, 10); n != 1 || len(vs) != 3 {
		t.Fatalf("idempotent race: %d distinct patches, %d versions", n, len(vs))
	}
}
