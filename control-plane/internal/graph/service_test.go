package graph

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/systera/control-plane/internal/platform/ids"
)

func TestRelationMatrix(t *testing.T) {
	ok := [][3]string{
		{"calls", "service", "api"}, {"writes", "service", "database"}, {"publishes", "component", "topic"},
		{"deploys_to", "service", "region"}, {"replicates", "database", "database"}, {"depends_on", "slo", "service"},
		{"authenticates", "api", "actor"}, {"owns", "domain", "service"}, {"fails_over_to", "region", "region"},
	}
	for _, c := range ok {
		if err := CheckRelation(c[0], c[1], c[2]); err != nil {
			t.Errorf("%v should be allowed: %v", c, err)
		}
	}
	bad := [][3]string{
		{"calls", "database", "service"}, {"writes", "service", "api"}, {"replicates", "database", "cache"},
		{"publishes", "service", "database"}, {"deploys_to", "region", "service"}, {"teleports", "service", "service"},
	}
	for _, c := range bad {
		if err := CheckRelation(c[0], c[1], c[2]); err == nil {
			t.Errorf("%v should be rejected", c)
		}
	}
	for _, r := range RelationTypes {
		if _, ok := RelationRules[r]; !ok {
			t.Errorf("relation %q has no rule", r)
		}
	}
}

func op(kind, target string, payload any) Operation {
	raw, _ := json.Marshal(payload)
	return Operation{Op: kind, TargetID: target, Payload: raw, Rationale: "because"}
}

func codes(fs []Finding) string {
	var c []string
	for _, f := range fs {
		if f.Severity == "ERROR" {
			c = append(c, f.Code)
		}
	}
	return strings.Join(c, ",")
}

func TestPlanValidation(t *testing.T) {
	svc, db := ids.New(), ids.New()
	g := Graph{Nodes: []Node{
		{ID: svc, Key: "svc.a", Type: "service", Name: "A", LifecycleState: "PROPOSED", EvidenceState: "PROPOSED", Revision: 1},
		{ID: db, Key: "db.a", Type: "database", Name: "DB", LifecycleState: "PROPOSED", EvidenceState: "PROPOSED", Revision: 1},
	}}
	e := ids.New()
	g.Edges = []Edge{{ID: e, SourceNodeID: svc, TargetNodeID: db, RelationType: "writes", Direction: "DIRECTED"}}
	ever := func() (map[string]bool, map[string]bool) {
		return map[string]bool{svc: true, db: true}, map[string]bool{e: true}
	}

	newNode := ids.New()
	cases := []struct {
		name string
		ops  []Operation
		want string
	}{
		{"valid add + edge referencing it", []Operation{
			op(OpAddNode, newNode, AddNodePayload{Key: "cache.a", Type: "cache", Name: "Cache"}),
			op(OpAddEdge, ids.New(), AddEdgePayload{SourceNodeID: svc, TargetNodeID: newNode, RelationType: "reads"}),
		}, ""},
		{"incompatible edge", []Operation{op(OpAddEdge, ids.New(), AddEdgePayload{SourceNodeID: db, TargetNodeID: svc, RelationType: "calls"})}, "incompatible_edge"},
		{"unknown endpoint", []Operation{op(OpAddEdge, ids.New(), AddEdgePayload{SourceNodeID: svc, TargetNodeID: ids.New(), RelationType: "calls"})}, "unknown_endpoint"},
		{"duplicate key", []Operation{op(OpAddNode, ids.New(), AddNodePayload{Key: "svc.a", Type: "service", Name: "dup"})}, "duplicate_key"},
		{"bad type and key", []Operation{op(OpAddNode, ids.New(), AddNodePayload{Key: "Bad Key", Type: "spaceship", Name: "x"})}, "invalid_key,invalid_node_type"},
		{"id reuse", []Operation{op(OpAddNode, svc, AddNodePayload{Key: "svc.b", Type: "service", Name: "B"})}, "id_reused"},
		{"type change via update", []Operation{op(OpUpdateNode, svc, map[string]any{"type": "database"})}, "invalid_payload"},
		{"remove node with edges", []Operation{op(OpRemoveNode, db, RemoveNodePayload{})}, "node_has_edges"},
		{"cascade remove", []Operation{op(OpRemoveNode, db, RemoveNodePayload{CascadeEdges: true})}, ""},
		{"duplicate target", []Operation{op(OpUpdateNode, svc, map[string]any{"name": "x"}), op(OpUpdateNode, svc, map[string]any{"name": "y"})}, "duplicate_target"},
		{"duplicate edge", []Operation{op(OpAddEdge, ids.New(), AddEdgePayload{SourceNodeID: svc, TargetNodeID: db, RelationType: "writes"})}, "duplicate_edge"},
		{"missing rationale", []Operation{{Op: OpRemoveEdge, TargetID: e, Payload: json.RawMessage(`{}`)}}, "missing_rationale"},
		{"unknown op", []Operation{op("rename_node", svc, map[string]any{})}, "unknown_op"},
		{"edge dangles after later removal", []Operation{
			op(OpAddNode, newNode, AddNodePayload{Key: "cache.b", Type: "cache", Name: "C"}),
			op(OpRemoveEdge, e, map[string]any{}),
			op(OpAddEdge, ids.New(), AddEdgePayload{SourceNodeID: svc, TargetNodeID: db, RelationType: "reads"}),
			op(OpRemoveNode, db, RemoveNodePayload{}),
		}, "node_has_edges"},
	}
	for _, c := range cases {
		n, ed := ever()
		p := newPlan(g)
		if got := codes(p.validate(c.ops, n, ed)); got != c.want {
			t.Errorf("%s: findings %q, want %q", c.name, got, c.want)
		}
	}

	// Cascade removal removes the attached edge too.
	n, ed := ever()
	p := newPlan(g)
	p.validate([]Operation{op(OpRemoveNode, db, RemoveNodePayload{CascadeEdges: true})}, n, ed)
	if len(p.changes.EdgesRemoved) != 1 || len(p.edges) != 0 || len(p.nodes) != 1 {
		t.Fatalf("cascade: %+v", p.changes)
	}
}

func TestDiffGraphs(t *testing.T) {
	a, b, c := ids.New(), ids.New(), ids.New()
	n := func(id, key, name string) Node {
		return Node{ID: id, Key: key, Type: "service", Name: name, LifecycleState: "PROPOSED", EvidenceState: "PROPOSED"}
	}
	from := Graph{Version: Version{Number: 1}, Nodes: []Node{n(a, "a", "A"), n(b, "b", "B")}}
	to := Graph{Version: Version{Number: 2}, Nodes: []Node{n(a, "a", "A2"), n(c, "c", "C")}}
	d := DiffGraphs(from, to)
	if len(d.Nodes.Added) != 1 || d.Nodes.Added[0].ID != c || len(d.Nodes.Removed) != 1 || d.Nodes.Removed[0].ID != b ||
		len(d.Nodes.Modified) != 1 || strings.Join(d.Nodes.Modified[0].ChangedFields, ",") != "name" {
		t.Fatalf("diff: %+v", d)
	}
	if !DiffGraphs(from, from).Empty() {
		t.Fatal("self-diff must be empty")
	}
}

// The Go types must emit every field the published contracts require.
func TestContractsMatchTypes(t *testing.T) {
	check := func(file string, v any) {
		t.Helper()
		raw, err := os.ReadFile("../../../contracts/graph/" + file)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(v)
		var got map[string]any
		_ = json.Unmarshal(b, &got)
		for _, k := range schema.Required {
			if _, ok := got[k]; !ok {
				t.Errorf("%s: required field %q missing from Go encoding", file, k)
			}
		}
	}
	zero := 0
	check("graph-patch.v1.json", Patch{BaseGraphVersion: &zero, IdempotencyKey: "k", Operations: []Operation{{}}})
	check("graph-patch-result.v1.json", Result{Changes: newChangeSummary(), Conflicts: []Conflict{}, ValidationFindings: []Finding{},
		InvalidatedArtifacts: []string{}, ImpactedComponents: []ImpactedComponent{}})
}

func TestInvariant34_EvidenceRequirements(t *testing.T) {
	base := Graph{}
	p := newPlan(base)

	obsNoRef := op(OpAddNode, ids.New(), AddNodePayload{Key: "svc.obs", Type: "service", Name: "Obs", EvidenceState: "OBSERVED"})
	if got := codes(p.validate([]Operation{obsNoRef}, map[string]bool{}, map[string]bool{})); got != "missing_evidence" {
		t.Fatalf("OBSERVED without evidence_refs: got %q, want missing_evidence", got)
	}

	p = newPlan(base)
	infNoRef := op(OpAddNode, ids.New(), AddNodePayload{Key: "svc.inf", Type: "service", Name: "Inf", EvidenceState: "INFERRED"})
	if got := codes(p.validate([]Operation{infNoRef}, map[string]bool{}, map[string]bool{})); got != "missing_evidence_or_reasoning" {
		t.Fatalf("INFERRED without evidence_refs: got %q, want missing_evidence_or_reasoning", got)
	}

	p = newPlan(base)
	obsOK := op(OpAddNode, ids.New(), AddNodePayload{Key: "svc.obs2", Type: "service", Name: "Obs2", EvidenceState: "OBSERVED"})
	obsOK.EvidenceRefs = []string{"obs-1"}
	if got := codes(p.validate([]Operation{obsOK}, map[string]bool{}, map[string]bool{})); got != "" {
		t.Fatalf("OBSERVED with evidence_refs: unexpected findings %q", got)
	}
}

func TestInvariant35_NoOrphanedRequiredRelations(t *testing.T) {
	svc := ids.New()
	api := ids.New()
	base := Graph{Nodes: []Node{
		{ID: svc, Key: "svc.a", Type: "service", Name: "A", LifecycleState: "PROPOSED", EvidenceState: "PROPOSED", Revision: 1},
	}}

	p := newPlan(base)
	orphan := op(OpAddNode, api, AddNodePayload{Key: "api.orphan", Type: "api", Name: "Orphan API"})
	if got := codes(p.validate([]Operation{orphan}, map[string]bool{svc: true}, map[string]bool{})); got != "orphaned_required_relation" {
		t.Fatalf("orphaned api node: got %q, want orphaned_required_relation", got)
	}

	p = newPlan(base)
	owned := op(OpAddEdge, ids.New(), AddEdgePayload{SourceNodeID: svc, TargetNodeID: api, RelationType: "owns"})
	if got := codes(p.validate([]Operation{orphan, owned}, map[string]bool{svc: true}, map[string]bool{})); got != "" {
		t.Fatalf("connected api node: unexpected findings %q", got)
	}
}
