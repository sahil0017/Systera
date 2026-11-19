package graph

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
)

// Event types emitted by the dependency engine.
const (
	EventArtifactInvalidated = "artifact.invalidated"
	EventDecisionNeedsReview = "decision.needs_review" // same name as internal/decisions
	EventClaimStale          = "claim.stale"           // same name as internal/evidence
)

// ImpactLimits bound an impact traversal (checklist 6.5).
type ImpactLimits struct {
	MaxDepth      int      // hops from the changed nodes (default 25)
	MaxNodes      int      // affected nodes before the walk is truncated (default 10000)
	RelationTypes []string // relations that carry dependency (default: all)
}

// DefaultImpactLimits are used by patch application.
var DefaultImpactLimits = ImpactLimits{MaxDepth: 25, MaxNodes: 10000}

func (l ImpactLimits) normalized() ImpactLimits {
	if l.MaxDepth <= 0 {
		l.MaxDepth = DefaultImpactLimits.MaxDepth
	}
	if l.MaxNodes <= 0 {
		l.MaxNodes = DefaultImpactLimits.MaxNodes
	}
	if l.RelationTypes == nil {
		l.RelationTypes = []string{}
	}
	return l
}

// ImpactSet is the result of a bounded inbound traversal: the changed nodes plus every
// node that (transitively) depends on them. Every relation type points from dependent
// (source) to dependency (target), so dependents are found by walking edges backwards.
type ImpactSet struct {
	Depth     map[string]int // node id -> hops from the nearest changed node (0 = changed)
	MaxDepth  int            // deepest level reached
	Truncated bool           // a limit was hit; the set is incomplete
}

func (s ImpactSet) IDs() []string {
	out := make([]string, 0, len(s.Depth))
	for id := range s.Depth {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// impactTraversal walks breadth-first from starts at a version, one query per level, so
// depth and node limits are exact and the cost is O(affected nodes + their in-edges).
func impactTraversal(ctx context.Context, tx pgx.Tx, modelID string, version int, starts []string, lim ImpactLimits) (ImpactSet, error) {
	return TraverseTx(ctx, tx, modelID, version, starts, Inbound, lim)
}

// TraverseTx is the bounded breadth-first traversal behind impact analysis. Inbound finds
// dependents (impact); Outbound finds dependencies (an artifact's dependency closure).
func TraverseTx(ctx context.Context, tx pgx.Tx, modelID string, version int, starts []string, direction Direction, lim ImpactLimits) (ImpactSet, error) {
	if direction != Inbound && direction != Outbound {
		return ImpactSet{}, fmt.Errorf("%w: traversal direction must be in or out", ErrInvalidQuery)
	}
	lim = lim.normalized()
	set := ImpactSet{Depth: map[string]int{}}
	var frontier []string
	for _, s := range starts {
		if _, ok := set.Depth[s]; !ok {
			set.Depth[s] = 0
			frontier = append(frontier, s)
		}
	}
	for d := 1; len(frontier) > 0; d++ {
		// At d = MaxDepth+1 the level is only probed: any unseen node there means the
		// walk is cut short by the depth limit.
		probe := d > lim.MaxDepth
		// Two index-friendly halves (idx_architecture_edges_in / _out): the edges that lead
		// in the walk direction, plus BIDIRECTIONAL edges seen from their other end.
		near, far := "target_node_id", "source_node_id"
		if direction == Outbound {
			near, far = far, near
		}
		rows, err := tx.Query(ctx, `
			SELECT source_node_id::text, target_node_id::text, direction FROM architecture_edges
			WHERE model_id = $1 AND `+near+` = ANY($3::uuid[])
			  AND valid_from_version <= $2 AND (valid_to_version IS NULL OR valid_to_version > $2)
			  AND (cardinality($4::text[]) = 0 OR relation_type = ANY($4::text[]))
			UNION ALL
			SELECT source_node_id::text, target_node_id::text, direction FROM architecture_edges
			WHERE model_id = $1 AND `+far+` = ANY($3::uuid[]) AND direction = 'BIDIRECTIONAL'
			  AND valid_from_version <= $2 AND (valid_to_version IS NULL OR valid_to_version > $2)
			  AND (cardinality($4::text[]) = 0 OR relation_type = ANY($4::text[]))`,
			modelID, version, frontier, lim.RelationTypes)
		if err != nil {
			return set, err
		}
		inFrontier := make(map[string]bool, len(frontier))
		for _, f := range frontier {
			inFrontier[f] = true
		}
		var next []string
		for rows.Next() {
			var src, dst, dir string
			if err := rows.Scan(&src, &dst, &dir); err != nil {
				rows.Close()
				return set, err
			}
			// Edges point dependent -> dependency; BIDIRECTIONAL edges count both ways.
			bidi := dir == "BIDIRECTIONAL"
			cands := []string{}
			if inFrontier[dst] && (direction == Inbound || bidi) {
				cands = append(cands, src)
			}
			if inFrontier[src] && (direction == Outbound || bidi) {
				cands = append(cands, dst)
			}
			for _, c := range cands {
				if _, seen := set.Depth[c]; seen {
					continue
				}
				if probe {
					set.Truncated = true
					continue
				}
				if len(set.Depth) >= lim.MaxNodes {
					set.Truncated = true
					continue
				}
				set.Depth[c] = d
				next = append(next, c)
			}
		}
		if err := rows.Err(); err != nil {
			return set, err
		}
		if len(next) > 0 {
			set.MaxDepth = d
		}
		if probe || (set.Truncated && len(set.Depth) >= lim.MaxNodes) {
			break
		}
		frontier = next
	}
	return set, nil
}

// Invalidation is what a change made stale.
type Invalidation struct {
	ImpactedNodes   []string `json:"impacted_node_ids"`
	Artifacts       []string `json:"invalidated_artifact_ids"`
	Decisions       []string `json:"decisions_needing_review"`
	Claims          []string `json:"stale_claim_ids"`
	Truncated       bool     `json:"traversal_truncated"`
	RevalidationReq string   `json:"revalidation_request_id,omitempty"`
}

type invalidator struct {
	tx                           pgx.Tx
	tenantID, modelID, projectID string
	runID, cause, correlationID  string
	graphVersion                 int
}

// invalidate applies the dependency engine to one accepted patch inside its transaction:
// everything that depends on the changed nodes / edges becomes stale.
func (s *Service) invalidate(ctx context.Context, inv invalidator, before Graph, plan *plan, lim ImpactLimits) (Invalidation, error) {
	var out Invalidation
	// Live starting points at the new version, removed nodes at the previous one.
	startsNext := map[string]bool{}
	for _, id := range append(slices.Clone(plan.changes.NodesAdded), plan.changes.NodesUpdated...) {
		startsNext[id] = true
	}
	startsPrev := map[string]bool{}
	for _, id := range plan.changes.NodesRemoved {
		startsPrev[id] = true
	}
	for _, id := range append(slices.Clone(plan.changes.EdgesAdded), plan.changes.EdgesRemoved...) {
		e, ok := plan.edges[id]
		if !ok {
			e = plan.removedEdges[id]
		}
		for _, end := range []string{e.SourceNodeID, e.TargetNodeID} {
			if _, live := plan.nodes[end]; live {
				startsNext[end] = true
			} else {
				startsPrev[end] = true
			}
		}
	}
	nextSet, err := impactTraversal(ctx, inv.tx, inv.modelID, inv.graphVersion, keys(startsNext), lim)
	if err != nil {
		return out, err
	}
	prevSet, err := impactTraversal(ctx, inv.tx, inv.modelID, before.Version.Number, keys(startsPrev), lim)
	if err != nil {
		return out, err
	}
	affected := map[string]bool{}
	for id := range nextSet.Depth {
		affected[id] = true
	}
	for id := range prevSet.Depth {
		affected[id] = true
	}
	out.ImpactedNodes = keys(affected)
	out.Truncated = nextSet.Truncated || prevSet.Truncated
	changedEdges := append(slices.Clone(plan.changes.EdgesAdded), plan.changes.EdgesRemoved...)
	reason := fmt.Sprintf("graph version %d changed %d node(s) and %d edge(s) it depends on",
		inv.graphVersion, len(plan.changes.NodesAdded)+len(plan.changes.NodesUpdated)+len(plan.changes.NodesRemoved), len(changedEdges))
	if out.Truncated {
		reason = fmt.Sprintf("graph version %d: impact traversal exceeded its limits (depth %d, %d nodes); invalidated conservatively",
			inv.graphVersion, lim.normalized().MaxDepth, lim.normalized().MaxNodes)
	}
	if out.Claims, err = staleClaims(ctx, inv, out.ImpactedNodes, out.Truncated, reason); err != nil {
		return out, err
	}
	if out.Decisions, err = flagDecisions(ctx, inv, out.ImpactedNodes, out.Truncated, reason); err != nil {
		return out, err
	}
	if out.Artifacts, err = staleArtifacts(ctx, inv, out.ImpactedNodes, changedEdges, out.Decisions, out.Claims, out.Truncated, reason); err != nil {
		return out, err
	}
	if len(out.Artifacts) > 0 {
		if err := inv.tx.QueryRow(ctx, `
			INSERT INTO revalidation_requests (tenant_id, model_id, artifact_ids, graph_version, cause, reason)
			VALUES ($1, $2, $3::uuid[], $4, $5, $6) RETURNING id::text`,
			inv.tenantID, inv.modelID, out.Artifacts, inv.graphVersion, inv.cause, reason).Scan(&out.RevalidationReq); err != nil {
			return out, fmt.Errorf("queue revalidation: %w", err)
		}
	}
	return out, nil
}

func (inv invalidator) emit(ctx context.Context, eventType string, payload map[string]any) error {
	payload["model_id"] = inv.modelID
	payload["graph_version"] = inv.graphVersion
	payload["cause"] = inv.cause
	_, err := outbox.InsertTx(ctx, inv.tx, outbox.EventEnvelope{
		EventType: eventType, TenantID: inv.tenantID, ProjectID: inv.projectID, RunID: inv.runID,
		CorrelationID: inv.correlationID, Payload: payload,
	})
	return err
}

func collectIDs(rows pgx.Rows) ([]string, error) {
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if list == nil {
		list = []string{}
	}
	sort.Strings(list)
	return list, err
}

// staleClaims marks ACTIVE claims about affected nodes STALE.
func staleClaims(ctx context.Context, inv invalidator, affected []string, conservative bool, reason string) ([]string, error) {
	rows, err := inv.tx.Query(ctx, `
		UPDATE claims SET lifecycle = 'STALE', stale_reason = $4, stale_since = NOW()
		WHERE model_id = $1 AND lifecycle = 'ACTIVE' AND cardinality(subject_node_ids) > 0
		  AND ($3 OR subject_node_ids && $2::uuid[])
		RETURNING id::text`, inv.modelID, affected, conservative, "subject architecture changed: "+reason)
	if err != nil {
		return nil, fmt.Errorf("stale claims: %w", err)
	}
	ids, err := collectIDs(rows)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err := inv.emit(ctx, EventClaimStale, map[string]any{"claim_id": id, "reason": "subject architecture changed"}); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// flagDecisions moves PROPOSED / ACCEPTED ADRs about affected nodes to NEEDS_REVIEW.
func flagDecisions(ctx context.Context, inv invalidator, affected []string, conservative bool, reason string) ([]string, error) {
	rows, err := inv.tx.Query(ctx, `
		UPDATE decisions SET status = 'NEEDS_REVIEW', review_reason = $4, updated_at = NOW()
		WHERE model_id = $1 AND tenant_id = $5 AND status IN ('PROPOSED', 'ACCEPTED') AND cardinality(affected_nodes) > 0
		  AND ($3 OR affected_nodes && $2::uuid[])
		RETURNING id::text`, inv.modelID, affected, conservative, "affected architecture nodes changed: "+reason, inv.tenantID)
	if err != nil {
		return nil, fmt.Errorf("flag decisions: %w", err)
	}
	ids, err := collectIDs(rows)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		if err := inv.emit(ctx, EventDecisionNeedsReview, map[string]any{"decision_ids": ids, "reason": reason}); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// staleArtifacts marks CURRENT artifacts stale whose current version depends on an
// affected node, a changed edge, a flagged decision or a stale claim, and then, through
// ARTIFACT dependencies, every artifact downstream of those.
func staleArtifacts(ctx context.Context, inv invalidator, affected, changedEdges, decisions, claims []string, conservative bool, reason string) ([]string, error) {
	seeds := map[string]string{} // artifact id -> reason
	if conservative {
		rows, err := inv.tx.Query(ctx, `SELECT id::text FROM artifacts WHERE model_id = $1 AND status = 'CURRENT'`, inv.modelID)
		if err != nil {
			return nil, err
		}
		all, err := collectIDs(rows)
		if err != nil {
			return nil, err
		}
		for _, id := range all {
			seeds[id] = reason
		}
	} else {
		rows, err := inv.tx.Query(ctx, `
			SELECT DISTINCT a.id::text,
			       CASE d.dep_type WHEN 'NODE' THEN 'depends on a changed node'
			                       WHEN 'EDGE' THEN 'depends on a changed edge'
			                       WHEN 'DECISION' THEN 'depends on a decision now needing review'
			                       ELSE 'depends on a claim that went stale' END
			FROM artifacts a JOIN artifact_dependencies d ON d.artifact_version_id = a.current_version_id
			WHERE a.model_id = $1 AND a.status = 'CURRENT' AND (
			      (d.dep_type = 'NODE' AND d.target_id = ANY($2::uuid[]))
			   OR (d.dep_type = 'EDGE' AND d.target_id = ANY($3::uuid[]))
			   OR (d.dep_type = 'DECISION' AND d.target_id = ANY($4::uuid[]))
			   OR (d.dep_type = 'CLAIM' AND d.target_id = ANY($5::uuid[])))`,
			inv.modelID, affected, changedEdges, decisions, claims)
		if err != nil {
			return nil, fmt.Errorf("find dependent artifacts: %w", err)
		}
		for rows.Next() {
			var id, why string
			if err := rows.Scan(&id, &why); err != nil {
				rows.Close()
				return nil, err
			}
			if _, seen := seeds[id]; !seen {
				seeds[id] = why + " (" + reason + ")"
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return markArtifactsStale(ctx, inv.tx, inv.modelID, inv.graphVersion, seeds, func(id, why string) error {
		return inv.emit(ctx, EventArtifactInvalidated, map[string]any{"artifact_id": id, "reason": why})
	})
}

// MarkArtifactsStale propagates staleness from seed artifacts (id -> reason) to every
// CURRENT artifact downstream through ARTIFACT dependencies (visited-set recursive CTE),
// marks them all STALE and calls emit once per artifact. It returns the stale ids.
func MarkArtifactsStale(ctx context.Context, tx pgx.Tx, modelID string, graphVersion int, seeds map[string]string,
	emit func(id, why string) error) ([]string, error) {
	return markArtifactsStale(ctx, tx, modelID, graphVersion, seeds, emit)
}

func markArtifactsStale(ctx context.Context, tx pgx.Tx, modelID string, graphVersion int, seeds map[string]string,
	emit func(id, why string) error) ([]string, error) {
	if len(seeds) == 0 {
		return []string{}, nil
	}
	seedIDs := make([]string, 0, len(seeds))
	for id := range seeds {
		seedIDs = append(seedIDs, id)
	}
	rows, err := tx.Query(ctx, `
		WITH RECURSIVE downstream (id, upstream) AS (
			SELECT unnest($2::uuid[]), NULL::uuid
			UNION
			SELECT a.id, s.id FROM downstream s
			JOIN artifact_dependencies d ON d.dep_type = 'ARTIFACT' AND d.target_id = s.id
			JOIN artifacts a ON a.current_version_id = d.artifact_version_id
			WHERE a.model_id = $1 AND a.status = 'CURRENT'
		)
		SELECT DISTINCT ON (id) id::text, COALESCE(upstream::text, '') FROM downstream ORDER BY id, upstream NULLS FIRST`,
		modelID, seedIDs)
	if err != nil {
		return nil, fmt.Errorf("propagate artifact staleness: %w", err)
	}
	reasons := map[string]string{}
	for rows.Next() {
		var id, upstream string
		if err := rows.Scan(&id, &upstream); err != nil {
			rows.Close()
			return nil, err
		}
		if why, ok := seeds[id]; ok {
			reasons[id] = why
		} else {
			reasons[id] = "upstream artifact " + upstream + " is stale"
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var stale []string
	for id, why := range reasons {
		tag, err := tx.Exec(ctx, `UPDATE artifacts SET status = 'STALE', stale_reason = $3, stale_since = NOW(), stale_graph_version = $4
			WHERE id = $1 AND model_id = $2 AND status = 'CURRENT'`, id, modelID, why, graphVersion)
		if err != nil {
			return nil, fmt.Errorf("mark artifact stale: %w", err)
		}
		if tag.RowsAffected() == 1 {
			stale = append(stale, id)
			if err := emit(id, why); err != nil {
				return nil, err
			}
		}
	}
	sort.Strings(stale)
	if stale == nil {
		stale = []string{}
	}
	return stale, nil
}

// ---------------------------------------------------------------------------
// Dry-run impact analysis (read-only)
// ---------------------------------------------------------------------------

// ImpactedNode is a node in an impact report with its distance from the change.
type ImpactedNode struct {
	NodeID string `json:"node_id"`
	Key    string `json:"key"`
	Type   string `json:"type"`
	Depth  int    `json:"depth"`
}

// ImpactReport answers "what would go stale if these nodes changed?".
type ImpactReport struct {
	Version    int            `json:"version"`
	Changed    []string       `json:"changed_node_ids"`
	Nodes      []ImpactedNode `json:"impacted_nodes"`
	Artifacts  []string       `json:"artifact_ids"`
	Decisions  []string       `json:"decision_ids"`
	Claims     []string       `json:"claim_ids"`
	MaxDepth   int            `json:"max_depth_reached"`
	Truncated  bool           `json:"truncated"`
	DurationMS float64        `json:"duration_ms"`
}

// ImpactOf runs the dependency engine without changing anything.
func (s *Store) ImpactOf(ctx context.Context, tenantID, modelID string, number int, nodeIDs []string, lim ImpactLimits) (ImpactReport, error) {
	if len(nodeIDs) == 0 {
		return ImpactReport{}, fmt.Errorf("%w: at least one node id is required", ErrInvalidQuery)
	}
	for _, id := range nodeIDs {
		if !ids.Valid(id) {
			return ImpactReport{}, ErrNodeNotFound
		}
	}
	if lim.MaxDepth > 100 || lim.MaxNodes > 100000 {
		return ImpactReport{}, fmt.Errorf("%w: max_depth <= 100 and max_nodes <= 100000", ErrInvalidQuery)
	}
	rep := ImpactReport{Changed: nodeIDs}
	start := time.Now()
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		v, err := resolveVersion(ctx, tx, tenantID, modelID, number)
		if err != nil {
			return err
		}
		rep.Version = v.Number
		var live int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM architecture_nodes WHERE model_id = $1 AND `+liveAt+` AND node_id = ANY($3::uuid[])`,
			modelID, v.Number, nodeIDs).Scan(&live); err != nil {
			return err
		}
		if live != len(nodeIDs) {
			return ErrNodeNotFound
		}
		set, err := impactTraversal(ctx, tx, modelID, v.Number, nodeIDs, lim)
		if err != nil {
			return err
		}
		rep.MaxDepth, rep.Truncated = set.MaxDepth, set.Truncated
		affected := set.IDs()
		rows, err := tx.Query(ctx, `SELECT node_id::text, node_key, node_type FROM architecture_nodes
			WHERE model_id = $1 AND `+liveAt+` AND node_id = ANY($3::uuid[])`, modelID, v.Number, affected)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n ImpactedNode
			if err := rows.Scan(&n.NodeID, &n.Key, &n.Type); err != nil {
				rows.Close()
				return err
			}
			n.Depth = set.Depth[n.NodeID]
			rep.Nodes = append(rep.Nodes, n)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		sort.Slice(rep.Nodes, func(i, j int) bool {
			if rep.Nodes[i].Depth != rep.Nodes[j].Depth {
				return rep.Nodes[i].Depth < rep.Nodes[j].Depth
			}
			return rep.Nodes[i].Key < rep.Nodes[j].Key
		})
		q := func(sql string, args ...any) ([]string, error) {
			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return nil, err
			}
			return collectIDs(rows)
		}
		if rep.Claims, err = q(`SELECT id::text FROM claims WHERE model_id = $1 AND lifecycle = 'ACTIVE'
			AND ($3 OR subject_node_ids && $2::uuid[]) AND cardinality(subject_node_ids) > 0`, modelID, affected, set.Truncated); err != nil {
			return err
		}
		if rep.Decisions, err = q(`SELECT id::text FROM decisions WHERE model_id = $1 AND status IN ('PROPOSED', 'ACCEPTED')
			AND ($3 OR affected_nodes && $2::uuid[]) AND cardinality(affected_nodes) > 0`, modelID, affected, set.Truncated); err != nil {
			return err
		}
		rep.Artifacts, err = q(`
			WITH RECURSIVE downstream (id) AS (
				SELECT a.id FROM artifacts a JOIN artifact_dependencies d ON d.artifact_version_id = a.current_version_id
				WHERE a.model_id = $1 AND a.status = 'CURRENT' AND ($5 OR
				      (d.dep_type = 'NODE' AND d.target_id = ANY($2::uuid[]))
				   OR (d.dep_type = 'DECISION' AND d.target_id = ANY($3::uuid[]))
				   OR (d.dep_type = 'CLAIM' AND d.target_id = ANY($4::uuid[])))
				UNION
				SELECT a.id FROM downstream s
				JOIN artifact_dependencies d ON d.dep_type = 'ARTIFACT' AND d.target_id = s.id
				JOIN artifacts a ON a.current_version_id = d.artifact_version_id
				WHERE a.model_id = $1 AND a.status = 'CURRENT')
			SELECT id::text FROM downstream`, modelID, affected, rep.Decisions, rep.Claims, set.Truncated)
		return err
	})
	rep.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	if rep.Nodes == nil {
		rep.Nodes = []ImpactedNode{}
	}
	return rep, err
}

// NodeHash / EdgeHash are the canonical content hashes recorded as dependency hashes.
func NodeHash(n Node) string { return Hash([]Node{n}, nil) }
func EdgeHash(e Edge) string { return Hash(nil, []Edge{e}) }

// CurrentVersionTx returns the model's current version (tenant scope set by the caller).
func CurrentVersionTx(ctx context.Context, tx pgx.Tx, tenantID, modelID string) (Version, error) {
	return resolveVersion(ctx, tx, tenantID, modelID, -1)
}

// NodesAtTx returns the requested nodes live at a version (missing ids are omitted).
func NodesAtTx(ctx context.Context, tx pgx.Tx, modelID string, version int, nodeIDs []string) ([]Node, error) {
	rows, err := tx.Query(ctx, `SELECT `+nodeCols+` FROM architecture_nodes
		WHERE model_id = $1 AND `+liveAt+` AND node_id = ANY($3::uuid[]) ORDER BY node_id`, modelID, version, nodeIDs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Node, error) { return scanNode(r) })
}

// EdgesFromTx returns live edges whose source is one of nodeIDs (their dependencies).
func EdgesFromTx(ctx context.Context, tx pgx.Tx, modelID string, version int, nodeIDs []string) ([]Edge, error) {
	rows, err := tx.Query(ctx, `SELECT `+edgeCols+` FROM architecture_edges
		WHERE model_id = $1 AND `+liveAt+` AND source_node_id = ANY($3::uuid[]) ORDER BY edge_id`, modelID, version, nodeIDs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Edge, error) { return scanEdge(r) })
}

// EdgesByIDTx returns the requested edges live at a version.
func EdgesByIDTx(ctx context.Context, tx pgx.Tx, modelID string, version int, edgeIDs []string) ([]Edge, error) {
	rows, err := tx.Query(ctx, `SELECT `+edgeCols+` FROM architecture_edges
		WHERE model_id = $1 AND `+liveAt+` AND edge_id = ANY($3::uuid[]) ORDER BY edge_id`, modelID, version, edgeIDs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Edge, error) { return scanEdge(r) })
}
