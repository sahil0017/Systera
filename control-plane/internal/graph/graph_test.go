package graph

import (
	"encoding/json"
	"testing"
)

func TestHashIsCanonical(t *testing.T) {
	a := Node{ID: "1", Key: "svc.a", Type: "service", Name: "A", Properties: map[string]any{"x": json.Number("1"), "y": "z"}, LifecycleState: "ACTIVE", EvidenceState: "PROPOSED"}
	b := Node{ID: "2", Key: "db.b", Type: "database", Name: "B", LifecycleState: "ACTIVE", EvidenceState: "OBSERVED"}
	e := Edge{ID: "e", SourceNodeID: "1", TargetNodeID: "2", RelationType: "writes", Direction: "DIRECTED"}

	h := Hash([]Node{a, b}, []Edge{e})
	if Hash([]Node{b, a}, []Edge{e}) != h {
		t.Fatal("hash must not depend on input order")
	}
	// Storage-only fields are excluded.
	a2, e2 := a, e
	a2.Revision, a2.ValidFrom, e2.Revision, e2.ValidFrom = 7, 9, 3, 4
	if Hash([]Node{a2, b}, []Edge{e2}) != h {
		t.Fatal("revision/validity must not affect the content hash")
	}
	// Nil and empty maps are equivalent; map key order never matters.
	b2 := b
	b2.Properties = map[string]any{}
	if Hash([]Node{a, b2}, []Edge{e}) != h {
		t.Fatal("nil and empty properties must hash equally")
	}
	// Every semantic field matters.
	mutations := []func(*Node, *Edge){
		func(n *Node, _ *Edge) { n.Name = "A2" },
		func(n *Node, _ *Edge) { n.Key = "svc.a2" },
		func(n *Node, _ *Edge) { n.Properties = map[string]any{"x": json.Number("2"), "y": "z"} },
		func(n *Node, _ *Edge) { n.EvidenceState = "OBSERVED" },
		func(_ *Node, ed *Edge) { ed.RelationType = "reads" },
		func(_ *Node, ed *Edge) { ed.SyncMode = "ASYNC" },
		func(_ *Node, ed *Edge) { ed.Constraints = map[string]any{"timeout_ms": json.Number("50")} },
	}
	for i, mut := range mutations {
		n, ed := a, e
		mut(&n, &ed)
		if Hash([]Node{n, b}, []Edge{ed}) == h {
			t.Errorf("mutation %d did not change the hash", i)
		}
	}
	if len(Hash(nil, nil)) != 64 || Hash(nil, nil) == h {
		t.Fatal("empty graph hash")
	}
}
