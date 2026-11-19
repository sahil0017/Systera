package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/postgres"
)

// NodeChange describes a node present in both versions with different content.
type NodeChange struct {
	ID            string   `json:"id"`
	Key           string   `json:"key"`
	ChangedFields []string `json:"changed_fields"`
	Before        Node     `json:"before"`
	After         Node     `json:"after"`
}

// EdgeChange describes an edge present in both versions with different content.
type EdgeChange struct {
	ID            string   `json:"id"`
	ChangedFields []string `json:"changed_fields"`
	Before        Edge     `json:"before"`
	After         Edge     `json:"after"`
}

// Diff is the semantic difference between two versions.
type Diff struct {
	ModelID     string `json:"model_id"`
	FromVersion int    `json:"from_version"`
	ToVersion   int    `json:"to_version"`
	FromHash    string `json:"from_hash"`
	ToHash      string `json:"to_hash"`
	Nodes       struct {
		Added    []Node       `json:"added"`
		Removed  []Node       `json:"removed"`
		Modified []NodeChange `json:"modified"`
	} `json:"nodes"`
	Edges struct {
		Added    []Edge       `json:"added"`
		Removed  []Edge       `json:"removed"`
		Modified []EdgeChange `json:"modified"`
	} `json:"edges"`
}

// Empty reports whether the two versions have identical content.
func (d Diff) Empty() bool {
	return len(d.Nodes.Added)+len(d.Nodes.Removed)+len(d.Nodes.Modified)+
		len(d.Edges.Added)+len(d.Edges.Removed)+len(d.Edges.Modified) == 0
}

func nodeFields(n Node) map[string]any {
	return map[string]any{
		"key": n.Key, "type": n.Type, "name": n.Name, "description": n.Description, "properties": nonNil(n.Properties),
		"owner_domain": n.OwnerDomain, "lifecycle_state": n.LifecycleState, "evidence_state": n.EvidenceState,
	}
}

func edgeFields(e Edge) map[string]any {
	return map[string]any{
		"source_node_id": e.SourceNodeID, "target_node_id": e.TargetNodeID, "relation_type": e.RelationType,
		"direction": e.Direction, "protocol": e.Protocol, "sync_mode": e.SyncMode,
		"constraints": nonNil(e.Constraints), "properties": nonNil(e.Properties),
	}
}

func changedFields(a, b map[string]any) []string {
	var out []string
	for k, av := range a {
		x, _ := json.Marshal(av)
		y, _ := json.Marshal(b[k])
		if !bytes.Equal(x, y) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// DiffGraphs computes a deterministic semantic diff (ordered by node key / edge id).
func DiffGraphs(from, to Graph) Diff {
	var d Diff
	d.ModelID, d.FromVersion, d.ToVersion = from.Version.ModelID, from.Version.Number, to.Version.Number
	d.FromHash, d.ToHash = from.Version.GraphHash, to.Version.GraphHash
	d.Nodes.Added, d.Nodes.Removed, d.Nodes.Modified = []Node{}, []Node{}, []NodeChange{}
	d.Edges.Added, d.Edges.Removed, d.Edges.Modified = []Edge{}, []Edge{}, []EdgeChange{}

	fromNodes := map[string]Node{}
	for _, n := range from.Nodes {
		fromNodes[n.ID] = n
	}
	toNodes := map[string]Node{}
	for _, n := range to.Nodes {
		toNodes[n.ID] = n
		old, ok := fromNodes[n.ID]
		if !ok {
			d.Nodes.Added = append(d.Nodes.Added, n)
		} else if fs := changedFields(nodeFields(old), nodeFields(n)); len(fs) > 0 {
			d.Nodes.Modified = append(d.Nodes.Modified, NodeChange{ID: n.ID, Key: n.Key, ChangedFields: fs, Before: old, After: n})
		}
	}
	for _, n := range from.Nodes {
		if _, ok := toNodes[n.ID]; !ok {
			d.Nodes.Removed = append(d.Nodes.Removed, n)
		}
	}
	fromEdges := map[string]Edge{}
	for _, e := range from.Edges {
		fromEdges[e.ID] = e
	}
	toEdges := map[string]Edge{}
	for _, e := range to.Edges {
		toEdges[e.ID] = e
		old, ok := fromEdges[e.ID]
		if !ok {
			d.Edges.Added = append(d.Edges.Added, e)
		} else if fs := changedFields(edgeFields(old), edgeFields(e)); len(fs) > 0 {
			d.Edges.Modified = append(d.Edges.Modified, EdgeChange{ID: e.ID, ChangedFields: fs, Before: old, After: e})
		}
	}
	for _, e := range from.Edges {
		if _, ok := toEdges[e.ID]; !ok {
			d.Edges.Removed = append(d.Edges.Removed, e)
		}
	}
	byKey := func(ns []Node) { sort.Slice(ns, func(i, j int) bool { return ns[i].Key < ns[j].Key }) }
	byKey(d.Nodes.Added)
	byKey(d.Nodes.Removed)
	sort.Slice(d.Nodes.Modified, func(i, j int) bool { return d.Nodes.Modified[i].Key < d.Nodes.Modified[j].Key })
	byID := func(es []Edge) { sort.Slice(es, func(i, j int) bool { return es[i].ID < es[j].ID }) }
	byID(d.Edges.Added)
	byID(d.Edges.Removed)
	sort.Slice(d.Edges.Modified, func(i, j int) bool { return d.Edges.Modified[i].ID < d.Edges.Modified[j].ID })
	return d
}

// Diff compares two versions of a model (numbers < 0 mean current) in one snapshot.
func (s *Store) Diff(ctx context.Context, tenantID, modelID string, from, to int) (Diff, error) {
	var d Diff
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		a, err := graphAtTx(ctx, tx, tenantID, modelID, from)
		if err != nil {
			return err
		}
		b, err := graphAtTx(ctx, tx, tenantID, modelID, to)
		if err != nil {
			return err
		}
		d = DiffGraphs(a, b)
		return nil
	})
	return d, err
}

// NeighbourhoodQuery bounds a depth-limited neighbourhood walk.
type NeighbourhoodQuery struct {
	Direction     Direction
	RelationTypes []string
	Depth         int // 1-5, default 1
	MaxNodes      int // default 200, max 2000
}

// NeighbourNode is a node with its hop distance from the centre.
type NeighbourNode struct {
	Node
	Depth int `json:"depth"`
}

// Neighbourhood is a subgraph around a centre node.
type Neighbourhood struct {
	CenterNodeID string          `json:"center_node_id"`
	Version      int             `json:"version"`
	Depth        int             `json:"depth"`
	Nodes        []NeighbourNode `json:"nodes"`
	Edges        []Edge          `json:"edges"`
	Truncated    bool            `json:"truncated"`
}

// Neighbourhood returns the nodes within Depth hops of center (breadth-first, one query
// per hop) and every live edge among them that matches the relation filter.
func (s *Store) Neighbourhood(ctx context.Context, tenantID, modelID string, number int, center string, q NeighbourhoodQuery) (Neighbourhood, error) {
	if q.Depth == 0 {
		q.Depth = 1
	}
	if q.Depth < 1 || q.Depth > 5 {
		return Neighbourhood{}, fmt.Errorf("%w: depth must be 1-5", ErrInvalidQuery)
	}
	if q.MaxNodes == 0 {
		q.MaxNodes = 200
	}
	if q.MaxNodes < 1 || q.MaxNodes > 2000 {
		return Neighbourhood{}, fmt.Errorf("%w: max_nodes must be 1-2000", ErrInvalidQuery)
	}
	switch q.Direction {
	case "":
		q.Direction = Both
	case Outbound, Inbound, Both:
	default:
		return Neighbourhood{}, fmt.Errorf("%w: direction must be out, in or both", ErrInvalidQuery)
	}
	for _, r := range q.RelationTypes {
		if !validRelationType(r) {
			return Neighbourhood{}, fmt.Errorf("%w: unknown relation type %q", ErrInvalidQuery, r)
		}
	}
	if q.RelationTypes == nil {
		q.RelationTypes = []string{}
	}
	if !ids.Valid(center) {
		return Neighbourhood{}, ErrNodeNotFound
	}

	res := Neighbourhood{CenterNodeID: center, Depth: q.Depth, Nodes: []NeighbourNode{}, Edges: []Edge{}}
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		v, err := resolveVersion(ctx, tx, tenantID, modelID, number)
		if err != nil {
			return err
		}
		res.Version = v.Number
		var live bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM architecture_nodes WHERE model_id = $1 AND node_id = $3 AND `+liveAt+`)`,
			modelID, v.Number, center).Scan(&live); err != nil {
			return err
		}
		if !live {
			return ErrNodeNotFound
		}

		depthOf := map[string]int{center: 0}
		frontier := []string{center}
	hops:
		for d := 1; d <= q.Depth && len(frontier) > 0; d++ {
			rows, err := tx.Query(ctx, `
				SELECT source_node_id::text, target_node_id::text, direction FROM architecture_edges
				WHERE model_id = $1 AND `+liveAt+`
				  AND (source_node_id = ANY($3::uuid[]) OR target_node_id = ANY($3::uuid[]))
				  AND (cardinality($4::text[]) = 0 OR relation_type = ANY($4::text[]))
				ORDER BY source_node_id, target_node_id`, modelID, v.Number, frontier, q.RelationTypes)
			if err != nil {
				return err
			}
			inFrontier := map[string]bool{}
			for _, f := range frontier {
				inFrontier[f] = true
			}
			var next []string
			for rows.Next() {
				var src, dst, dir string
				if err := rows.Scan(&src, &dst, &dir); err != nil {
					rows.Close()
					return err
				}
				bidi := dir == "BIDIRECTIONAL"
				var cand []string
				if inFrontier[src] && (q.Direction != Inbound || bidi) {
					cand = append(cand, dst)
				}
				if inFrontier[dst] && (q.Direction != Outbound || bidi) {
					cand = append(cand, src)
				}
				for _, c := range cand {
					if _, seen := depthOf[c]; seen {
						continue
					}
					if len(depthOf) > q.MaxNodes { // centre is not counted
						res.Truncated = true
						rows.Close()
						break hops
					}
					depthOf[c] = d
					next = append(next, c)
				}
			}
			if err := rows.Err(); err != nil {
				return err
			}
			frontier = next
		}

		idsList := make([]string, 0, len(depthOf))
		for id := range depthOf {
			idsList = append(idsList, id)
		}
		rows, err := tx.Query(ctx, `SELECT `+nodeCols+` FROM architecture_nodes
			WHERE model_id = $1 AND `+liveAt+` AND node_id = ANY($3::uuid[]) ORDER BY node_key`, modelID, v.Number, idsList)
		if err != nil {
			return err
		}
		nodes, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Node, error) { return scanNode(r) })
		if err != nil {
			return err
		}
		for _, n := range nodes {
			res.Nodes = append(res.Nodes, NeighbourNode{Node: n, Depth: depthOf[n.ID]})
		}
		sort.SliceStable(res.Nodes, func(i, j int) bool { return res.Nodes[i].Depth < res.Nodes[j].Depth })
		rows, err = tx.Query(ctx, `SELECT `+edgeCols+` FROM architecture_edges
			WHERE model_id = $1 AND `+liveAt+`
			  AND source_node_id = ANY($3::uuid[]) AND target_node_id = ANY($3::uuid[])
			  AND (cardinality($4::text[]) = 0 OR relation_type = ANY($4::text[]))
			ORDER BY source_node_id, relation_type, target_node_id, edge_id`, modelID, v.Number, idsList, q.RelationTypes)
		if err != nil {
			return err
		}
		res.Edges, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Edge, error) { return scanEdge(r) })
		if res.Edges == nil {
			res.Edges = []Edge{}
		}
		return err
	})
	return res, err
}
