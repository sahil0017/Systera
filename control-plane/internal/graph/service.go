package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
)

var nodeKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{0,127}$`)

// Service is the graph service: the only writer of the architecture graph (it alone
// assumes the systera_graph role). Every change is a validated GraphPatch that either
// creates exactly one new immutable version or is recorded as REJECTED / CONFLICT.
type Service struct {
	*Store
	now func() time.Time
	// Impact bounds the dependency engine's traversal (zero values use DefaultImpactLimits).
	Impact ImpactLimits
}

func (s *Service) impactLimits() ImpactLimits { return s.Impact.normalized() }

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{Store: NewStore(pool), now: time.Now}
}

func (s *Service) writeTx(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.GraphRole, fn)
}

// precheck validates patch structure that does not need the database. Problems here are
// request errors (HTTP 400): the patch is not recorded.
func precheck(p *Patch) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidPatch, fmt.Sprintf(format, args...))
	}
	switch {
	case p.BaseGraphVersion == nil || *p.BaseGraphVersion < 0:
		return bad("base_graph_version is required and must be >= 0")
	case p.IdempotencyKey == "" || len(p.IdempotencyKey) > maxIdempotencyKeyLen:
		return bad("idempotency_key is required (1-%d characters)", maxIdempotencyKeyLen)
	case len(p.Operations) == 0:
		return bad("operations must not be empty")
	case len(p.Operations) > MaxOperationsPerPatch:
		return bad("at most %d operations per patch", MaxOperationsPerPatch)
	case p.PatchID != "" && !ids.Valid(p.PatchID):
		return bad("patch_id must be a UUID")
	case p.RunID != "" && !ids.Valid(p.RunID):
		return bad("run_id must be a UUID")
	case p.AgentTaskID != "" && !ids.Valid(p.AgentTaskID):
		return bad("agent_task_id must be a UUID")
	case p.AgentTaskID != "" && p.RunID == "":
		return bad("agent_task_id requires run_id")
	}
	for i := 0; i < len(p.IdempotencyKey); i++ {
		if c := p.IdempotencyKey[i]; c < 0x21 || c > 0x7e {
			return bad("idempotency_key must be printable ASCII")
		}
	}
	for i, op := range p.Operations {
		if _, err := canonicalJSON(op.Payload); err != nil {
			return bad("operation %d: payload is not valid JSON: %v", i, err)
		}
	}
	return nil
}

// ApplyPatch validates and applies a GraphPatch (see package doc). It returns a Result
// for ACCEPTED, REJECTED and CONFLICT outcomes (all recorded in graph_patches), and an
// error only for requests that cannot be evaluated at all.
func (s *Service) ApplyPatch(ctx context.Context, tenantID, modelID string, p Patch, author Author, correlationID string) (Result, error) {
	if !ids.Valid(modelID) {
		return Result{}, ErrModelNotFound
	}
	if err := precheck(&p); err != nil {
		return Result{}, err
	}
	fingerprint, err := p.Fingerprint()
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidPatch, err)
	}
	if !ids.Valid(correlationID) {
		correlationID = ids.New()
	}

	var res Result
	err = s.writeTx(ctx, tenantID, func(tx pgx.Tx) error {
		// 1. Serialize all patches on this model. The lock is taken on the model row alone
		// and the current version read afterwards: with a join, a writer that waited for
		// the lock would re-check the join against the version row it saw before waiting
		// (READ COMMITTED re-evaluation) and wrongly find no row once current_version_id moved.
		var projectID, status, curVersionID string
		var curNum int
		err := tx.QueryRow(ctx, `
			SELECT project_id::text, status, current_version_id::text FROM architecture_models
			WHERE id = $1 AND tenant_id = $2
			FOR UPDATE`, modelID, tenantID).Scan(&projectID, &status, &curVersionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrModelNotFound
		}
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT version_number FROM architecture_versions WHERE id = $1`, curVersionID).Scan(&curNum); err != nil {
			return fmt.Errorf("read current version: %w", err)
		}

		// 2. Idempotency.
		var prevFP string
		var prevResult []byte
		err = tx.QueryRow(ctx, `SELECT request_fingerprint, COALESCE(result, 'null'::jsonb) FROM graph_patches
			WHERE model_id = $1 AND idempotency_key = $2`, modelID, p.IdempotencyKey).Scan(&prevFP, &prevResult)
		if err == nil {
			if prevFP != fingerprint {
				return ErrIdempotencyConflict
			}
			if err := json.Unmarshal(prevResult, &res); err != nil {
				return fmt.Errorf("decode stored result: %w", err)
			}
			res.Replayed = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		patchID := p.PatchID
		if patchID == "" {
			patchID = ids.New()
		} else {
			var used bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM graph_patches WHERE id = $1)`, patchID).Scan(&used); err != nil {
				return err
			}
			if used {
				return fmt.Errorf("%w: patch_id %s is already used", ErrInvalidPatch, patchID)
			}
		}
		opsJSON, err := json.Marshal(p.Operations)
		if err != nil {
			return err
		}
		invariants := p.ExpectedInvariants
		if invariants == nil {
			invariants = []string{}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO graph_patches (id, tenant_id, model_id, run_id, agent_task_id, base_version_number, idempotency_key,
			                           request_fingerprint, operations, expected_invariants, author_type, author_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			patchID, tenantID, modelID, nullable(p.RunID), nullable(p.AgentTaskID), *p.BaseGraphVersion, p.IdempotencyKey,
			fingerprint, opsJSON, invariants, author.Type, author.ID); err != nil {
			return fmt.Errorf("record patch: %w", err)
		}

		res = Result{
			SchemaVersion: "1.0", PatchID: patchID, ModelID: modelID, BaseGraphVersion: *p.BaseGraphVersion,
			CurrentGraphVersion: curNum, Changes: newChangeSummary(), Conflicts: []Conflict{},
			ValidationFindings: []Finding{}, InvalidatedArtifacts: []string{}, ImpactedComponents: []ImpactedComponent{},
		}

		cur, err := graphAtTx(ctx, tx, tenantID, modelID, curNum)
		if err != nil {
			return err
		}

		findings, err := s.provenanceFindings(ctx, tx, projectID, p)
		if err != nil {
			return err
		}
		if status != "ACTIVE" {
			findings = append(findings, Finding{OpIndex: -1, Code: "model_archived", Severity: "ERROR", Message: "the model is archived"})
		}
		invFindings, err := expectedInvariantFindings(ctx, tx, invariants)
		if err != nil {
			return err
		}
		findings = append(findings, invFindings...)
		refFindings, err := ledgerRefFindings(ctx, tx, modelID, p.Operations)
		if err != nil {
			return err
		}
		findings = append(findings, refFindings...)

		// 3. Optimistic concurrency.
		base := *p.BaseGraphVersion
		if base > curNum {
			findings = append(findings, Finding{OpIndex: -1, Code: "unknown_base_version", Severity: "ERROR",
				Message: fmt.Sprintf("base_graph_version %d does not exist (current is %d)", base, curNum)})
		}
		if base < curNum && !hasErrors(findings) {
			if err := s.conflict(ctx, tx, tenantID, modelID, p, cur, &res); err != nil {
				return err
			}
			return s.decide(ctx, tx, patchID, PatchConflict, &res, "", "")
		}

		// 4. Validate against the current graph.
		plan := newPlan(cur)
		everNodes, everEdges, err := everIDs(ctx, tx, modelID)
		if err != nil {
			return err
		}
		findings = append(findings, plan.validate(p.Operations, everNodes, everEdges)...)
		if !hasErrors(findings) && Hash(plan.nodeList(), plan.edgeList()) == cur.Version.GraphHash {
			findings = append(findings, Finding{OpIndex: -1, Code: "no_effect", Severity: "ERROR", Invariant: "patch_has_effect",
				Message: "the patch does not change the graph"})
		}
		res.ValidationFindings = findings
		if hasErrors(findings) {
			return s.decide(ctx, tx, patchID, PatchRejected, &res, "", "")
		}

		// 5. Apply: one new immutable version.
		next := curNum + 1
		nodes, edges := plan.nodeList(), plan.edgeList()
		hash := Hash(nodes, edges)
		var newVersionID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO architecture_versions (tenant_id, model_id, version_number, parent_version_id, graph_hash, node_count,
			                                   edge_count, change_summary, author_type, author_id, patch_id, run_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING id::text`,
			tenantID, modelID, next, curVersionID, hash, len(nodes), len(edges), plan.summary(), author.Type, author.ID,
			patchID, nullable(p.RunID)).Scan(&newVersionID); err != nil {
			return fmt.Errorf("insert version: %w", err)
		}
		if err := plan.write(ctx, tx, tenantID, modelID, next); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE architecture_models SET current_version_id = $2 WHERE id = $1`, modelID, newVersionID); err != nil {
			return fmt.Errorf("advance model: %w", err)
		}

		res.Changes = plan.changes
		res.NewGraphVersion = &next
		res.CurrentGraphVersion = next
		res.GraphHash = hash
		if res.ImpactedComponents, err = impacted(ctx, tx, modelID, cur, next, plan); err != nil {
			return err
		}

		changedNodes := append(append(slices.Clone(plan.changes.NodesAdded), plan.changes.NodesUpdated...), plan.changes.NodesRemoved...)
		changedEdges := append(slices.Clone(plan.changes.EdgesAdded), plan.changes.EdgesRemoved...)
		// Dependency engine (checklist 6.3): artifacts, decisions and claims that depend on
		// the changed nodes (transitively, bounded) go stale in this same transaction.
		inval, err := s.invalidate(ctx, invalidator{tx: tx, tenantID: tenantID, modelID: modelID, projectID: projectID,
			runID: p.RunID, cause: "patch:" + patchID, correlationID: correlationID, graphVersion: next}, cur, plan, s.impactLimits())
		if err != nil {
			return err
		}
		res.InvalidatedArtifacts = inval.Artifacts
		res.Invalidation = &inval
		if _, err := outbox.InsertTx(ctx, tx, outbox.EventEnvelope{
			EventType: EventVersionCreated, TenantID: tenantID, ProjectID: projectID, RunID: p.RunID,
			CorrelationID: correlationID, CausationID: patchID,
			Payload: map[string]any{
				"model_id": modelID, "graph_version": next, "previous_version": curNum, "patch_id": patchID,
				"graph_hash": hash, "changed_node_ids": changedNodes, "changed_edge_ids": changedEdges,
				"impacted_node_ids": inval.ImpactedNodes, "invalidated_artifact_ids": inval.Artifacts,
				"decisions_needing_review": inval.Decisions, "stale_claim_ids": inval.Claims, "traversal_truncated": inval.Truncated,
			},
		}); err != nil {
			return err
		}
		return s.decide(ctx, tx, patchID, PatchAccepted, &res, newVersionID, curVersionID)
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

func hasErrors(fs []Finding) bool {
	for _, f := range fs {
		if f.Severity == "ERROR" {
			return true
		}
	}
	return false
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func impactedIDs(cs []ImpactedComponent) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.NodeID
	}
	return out
}

// decide records the outcome on the patch row (frozen afterwards by the DB guard).
func (s *Service) decide(ctx context.Context, tx pgx.Tx, patchID, status string, res *Result, newVersionID, rollbackVersionID string) error {
	res.Status = status
	res.Accepted = status == PatchAccepted
	res.DecidedAt = s.now().UTC()
	raw, err := json.Marshal(res)
	if err != nil {
		return err
	}
	risk, _ := json.Marshal(map[string]any{
		"nodes_removed": len(res.Changes.NodesRemoved), "edges_removed": len(res.Changes.EdgesRemoved),
		"impacted_components": len(res.ImpactedComponents),
	})
	_, err = tx.Exec(ctx, `
		UPDATE graph_patches SET status = $2, decided_at = $3, result = $4, risk_summary = $5,
		       resulting_version_id = $6, rollback_version_id = $7
		WHERE id = $1`, patchID, status, res.DecidedAt, raw, risk, nullable(newVersionID), nullable(rollbackVersionID))
	return err
}

// provenanceFindings verifies run_id / agent_task_id belong to the model's project.
func (s *Service) provenanceFindings(ctx context.Context, tx pgx.Tx, projectID string, p Patch) ([]Finding, error) {
	var fs []Finding
	if p.RunID != "" {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM design_runs WHERE id = $1 AND project_id = $2)`, p.RunID, projectID).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			fs = append(fs, Finding{OpIndex: -1, Code: "unknown_run", Severity: "ERROR", Message: "run_id is not a run of this model's project"})
		}
	}
	if p.AgentTaskID != "" {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_tasks WHERE id = $1 AND run_id = $2)`, p.AgentTaskID, p.RunID).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			fs = append(fs, Finding{OpIndex: -1, Code: "unknown_agent_task", Severity: "ERROR", Message: "agent_task_id is not a task of run_id"})
		}
	}
	return fs, nil
}

func expectedInvariantFindings(ctx context.Context, tx pgx.Tx, keys []string) ([]Finding, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT key, active FROM graph_invariants WHERE key = ANY($1)`, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	known := map[string]bool{}
	for rows.Next() {
		var k string
		var active bool
		if err := rows.Scan(&k, &active); err != nil {
			return nil, err
		}
		known[k] = active
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var fs []Finding
	for _, k := range keys {
		active, ok := known[k]
		switch {
		case !ok:
			fs = append(fs, Finding{OpIndex: -1, Code: "unknown_invariant", Severity: "ERROR", Invariant: k, Message: "expected invariant is not registered"})
		case !active:
			fs = append(fs, Finding{OpIndex: -1, Code: "inactive_invariant", Severity: "ERROR", Invariant: k, Message: "expected invariant is not enforced yet"})
		}
	}
	return fs, nil
}

func everIDs(ctx context.Context, tx pgx.Tx, modelID string) (map[string]bool, map[string]bool, error) {
	collect := func(q string) (map[string]bool, error) {
		rows, err := tx.Query(ctx, q, modelID)
		if err != nil {
			return nil, err
		}
		list, err := pgx.CollectRows(rows, pgx.RowTo[string])
		out := make(map[string]bool, len(list))
		for _, id := range list {
			out[id] = true
		}
		return out, err
	}
	n, err := collect(`SELECT DISTINCT node_id::text FROM architecture_nodes WHERE model_id = $1`)
	if err != nil {
		return nil, nil, err
	}
	e, err := collect(`SELECT DISTINCT edge_id::text FROM architecture_edges WHERE model_id = $1`)
	return n, e, err
}

// conflict fills res with the reasons a stale-base patch overlaps later changes.
func (s *Service) conflict(ctx context.Context, tx pgx.Tx, tenantID, modelID string, p Patch, cur Graph, res *Result) error {
	base, err := graphAtTx(ctx, tx, tenantID, modelID, *p.BaseGraphVersion)
	if err != nil {
		return err
	}
	d := DiffGraphs(base, cur)
	since := newChangeSummary()
	touchedNodes := map[string]int{} // node id -> version it changed in (filled below)
	for _, n := range d.Nodes.Added {
		since.NodesAdded = append(since.NodesAdded, n.ID)
	}
	for _, n := range d.Nodes.Removed {
		since.NodesRemoved = append(since.NodesRemoved, n.ID)
		touchedNodes[n.ID] = 0
	}
	for _, c := range d.Nodes.Modified {
		since.NodesUpdated = append(since.NodesUpdated, c.ID)
		touchedNodes[c.ID] = 0
	}
	touchedEdges := map[string]int{}
	for _, e := range d.Edges.Added {
		since.EdgesAdded = append(since.EdgesAdded, e.ID)
	}
	for _, e := range d.Edges.Removed {
		since.EdgesRemoved = append(since.EdgesRemoved, e.ID)
		touchedEdges[e.ID] = 0
	}
	res.ChangesSinceBase = &since

	// The version in which each base revision was closed is when it changed.
	closedIn := func(table, col string, ids map[string]int) error {
		if len(ids) == 0 {
			return nil
		}
		list := make([]string, 0, len(ids))
		for id := range ids {
			list = append(list, id)
		}
		rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT %s::text, valid_to_version FROM %s
			WHERE model_id = $1 AND %s = ANY($2::uuid[]) AND valid_from_version <= $3
			  AND valid_to_version IS NOT NULL AND valid_to_version > $3`, col, table, col), modelID, list, *p.BaseGraphVersion)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			var v int
			if err := rows.Scan(&id, &v); err != nil {
				return err
			}
			ids[id] = v
		}
		return rows.Err()
	}
	if err := closedIn("architecture_nodes", "node_id", touchedNodes); err != nil {
		return err
	}
	if err := closedIn("architecture_edges", "edge_id", touchedEdges); err != nil {
		return err
	}

	removed := map[string]bool{}
	for _, n := range d.Nodes.Removed {
		removed[n.ID] = true
	}
	baseKeys := map[string]bool{}
	for _, n := range base.Nodes {
		baseKeys[n.Key] = true
	}
	curKeys := map[string]bool{}
	for _, n := range cur.Nodes {
		curKeys[n.Key] = true
	}
	add := func(i int, target, reason string, v int) {
		res.Conflicts = append(res.Conflicts, Conflict{OpIndex: i, TargetID: target, Reason: reason, ChangedInVersion: v})
	}
	for i, op := range p.Operations {
		switch op.Op {
		case OpUpdateNode, OpRemoveNode:
			if v, ok := touchedNodes[op.TargetID]; ok {
				reason := "modified_since_base"
				if removed[op.TargetID] {
					reason = "removed_since_base"
				}
				add(i, op.TargetID, reason, v)
			}
		case OpRemoveEdge:
			if v, ok := touchedEdges[op.TargetID]; ok {
				add(i, op.TargetID, "removed_since_base", v)
			}
		case OpAddNode:
			var pl AddNodePayload
			if decodeStrict(op.Payload, &pl) == nil && curKeys[pl.Key] && !baseKeys[pl.Key] {
				add(i, op.TargetID, "key_taken_since_base", 0)
			}
		case OpAddEdge:
			var pl AddEdgePayload
			if decodeStrict(op.Payload, &pl) == nil {
				for _, end := range []string{pl.SourceNodeID, pl.TargetNodeID} {
					if removed[end] {
						add(i, op.TargetID, "endpoint_removed_since_base", touchedNodes[end])
						break
					}
				}
			}
		}
	}
	return nil
}

// impacted returns the changed nodes plus everything that (transitively) depends on them:
// an inbound traversal at the new version, and at the old version for removed nodes.
func impacted(ctx context.Context, tx pgx.Tx, modelID string, before Graph, next int, plan *plan) ([]ImpactedComponent, error) {
	liveStarts := map[string]bool{}
	for _, id := range append(slices.Clone(plan.changes.NodesAdded), plan.changes.NodesUpdated...) {
		liveStarts[id] = true
	}
	for _, id := range append(slices.Clone(plan.changes.EdgesAdded), plan.changes.EdgesRemoved...) {
		e, ok := plan.edges[id]
		if !ok {
			e = plan.removedEdges[id]
		}
		for _, end := range []string{e.SourceNodeID, e.TargetNodeID} {
			if _, live := plan.nodes[end]; live {
				liveStarts[end] = true
			}
		}
	}
	out := map[string]ImpactedComponent{}
	walk := func(version int, starts []string) error {
		if len(starts) == 0 {
			return nil
		}
		nodes, err := reachableTx(ctx, tx, modelID, version, starts, Inbound, nil, maxImpacted, true)
		for _, n := range nodes {
			out[n.ID] = ImpactedComponent{NodeID: n.ID, Key: n.Key, Type: n.Type}
		}
		return err
	}
	if err := walk(next, keys(liveStarts)); err != nil {
		return nil, err
	}
	if err := walk(before.Version.Number, plan.changes.NodesRemoved); err != nil {
		return nil, err
	}
	list := make([]ImpactedComponent, 0, len(out))
	for _, c := range out {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
	if len(list) > maxImpacted {
		list = list[:maxImpacted]
	}
	return list, nil
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Plan: validate operations against a working copy, then write rows.
// ---------------------------------------------------------------------------

type plan struct {
	nodes        map[string]Node
	edges        map[string]Edge
	keyIndex     map[string]string
	baseNodes    map[string]Node
	removedEdges map[string]Edge
	changes      ChangeSummary
	added        map[string]bool // node ids added by this patch
	updated      map[string]bool
}

func newPlan(g Graph) *plan {
	p := &plan{nodes: map[string]Node{}, edges: map[string]Edge{}, keyIndex: map[string]string{}, baseNodes: map[string]Node{},
		removedEdges: map[string]Edge{}, changes: newChangeSummary(), added: map[string]bool{}, updated: map[string]bool{}}
	for _, n := range g.Nodes {
		p.nodes[n.ID], p.baseNodes[n.ID], p.keyIndex[n.Key] = n, n, n.ID
	}
	for _, e := range g.Edges {
		p.edges[e.ID] = e
	}
	return p
}

func (p *plan) nodeList() []Node {
	out := make([]Node, 0, len(p.nodes))
	for _, n := range p.nodes {
		out = append(out, n)
	}
	return out
}

func (p *plan) edgeList() []Edge {
	out := make([]Edge, 0, len(p.edges))
	for _, e := range p.edges {
		out = append(out, e)
	}
	return out
}

func (p *plan) summary() string {
	var parts []string
	for _, c := range []struct {
		n    int
		what string
	}{
		{len(p.changes.NodesAdded), "node(s) added"}, {len(p.changes.NodesUpdated), "node(s) updated"},
		{len(p.changes.NodesRemoved), "node(s) removed"}, {len(p.changes.EdgesAdded), "edge(s) added"},
		{len(p.changes.EdgesRemoved), "edge(s) removed"},
	} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.what))
		}
	}
	return strings.Join(parts, ", ")
}

func oneOf(v string, allowed []string) bool { return slices.Contains(allowed, v) }

func (p *plan) validate(ops []Operation, everNodes, everEdges map[string]bool) []Finding {
	var fs []Finding
	touched := map[string]bool{}
	for i, op := range ops {
		f := func(code, format string, args ...any) {
			fs = append(fs, Finding{OpIndex: i, TargetID: op.TargetID, Code: code, Severity: "ERROR", Message: fmt.Sprintf(format, args...)})
		}
		if strings.TrimSpace(op.Rationale) == "" {
			f("missing_rationale", "every operation needs a rationale")
		}
		for _, ref := range op.EvidenceRefs {
			if strings.TrimSpace(ref) == "" || len(ref) > 500 {
				f("invalid_evidence_ref", "evidence_refs must be non-empty strings of at most 500 characters")
				break
			}
		}
		if !ids.Valid(op.TargetID) {
			f("invalid_target_id", "target_id must be a UUID")
			continue
		}
		if touched[op.TargetID] {
			f("duplicate_target", "a patch may contain only one operation per target")
			continue
		}
		touched[op.TargetID] = true

		switch op.Op {
		case OpAddNode:
			var pl AddNodePayload
			if err := decodeStrict(op.Payload, &pl); err != nil {
				f("invalid_payload", "add_node payload: %v", err)
				continue
			}
			if everNodes[op.TargetID] {
				f("id_reused", "node ids are immutable and never reused; generate a new id")
				continue
			}
			n := Node{ID: op.TargetID, Key: pl.Key, Type: pl.Type, Name: pl.Name, Description: pl.Description,
				Properties: nonNil(pl.Properties), OwnerDomain: pl.OwnerDomain, LifecycleState: orDefault(pl.LifecycleState, "PROPOSED"),
				EvidenceState: orDefault(pl.EvidenceState, "PROPOSED"), Revision: 1}
			if !p.checkNode(n, f) || !checkEvidenceRequirements(n.EvidenceState, op, f, fsPtr(&fs)) {
				continue
			}
			p.nodes[n.ID], p.keyIndex[n.Key], p.added[n.ID] = n, n.ID, true
			p.changes.NodesAdded = append(p.changes.NodesAdded, n.ID)

		case OpUpdateNode:
			old, ok := p.nodes[op.TargetID]
			if !ok {
				f("unknown_node", "node does not exist at the base version")
				continue
			}
			var pl UpdateNodePayload
			if err := decodeStrict(op.Payload, &pl); err != nil {
				f("invalid_payload", "update_node payload: %v (a node's type cannot be changed)", err)
				continue
			}
			n := old
			if pl.Key != nil {
				n.Key = *pl.Key
			}
			if pl.Name != nil {
				n.Name = *pl.Name
			}
			if pl.Description != nil {
				n.Description = *pl.Description
			}
			if pl.Properties != nil {
				n.Properties = nonNil(*pl.Properties)
			}
			if pl.OwnerDomain != nil {
				n.OwnerDomain = *pl.OwnerDomain
			}
			if pl.LifecycleState != nil {
				n.LifecycleState = *pl.LifecycleState
			}
			if pl.EvidenceState != nil {
				n.EvidenceState = *pl.EvidenceState
			}
			delete(p.keyIndex, old.Key)
			if !p.checkNode(n, f) || !checkEvidenceRequirements(n.EvidenceState, op, f, fsPtr(&fs)) {
				p.keyIndex[old.Key] = old.ID
				continue
			}
			if Hash([]Node{n}, nil) == Hash([]Node{old}, nil) {
				fs = append(fs, Finding{OpIndex: i, TargetID: op.TargetID, Code: "update_no_effect", Severity: "WARNING", Message: "update does not change the node"})
			}
			n.Revision = old.Revision + 1
			p.nodes[n.ID], p.keyIndex[n.Key], p.updated[n.ID] = n, n.ID, true
			p.changes.NodesUpdated = append(p.changes.NodesUpdated, n.ID)

		case OpRemoveNode:
			old, ok := p.nodes[op.TargetID]
			if !ok {
				f("unknown_node", "node does not exist at the base version")
				continue
			}
			var pl RemoveNodePayload
			if err := decodeStrict(op.Payload, &pl); err != nil {
				f("invalid_payload", "remove_node payload: %v", err)
				continue
			}
			var attached []string
			for id, e := range p.edges {
				if e.SourceNodeID == old.ID || e.TargetNodeID == old.ID {
					attached = append(attached, id)
				}
			}
			sort.Strings(attached)
			if len(attached) > 0 && !pl.CascadeEdges {
				fs = append(fs, Finding{OpIndex: i, TargetID: op.TargetID, Code: "node_has_edges", Severity: "ERROR", Invariant: "edge_endpoints_exist",
					Message: fmt.Sprintf("node still has %d edge(s) %v; remove them or set cascade_edges", len(attached), attached)})
				continue
			}
			for _, id := range attached {
				p.removedEdges[id] = p.edges[id]
				delete(p.edges, id)
				p.changes.EdgesRemoved = append(p.changes.EdgesRemoved, id)
			}
			delete(p.nodes, old.ID)
			delete(p.keyIndex, old.Key)
			p.changes.NodesRemoved = append(p.changes.NodesRemoved, old.ID)

		case OpAddEdge:
			var pl AddEdgePayload
			if err := decodeStrict(op.Payload, &pl); err != nil {
				f("invalid_payload", "add_edge payload: %v", err)
				continue
			}
			if everEdges[op.TargetID] {
				f("id_reused", "edge ids are immutable and never reused; generate a new id")
				continue
			}
			e := Edge{ID: op.TargetID, SourceNodeID: pl.SourceNodeID, TargetNodeID: pl.TargetNodeID, RelationType: pl.RelationType,
				Direction: orDefault(pl.Direction, "DIRECTED"), Protocol: pl.Protocol, SyncMode: pl.SyncMode,
				Constraints: nonNil(pl.Constraints), Properties: nonNil(pl.Properties), Revision: 1}
			if !p.checkEdge(e, f) {
				continue
			}
			p.edges[e.ID] = e
			p.changes.EdgesAdded = append(p.changes.EdgesAdded, e.ID)

		case OpRemoveEdge:
			e, ok := p.edges[op.TargetID]
			if !ok {
				f("unknown_edge", "edge does not exist at the base version")
				continue
			}
			if err := decodeStrict(op.Payload, &struct{}{}); err != nil {
				f("invalid_payload", "remove_edge takes an empty payload: %v", err)
				continue
			}
			p.removedEdges[e.ID] = e
			delete(p.edges, e.ID)
			p.changes.EdgesRemoved = append(p.changes.EdgesRemoved, e.ID)

		default:
			f("unknown_op", "op must be one of add_node, update_node, remove_node, add_edge, remove_edge")
		}
	}

	// Edges added earlier in the patch must not dangle after later removals.
	degree := make(map[string]int, len(p.nodes))
	for id, e := range p.edges {
		_, s := p.nodes[e.SourceNodeID]
		_, t := p.nodes[e.TargetNodeID]
		if !s || !t {
			fs = append(fs, Finding{OpIndex: -1, TargetID: id, Code: "dangling_edge", Severity: "ERROR", Invariant: "edge_endpoints_exist",
				Message: "edge would reference a node that no longer exists"})
			continue
		}
		degree[e.SourceNodeID]++
		degree[e.TargetNodeID]++
	}

	// Checklist 3.5: structural sub-elements must not be orphaned.
	for id, n := range p.nodes {
		switch n.Type {
		case "api", "table", "event", "topic":
			if degree[id] == 0 {
				fs = append(fs, Finding{
					OpIndex: -1, TargetID: id, Code: "orphaned_required_relation", Severity: "ERROR",
					Invariant: "no_orphaned_required_relations",
					Message:   fmt.Sprintf("%s node %q is orphaned and requires at least one relation", n.Type, n.Key),
				})
			}
		}
	}
	return fs
}

func fsPtr(fs *[]Finding) *[]Finding { return fs }

// checkEvidenceRequirements enforces Checklist 3.4:
//   - OBSERVED claims/nodes require at least one direct evidence_ref.
//   - INFERRED claims/nodes require both evidence_refs and non-empty rationale.
//   - PROPOSED claims/nodes are recommendations.
func checkEvidenceRequirements(state string, op Operation, _ func(code, format string, args ...any), fs *[]Finding) bool {
	switch state {
	case "OBSERVED":
		if len(op.EvidenceRefs) == 0 {
			*fs = append(*fs, Finding{
				TargetID: op.TargetID, Code: "missing_evidence", Severity: "ERROR",
				Invariant: "claim_evidence_requirements",
				Message:   "OBSERVED nodes require at least one evidence_ref",
			})
			return false
		}
	case "INFERRED":
		if len(op.EvidenceRefs) == 0 || strings.TrimSpace(op.Rationale) == "" {
			*fs = append(*fs, Finding{
				TargetID: op.TargetID, Code: "missing_evidence_or_reasoning", Severity: "ERROR",
				Invariant: "claim_evidence_requirements",
				Message:   "INFERRED nodes require both evidence_refs and rationale",
			})
			return false
		}
	}
	return true
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func (p *plan) checkNode(n Node, f func(code, format string, args ...any)) bool {
	ok := true
	if !nodeKeyPattern.MatchString(n.Key) {
		f("invalid_key", "key %q must match %s", n.Key, nodeKeyPattern)
		ok = false
	} else if other, taken := p.keyIndex[n.Key]; taken && other != n.ID {
		f("duplicate_key", "key %q is already used by node %s", n.Key, other)
		ok = false
	}
	if !validNodeType(n.Type) {
		f("invalid_node_type", "type %q is not one of %v", n.Type, NodeTypes)
		ok = false
	}
	if l := utf8.RuneCountInString(n.Name); l < 1 || l > 200 {
		f("invalid_name", "name must be 1-200 characters")
		ok = false
	}
	if !oneOf(n.LifecycleState, LifecycleStates) {
		f("invalid_lifecycle_state", "lifecycle_state must be one of %v", LifecycleStates)
		ok = false
	}
	if !oneOf(n.EvidenceState, EvidenceStates) {
		f("invalid_evidence_state", "evidence_state must be one of %v", EvidenceStates)
		ok = false
	}
	return ok
}

func (p *plan) checkEdge(e Edge, f func(code, format string, args ...any)) bool {
	src, sok := p.nodes[e.SourceNodeID]
	dst, dok := p.nodes[e.TargetNodeID]
	switch {
	case !ids.Valid(e.SourceNodeID) || !ids.Valid(e.TargetNodeID):
		f("invalid_endpoint", "source_node_id and target_node_id must be UUIDs")
		return false
	case e.SourceNodeID == e.TargetNodeID:
		f("self_loop", "an edge cannot connect a node to itself")
		return false
	case !sok || !dok:
		f("unknown_endpoint", "edge endpoints must exist (at the base version or added earlier in this patch)")
		return false
	case !validRelationType(e.RelationType):
		f("invalid_relation_type", "relation_type %q is not one of %v", e.RelationType, RelationTypes)
		return false
	}
	ok := true
	if err := CheckRelation(e.RelationType, src.Type, dst.Type); err != nil {
		f("incompatible_edge", "%v", err)
		ok = false
	}
	if !oneOf(e.Direction, EdgeDirections) {
		f("invalid_direction", "direction must be one of %v", EdgeDirections)
		ok = false
	}
	if e.SyncMode != "" && !oneOf(e.SyncMode, SyncModes) {
		f("invalid_sync_mode", "sync_mode must be one of %v", SyncModes)
		ok = false
	}
	if len(e.Protocol) > 64 {
		f("invalid_protocol", "protocol must be at most 64 characters")
		ok = false
	}
	for id, other := range p.edges {
		if id != e.ID && other.SourceNodeID == e.SourceNodeID && other.TargetNodeID == e.TargetNodeID && other.RelationType == e.RelationType {
			f("duplicate_edge", "an identical %s edge %s already exists", e.RelationType, id)
			ok = false
			break
		}
	}
	return ok
}

// write persists the plan at version next: close superseded rows, append new ones.
func (p *plan) write(ctx context.Context, tx pgx.Tx, tenantID, modelID string, next int) error {
	closeNodes := append(slices.Clone(p.changes.NodesUpdated), p.changes.NodesRemoved...)
	if len(closeNodes) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE architecture_nodes SET valid_to_version = $3
			WHERE model_id = $1 AND node_id = ANY($2::uuid[]) AND valid_to_version IS NULL`, modelID, closeNodes, next); err != nil {
			return fmt.Errorf("close node revisions: %w", err)
		}
	}
	if len(p.changes.EdgesRemoved) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE architecture_edges SET valid_to_version = $3
			WHERE model_id = $1 AND edge_id = ANY($2::uuid[]) AND valid_to_version IS NULL`, modelID, p.changes.EdgesRemoved, next); err != nil {
			return fmt.Errorf("close edge revisions: %w", err)
		}
	}
	batch := &pgx.Batch{}
	for _, id := range append(slices.Clone(p.changes.NodesAdded), p.changes.NodesUpdated...) {
		n := p.nodes[id]
		props, err := json.Marshal(n.Properties)
		if err != nil {
			return err
		}
		batch.Queue(`
			INSERT INTO architecture_nodes (tenant_id, model_id, node_id, node_key, node_type, name, description, properties,
			                                owner_domain, lifecycle_state, evidence_state, revision, valid_from_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			tenantID, modelID, n.ID, n.Key, n.Type, n.Name, n.Description, props, nullable(n.OwnerDomain),
			n.LifecycleState, n.EvidenceState, n.Revision, next)
	}
	for _, id := range p.changes.EdgesAdded {
		e := p.edges[id]
		cons, err := json.Marshal(e.Constraints)
		if err != nil {
			return err
		}
		props, err := json.Marshal(e.Properties)
		if err != nil {
			return err
		}
		batch.Queue(`
			INSERT INTO architecture_edges (tenant_id, model_id, edge_id, source_node_id, target_node_id, relation_type, direction,
			                                protocol, sync_mode, constraints, properties, revision, valid_from_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 1, $12)`,
			tenantID, modelID, e.ID, e.SourceNodeID, e.TargetNodeID, e.RelationType, e.Direction,
			nullable(e.Protocol), nullable(e.SyncMode), cons, props, next)
	}
	if batch.Len() == 0 {
		return nil
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("append graph rows: %w", err)
	}
	return nil
}

// GetPatch returns a recorded patch's result.
func (s *Service) GetPatch(ctx context.Context, tenantID, modelID, patchID string) (Result, error) {
	if !ids.Valid(modelID) || !ids.Valid(patchID) {
		return Result{}, ErrPatchNotFound
	}
	var raw []byte
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(result, 'null'::jsonb) FROM graph_patches WHERE id = $1 AND model_id = $2`, patchID, modelID).Scan(&raw)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrPatchNotFound
	}
	if err != nil {
		return Result{}, err
	}
	var res Result
	return res, json.Unmarshal(raw, &res)
}
