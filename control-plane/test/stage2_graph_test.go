package test

// Stage 2.1 (canonical graph data model) integration tests. Require SYSTERA_TEST_DATABASE_URL.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/postgres"
)

type graphEnv struct {
	pool             *pgxpool.Pool
	store            *graph.Store
	tenantID, userID string
	projectID        string
	otherTenant      string
	otherProjectID   string
	model            graph.Model
	author           graph.Author
}

func newGraphEnv(t *testing.T) *graphEnv {
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
	e := &graphEnv{pool: pool, store: graph.NewStore(pool)}
	e.tenantID, e.userID, _ = seedIdentity(t, pool, "OWNER")
	e.otherTenant, _, _ = seedIdentity(t, pool, "OWNER")
	e.projectID = seedProject(t, pool, e.tenantID, e.userID)
	otherUser := ""
	if err := pool.QueryRow(ctx, `SELECT user_id::text FROM tenant_memberships WHERE tenant_id = $1`, e.otherTenant).Scan(&otherUser); err != nil {
		t.Fatal(err)
	}
	e.otherProjectID = seedProject(t, pool, e.otherTenant, otherUser)
	e.author = graph.Author{Type: "USER", ID: e.userID}
	return e
}

func seedProject(t *testing.T, pool *pgxpool.Pool, tenantID, userID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `INSERT INTO projects (tenant_id, name, created_by) VALUES ($1, 'graph', $2) RETURNING id::text`,
		tenantID, userID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// change is one version's worth of edits, applied the way the graph service will:
// compute the next graph and its hash, append the version, then close/append rows.
type change struct {
	addNodes    []graph.Node
	editNodes   []graph.Node // same ID, new content
	removeNodes []string
	addEdges    []graph.Edge
	removeEdges []string
	summary     string
}

func node(key, typ, name string) graph.Node {
	return graph.Node{ID: ids.New(), Key: key, Type: typ, Name: name, Properties: map[string]any{}, LifecycleState: "PROPOSED", EvidenceState: "PROPOSED"}
}

func edge(src, dst graph.Node, rel string) graph.Edge {
	return graph.Edge{ID: ids.New(), SourceNodeID: src.ID, TargetNodeID: dst.ID, RelationType: rel, Direction: "DIRECTED",
		Constraints: map[string]any{}, Properties: map[string]any{}}
}

func (e *graphEnv) apply(t *testing.T, modelID string, c change) (graph.Version, error) {
	t.Helper()
	ctx := context.Background()
	cur, err := e.store.GraphAt(ctx, e.tenantID, modelID, -1)
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[string]graph.Node{}
	for _, n := range cur.Nodes {
		nodes[n.ID] = n
	}
	edges := map[string]graph.Edge{}
	for _, ed := range cur.Edges {
		edges[ed.ID] = ed
	}
	for _, id := range c.removeNodes {
		delete(nodes, id)
	}
	for _, n := range append(append([]graph.Node{}, c.editNodes...), c.addNodes...) {
		nodes[n.ID] = n
	}
	for _, id := range c.removeEdges {
		delete(edges, id)
	}
	for _, ed := range c.addEdges {
		edges[ed.ID] = ed
	}
	var nl []graph.Node
	for _, n := range nodes {
		nl = append(nl, n)
	}
	var el []graph.Edge
	for _, ed := range edges {
		el = append(el, ed)
	}
	next := cur.Version.Number + 1

	var ver graph.Version
	err = postgres.WithTenantRoleTx(ctx, e.pool, e.tenantID, postgres.GraphRole, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM architecture_models WHERE id = $1 FOR UPDATE`, modelID); err != nil {
			return err
		}
		var verID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO architecture_versions (tenant_id, model_id, version_number, parent_version_id, graph_hash, node_count, edge_count, change_summary, author_type, author_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'USER', $9) RETURNING id::text`,
			e.tenantID, modelID, next, cur.Version.ID, graph.Hash(nl, el), len(nl), len(el), c.summary, e.userID).Scan(&verID); err != nil {
			return err
		}
		closeRows := func(table, col string, idsToClose []string) error {
			for _, id := range idsToClose {
				if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET valid_to_version = $3 WHERE model_id = $1 AND %s = $2 AND valid_to_version IS NULL`, table, col),
					modelID, id, next); err != nil {
					return err
				}
			}
			return nil
		}
		var edited []string
		for _, n := range c.editNodes {
			edited = append(edited, n.ID)
		}
		if err := closeRows("architecture_nodes", "node_id", append(append([]string{}, c.removeNodes...), edited...)); err != nil {
			return err
		}
		if err := closeRows("architecture_edges", "edge_id", c.removeEdges); err != nil {
			return err
		}
		for _, n := range append(append([]graph.Node{}, c.editNodes...), c.addNodes...) {
			if _, err := tx.Exec(ctx, `
				INSERT INTO architecture_nodes (tenant_id, model_id, node_id, node_key, node_type, name, description, properties,
				                                lifecycle_state, evidence_state, revision, valid_from_version)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
				        (SELECT COALESCE(MAX(revision), 0) + 1 FROM architecture_nodes WHERE node_id = $3), $11)`,
				e.tenantID, modelID, n.ID, n.Key, n.Type, n.Name, n.Description, n.Properties, n.LifecycleState, n.EvidenceState, next); err != nil {
				return err
			}
		}
		for _, ed := range c.addEdges {
			if _, err := tx.Exec(ctx, `
				INSERT INTO architecture_edges (tenant_id, model_id, edge_id, source_node_id, target_node_id, relation_type, direction, constraints, properties, valid_from_version)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
				e.tenantID, modelID, ed.ID, ed.SourceNodeID, ed.TargetNodeID, ed.RelationType, ed.Direction, ed.Constraints, ed.Properties, next); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE architecture_models SET current_version_id = $2 WHERE id = $1`, modelID, verID); err != nil {
			return err
		}
		ver = graph.Version{ID: verID, Number: next}
		return nil
	})
	return ver, err
}

// asGraph runs SQL as the graph-writer role (the only role with graph write privileges),
// so the database's own integrity guards are what decide.
func asGraph(pool *pgxpool.Pool, tenantID, sql string, args ...any) error {
	return postgres.WithTenantRoleTx(context.Background(), pool, tenantID, postgres.GraphRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), sql, args...)
		return err
	})
}

// asTenant runs SQL as the application role bound to a tenant.
func asTenant(pool *pgxpool.Pool, tenantID, sql string, args ...any) error {
	return postgres.WithTenantTx(context.Background(), pool, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), sql, args...)
		return err
	})
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func expectPG(t *testing.T, err error, code, what string) {
	t.Helper()
	if pgCode(err) != code {
		t.Fatalf("%s: expected SQLSTATE %s, got %v", what, code, err)
	}
}

// ---------------------------------------------------------------------------

func TestGraphModelLifecycleAndTraversal(t *testing.T) {
	e := newGraphEnv(t)
	ctx := context.Background()

	m, v0, err := e.store.CreateModel(ctx, e.tenantID, e.projectID, e.author, "")
	if err != nil {
		t.Fatal(err)
	}
	if m.CurrentVersion != 0 || v0.Number != 0 || v0.GraphHash != graph.Hash(nil, nil) || v0.ParentID != "" {
		t.Fatalf("model/version 0: %+v %+v", m, v0)
	}
	if _, _, err := e.store.CreateModel(ctx, e.tenantID, e.projectID, e.author, ""); !errors.Is(err, graph.ErrModelExists) {
		t.Fatalf("second model for project: %v", err)
	}
	if _, _, err := e.store.CreateModel(ctx, e.tenantID, e.otherProjectID, e.author, ""); !errors.Is(err, graph.ErrProjectNotFound) {
		t.Fatalf("model for another tenant's project: %v", err)
	}
	var evt string
	if err := e.pool.QueryRow(ctx, `SELECT payload::text FROM outbox_events WHERE event_type = $1 AND payload->>'model_id' = $2`,
		graph.EventVersionCreated, m.ID).Scan(&evt); err != nil || !strings.Contains(evt, `"graph_version": 0`) {
		t.Fatalf("version-created event: %q %v", evt, err)
	}

	// v1: URL shortener core.
	gw, redirect, links, cache := node("svc.gateway", "service", "API Gateway"), node("svc.redirect", "service", "Redirect Service"),
		node("db.links", "database", "Links DB"), node("cache.links", "cache", "Links Cache")
	e1, e2, e3, e4 := edge(gw, redirect, "calls"), edge(redirect, links, "reads"), edge(redirect, links, "writes"), edge(redirect, cache, "reads")
	v1, err := e.apply(t, m.ID, change{addNodes: []graph.Node{gw, redirect, links, cache}, addEdges: []graph.Edge{e1, e2, e3, e4}, summary: "core"})
	if err != nil {
		t.Fatal(err)
	}

	// v2: rename the redirect service, drop the cache (and its edge).
	renamed := redirect
	renamed.Name = "Redirect Service (Go)"
	renamed.Properties = map[string]any{"language": "go", "replicas": 3}
	if _, err := e.apply(t, m.ID, change{editNodes: []graph.Node{renamed}, removeNodes: []string{cache.ID}, removeEdges: []string{e4.ID}, summary: "drop cache"}); err != nil {
		t.Fatal(err)
	}

	g1, err := e.store.GraphAt(ctx, e.tenantID, m.ID, v1.Number)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := e.store.GraphAt(ctx, e.tenantID, m.ID, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(g1.Nodes) != 4 || len(g1.Edges) != 4 || cur.Version.Number != 2 || len(cur.Nodes) != 3 || len(cur.Edges) != 3 {
		t.Fatalf("history: v1 %d/%d, current v%d %d/%d", len(g1.Nodes), len(g1.Edges), cur.Version.Number, len(cur.Nodes), len(cur.Edges))
	}
	for _, g := range []graph.Graph{g1, cur} {
		if g.ComputeHash() != g.Version.GraphHash {
			t.Fatalf("v%d: stored hash does not match content", g.Version.Number)
		}
	}
	var r1, r2 graph.Node
	for _, n := range g1.Nodes {
		if n.ID == redirect.ID {
			r1 = n
		}
	}
	for _, n := range cur.Nodes {
		if n.ID == redirect.ID {
			r2 = n
		}
	}
	if r1.Name != "Redirect Service" || r1.Revision != 1 || r2.Name != "Redirect Service (Go)" || r2.Revision != 2 || r2.Properties["replicas"].(fmt.Stringer).String() != "3" {
		t.Fatalf("node revisions: v1 %+v, v2 %+v", r1, r2)
	}
	if vs, err := e.store.Versions(ctx, e.tenantID, m.ID, 10); err != nil || len(vs) != 3 || vs[0].Number != 2 || vs[0].ParentID != vs[1].ID {
		t.Fatalf("versions: %+v %v", vs, err)
	}

	// Impact analysis direction: what depends on the links DB (inbound walk)?
	in, err := e.store.Reachable(ctx, e.tenantID, m.ID, 1, links.ID, graph.TraversalQuery{Direction: graph.Inbound})
	if err != nil {
		t.Fatal(err)
	}
	if keys := nodeKeys(in.Nodes); keys != "svc.gateway,svc.redirect" {
		t.Fatalf("inbound from db at v1: %s", keys)
	}
	out, _ := e.store.Reachable(ctx, e.tenantID, m.ID, 1, gw.ID, graph.TraversalQuery{Direction: graph.Outbound, RelationTypes: []string{"calls"}})
	if keys := nodeKeys(out.Nodes); keys != "svc.redirect" {
		t.Fatalf("outbound calls-only from gateway: %s", keys)
	}
	if _, err := e.store.Reachable(ctx, e.tenantID, m.ID, -1, cache.ID, graph.TraversalQuery{}); !errors.Is(err, graph.ErrNodeNotFound) {
		t.Fatalf("removed node at current version: %v", err)
	}
	all, _ := e.store.Reachable(ctx, e.tenantID, m.ID, 1, gw.ID, graph.TraversalQuery{Direction: graph.Both, MaxNodes: 2})
	if !all.Truncated || len(all.Nodes) != 2 {
		t.Fatalf("truncation: %+v", all)
	}

	// Cycles terminate: add links -> gateway (depends_on) closing a loop.
	if _, err := e.apply(t, m.ID, change{addEdges: []graph.Edge{edge(links, gw, "depends_on")}}); err != nil {
		t.Fatal(err)
	}
	loop, err := e.store.Reachable(ctx, e.tenantID, m.ID, -1, gw.ID, graph.TraversalQuery{Direction: graph.Outbound})
	if err != nil || nodeKeys(loop.Nodes) != "db.links,svc.redirect" {
		t.Fatalf("cyclic walk: %s %v", nodeKeys(loop.Nodes), err)
	}

	// Published snapshots are idempotent and immutable.
	s1, err := e.store.PublishSnapshot(ctx, e.tenantID, m.ID, 1, "design review", e.userID)
	if err != nil {
		t.Fatal(err)
	}
	s1b, err := e.store.PublishSnapshot(ctx, e.tenantID, m.ID, 1, "again", e.userID)
	if err != nil || s1b.ID != s1.ID || s1.GraphHash != g1.Version.GraphHash {
		t.Fatalf("snapshot idempotency: %+v %+v %v", s1, s1b, err)
	}
	// Two layers: the app role lacks UPDATE entirely; even the owner hits the trigger.
	expectPG(t, asTenant(e.pool, e.tenantID, `UPDATE graph_snapshots SET label = 'x' WHERE id = $1`, s1.ID), "42501", "snapshot update as app")
	_, err = e.pool.Exec(ctx, `UPDATE graph_snapshots SET label = 'x' WHERE id = $1`, s1.ID)
	expectPG(t, err, "23001", "snapshot update as owner")
}

func nodeKeys(ns []graph.Node) string {
	keys := make([]string, len(ns))
	for i, n := range ns {
		keys[i] = n.Key
	}
	return strings.Join(keys, ",")
}

func TestGraphIntegrityIsEnforcedByTheDatabase(t *testing.T) {
	e := newGraphEnv(t)
	ctx := context.Background()
	m, v0, err := e.store.CreateModel(ctx, e.tenantID, e.projectID, e.author, "")
	if err != nil {
		t.Fatal(err)
	}
	a, b := node("svc.a", "service", "A"), node("svc.b", "service", "B")
	ab := edge(a, b, "calls")
	v1, err := e.apply(t, m.ID, change{addNodes: []graph.Node{a, b}, addEdges: []graph.Edge{ab}})
	if err != nil {
		t.Fatal(err)
	}

	// Rejected changes leave the model untouched.
	ghost := node("svc.ghost", "service", "Ghost")
	_, err = e.apply(t, m.ID, change{addEdges: []graph.Edge{edge(a, ghost, "calls")}})
	expectPG(t, err, "23503", "edge to a node that does not exist")
	_, err = e.apply(t, m.ID, change{removeNodes: []string{b.ID}})
	expectPG(t, err, "23503", "removing a node that live edges reference")
	retyped := a
	retyped.Type = "database"
	_, err = e.apply(t, m.ID, change{editNodes: []graph.Node{retyped}})
	expectPG(t, err, "23514", "changing a node's type")
	dup := node("svc.a", "service", "Another A")
	_, err = e.apply(t, m.ID, change{addNodes: []graph.Node{dup}})
	expectPG(t, err, "23P01", "two live nodes with one key")
	if cur, _ := e.store.GraphAt(ctx, e.tenantID, m.ID, -1); cur.Version.Number != 1 {
		t.Fatalf("failed changes must not advance the model, at v%d", cur.Version.Number)
	}

	cases := []struct {
		what, sql, code string
		args            []any
	}{
		{"rewriting a node in place", `UPDATE architecture_nodes SET name = 'hacked' WHERE node_id = $1`, "23001", []any{a.ID}},
		{"second live revision of a node", `INSERT INTO architecture_nodes (tenant_id, model_id, node_id, node_key, node_type, name, revision, valid_from_version)
			VALUES ($1, $2, $3, 'svc.a2', 'service', 'A2', 2, 1)`, "23P01", []any{e.tenantID, m.ID, a.ID}},
		{"skipping a revision number", `INSERT INTO architecture_nodes (tenant_id, model_id, node_id, node_key, node_type, name, revision, valid_from_version)
			VALUES ($1, $2, gen_random_uuid(), 'svc.c', 'service', 'C', 5, 1)`, "23514", []any{e.tenantID, m.ID}},
		{"unknown node type", `INSERT INTO architecture_nodes (tenant_id, model_id, node_id, node_key, node_type, name, valid_from_version)
			VALUES ($1, $2, gen_random_uuid(), 'x.y', 'spaceship', 'X', 1)`, "23514", []any{e.tenantID, m.ID}},
		{"self-loop edge", `INSERT INTO architecture_edges (tenant_id, model_id, edge_id, source_node_id, target_node_id, relation_type, valid_from_version)
			VALUES ($1, $2, gen_random_uuid(), $3, $3, 'calls', 1)`, "23514", []any{e.tenantID, m.ID, a.ID}},
		{"row at a version that does not exist", `INSERT INTO architecture_nodes (tenant_id, model_id, node_id, node_key, node_type, name, valid_from_version)
			VALUES ($1, $2, gen_random_uuid(), 'svc.future', 'service', 'F', 9)`, "23503", []any{e.tenantID, m.ID}},
		{"rewriting a version (insert-only even for the graph writer)", `UPDATE architecture_versions SET change_summary = 'x' WHERE id = $1`, "42501", []any{v1.ID}},
		{"deleting a version", `DELETE FROM architecture_versions WHERE id = $1`, "42501", []any{v1.ID}},
		{"deleting a node revision", `DELETE FROM architecture_nodes WHERE node_id = $1`, "42501", []any{a.ID}},
		{"version gap", `INSERT INTO architecture_versions (tenant_id, model_id, version_number, parent_version_id, graph_hash, author_type, author_id)
			VALUES ($1, $2, 5, $3, repeat('a', 64), 'USER', 'u')`, "23514", []any{e.tenantID, m.ID, v1.ID}},
		{"second root version", `INSERT INTO architecture_versions (tenant_id, model_id, version_number, graph_hash, author_type, author_id)
			VALUES ($1, $2, 0, repeat('a', 64), 'USER', 'u')`, "23505", []any{e.tenantID, m.ID}},
		{"moving the current version backwards", `UPDATE architecture_models SET current_version_id = $2 WHERE id = $1`, "23514", []any{m.ID, v0.ID}},
		{"malformed graph hash", `INSERT INTO architecture_versions (tenant_id, model_id, version_number, parent_version_id, graph_hash, author_type, author_id)
			VALUES ($1, $2, 2, $3, 'not-a-hash', 'USER', 'u')`, "23514", []any{e.tenantID, m.ID, v1.ID}},
		{"snapshot with a forged hash", `INSERT INTO graph_snapshots (tenant_id, model_id, version_id, version_number, graph_hash, published_by)
			VALUES ($1, $2, $3, 1, repeat('b', 64), 'u')`, "23514", []any{e.tenantID, m.ID, v1.ID}},
	}
	for _, c := range cases {
		expectPG(t, asGraph(e.pool, e.tenantID, c.sql, c.args...), c.code, c.what)
	}
	// The owner role has the privilege, so the immutability trigger is what stops it.
	_, err = e.pool.Exec(ctx, `UPDATE architecture_versions SET change_summary = 'x' WHERE id = $1`, v1.ID)
	expectPG(t, err, "23001", "rewriting a version as owner")

	// Closing a revision is the one permitted update, and it is final.
	edited := b
	edited.Name = "B v2"
	if _, err := e.apply(t, m.ID, change{editNodes: []graph.Node{edited}}); err != nil {
		t.Fatalf("legitimate edit: %v", err)
	}
	expectPG(t, asGraph(e.pool, e.tenantID, `UPDATE architecture_nodes SET valid_to_version = NULL WHERE node_id = $1 AND revision = 1`, b.ID), "23001", "reopening a closed revision")

	// Patches: content immutable; decided patches frozen; ACCEPTED needs a version.
	var patchID string
	err = postgres.WithTenantRoleTx(ctx, e.pool, e.tenantID, postgres.GraphRole, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO graph_patches (tenant_id, model_id, base_version_number, idempotency_key, request_fingerprint, operations, author_type, author_id)
			VALUES ($1, $2, 1, 'k1', repeat('c', 64), '[{"op":"add_node"}]', 'AGENT', 'agent-1') RETURNING id::text`,
			e.tenantID, m.ID).Scan(&patchID)
	})
	if err != nil {
		t.Fatal(err)
	}
	expectPG(t, asGraph(e.pool, e.tenantID, `UPDATE graph_patches SET operations = '[{"op":"remove_node"}]' WHERE id = $1`, patchID), "23001", "editing patch content")
	expectPG(t, asGraph(e.pool, e.tenantID, `UPDATE graph_patches SET status = 'ACCEPTED', decided_at = NOW() WHERE id = $1`, patchID), "23514", "ACCEPTED without a resulting version")
	expectPG(t, asGraph(e.pool, e.tenantID, `INSERT INTO graph_patches (tenant_id, model_id, base_version_number, idempotency_key, request_fingerprint, operations, author_type, author_id)
		VALUES ($1, $2, 1, 'k1', repeat('d', 64), '[{"op":"add_node"}]', 'AGENT', 'agent-1')`, e.tenantID, m.ID), "23505", "reusing an idempotency key")
	if err := asGraph(e.pool, e.tenantID, `UPDATE graph_patches SET status = 'REJECTED', decided_at = NOW(), result = '{"reason":"test"}' WHERE id = $1`, patchID); err != nil {
		t.Fatalf("rejecting a pending patch: %v", err)
	}
	expectPG(t, asGraph(e.pool, e.tenantID, `UPDATE graph_patches SET status = 'PENDING', decided_at = NULL WHERE id = $1`, patchID), "23001", "reopening a decided patch")
}

func TestGraphTenantIsolation(t *testing.T) {
	e := newGraphEnv(t)
	ctx := context.Background()
	m, _, err := e.store.CreateModel(ctx, e.tenantID, e.projectID, e.author, "")
	if err != nil {
		t.Fatal(err)
	}
	a := node("svc.a", "service", "A")
	if _, err := e.apply(t, m.ID, change{addNodes: []graph.Node{a}}); err != nil {
		t.Fatal(err)
	}

	if _, err := e.store.ModelForProject(ctx, e.otherTenant, e.projectID); !errors.Is(err, graph.ErrModelNotFound) {
		t.Fatalf("other tenant sees the model: %v", err)
	}
	if _, err := e.store.GraphAt(ctx, e.otherTenant, m.ID, -1); !errors.Is(err, graph.ErrVersionNotFound) {
		t.Fatalf("other tenant reads the graph: %v", err)
	}
	if _, err := e.store.Reachable(ctx, e.otherTenant, m.ID, -1, a.ID, graph.TraversalQuery{}); !errors.Is(err, graph.ErrVersionNotFound) {
		t.Fatalf("other tenant traverses the graph: %v", err)
	}
	for table, col := range map[string]string{"architecture_models": "id", "architecture_versions": "model_id", "architecture_nodes": "model_id"} {
		var n int
		err := postgres.WithTenantTx(ctx, e.pool, e.otherTenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE `+col+` = $1`, m.ID).Scan(&n)
		})
		if err != nil || n != 0 {
			t.Fatalf("%s visible across tenants: n=%d err=%v", table, n, err)
		}
	}
	// Writing into another tenant's model is refused, whether the writer uses its own
	// tenant id (the model is invisible to it) or spoofs the owner's (RLS WITH CHECK).
	for _, spoof := range []string{e.otherTenant, e.tenantID} {
		err := asGraph(e.pool, e.otherTenant, `INSERT INTO architecture_nodes (tenant_id, model_id, node_id, node_key, node_type, name, valid_from_version)
			VALUES ($1, $2, gen_random_uuid(), 'svc.x', 'service', 'X', 1)`, spoof, m.ID)
		if err == nil || (pgCode(err) != "23514" && !strings.Contains(err.Error(), "row-level security")) {
			t.Fatalf("cross-tenant write with tenant_id=%s: %v", spoof[:8], err)
		}
	}
	if g, _ := e.store.GraphAt(ctx, e.tenantID, m.ID, -1); len(g.Nodes) != 1 {
		t.Fatalf("owner's graph changed: %d nodes", len(g.Nodes))
	}
}

// Recursive traversal over the adjacency tables stays fast on a realistic model size.
func TestGraphTraversalScale(t *testing.T) {
	e := newGraphEnv(t)
	ctx := context.Background()
	m, _, err := e.store.CreateModel(ctx, e.tenantID, e.projectID, e.author, "")
	if err != nil {
		t.Fatal(err)
	}
	const n = 2000
	nodes := make([]graph.Node, n)
	for i := range nodes {
		nodes[i] = node(fmt.Sprintf("c.%04d", i), "component", fmt.Sprintf("Component %d", i))
	}
	var edges []graph.Edge
	for i := 1; i < n; i++ { // binary-tree dependencies plus cross links (a DAG with shared descendants)
		edges = append(edges, edge(nodes[(i-1)/2], nodes[i], "depends_on"))
		if i > 10 && i%7 == 0 {
			edges = append(edges, edge(nodes[i-10], nodes[i], "calls"))
		}
	}
	if _, err := e.apply(t, m.ID, change{addNodes: nodes, addEdges: edges}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, err := e.store.Reachable(ctx, e.tenantID, m.ID, -1, nodes[0].ID, graph.TraversalQuery{Direction: graph.Outbound, MaxNodes: 10000})
	elapsed := time.Since(start)
	if err != nil || len(res.Nodes) != n-1 || res.Truncated {
		t.Fatalf("full traversal: %d nodes, truncated=%v, err=%v", len(res.Nodes), res.Truncated, err)
	}
	in, err := e.store.Reachable(ctx, e.tenantID, m.ID, -1, nodes[n-1].ID, graph.TraversalQuery{Direction: graph.Inbound})
	if err != nil || len(in.Nodes) == 0 {
		t.Fatalf("inbound traversal: %v", err)
	}
	t.Logf("reachable(%d nodes, %d edges) in %s; inbound from a leaf found %d ancestors", n, len(edges), elapsed, len(in.Nodes))
	if elapsed > 2*time.Second {
		t.Fatalf("traversal too slow: %s", elapsed)
	}
}
