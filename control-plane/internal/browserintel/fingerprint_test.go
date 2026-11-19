package browserintel

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/ids"
)

func TestBuildFingerprintAllEightLayersAndValidGraphPatch(t *testing.T) {
	w := WebsiteProfile{
		ID:        ids.New(),
		TenantID:  ids.New(),
		SessionID: ids.New(),
		Domain:    "shop.example.com",
		Capabilities: []CapabilityFinding{
			{Name: "Product Search", EvidenceClass: "OBSERVED", Confidence: "HIGH", EvidenceRefs: []string{"obs:1"}, Rationale: "Search input visible"},
			{Name: "Shopping Cart", EvidenceClass: "OBSERVED", Confidence: "HIGH", EvidenceRefs: []string{"obs:1"}, Rationale: "Cart link visible"},
		},
		Journeys: []string{"Browse -> Cart -> Checkout"},
	}
	obs := []Observation{{ID: ids.New(), URL: "https://shop.example.com/", Title: "Shop"}}

	fp := BuildFingerprint(w, obs)
	if !strings.Contains(strings.ToLower(fp.Disclaimer), "not a factual map of hidden infrastructure") {
		t.Fatalf("missing explicit non-factual disclaimer: %q", fp.Disclaimer)
	}
	for _, layer := range AllFingerprintLayers {
		sub, ok := fp.Layers[layer]
		if !ok || len(sub.Elements) == 0 {
			t.Errorf("expected non-empty layer %q", layer)
		}
	}
	if len(fp.QualityAttributes) < 3 {
		t.Fatalf("expected latency/availability/scale quality attributes, got %+v", fp.QualityAttributes)
	}

	// Verify BuildFingerprintPatch produces edges compatible with graph.CheckRelation (5.2).
	patch := BuildFingerprintPatch(0, fp, "fp-test-1")
	nodeTypes := make(map[string]string)
	for _, op := range patch.Operations {
		if op.Op == graph.OpAddNode {
			var pl graph.AddNodePayload
			if err := json.Unmarshal(op.Payload, &pl); err != nil {
				t.Fatalf("unmarshal add_node: %v", err)
			}
			nodeTypes[op.TargetID] = pl.Type
		}
	}
	for _, op := range patch.Operations {
		if op.Op == graph.OpAddEdge {
			var pl graph.AddEdgePayload
			if err := json.Unmarshal(op.Payload, &pl); err != nil {
				t.Fatalf("unmarshal add_edge: %v", err)
			}
			if err := graph.CheckRelation(pl.RelationType, nodeTypes[pl.SourceNodeID], nodeTypes[pl.TargetNodeID]); err != nil {
				t.Errorf("fingerprint edge %s incompatible: %v", op.TargetID, err)
			}
		}
	}
}

func TestReconcileFingerprintKeepsUpdatesAndRemoves(t *testing.T) {
	w := WebsiteProfile{ID: ids.New(), SessionID: ids.New(), Domain: "shop.example.com",
		Capabilities: []CapabilityFinding{{Name: "Cart", EvidenceClass: "OBSERVED", Confidence: "HIGH", EvidenceRefs: []string{"evidence:a"}, Rationale: "r"}}}
	first := BuildFingerprint(w, nil)
	ops := ReconcileFingerprint(&first, graph.Graph{})
	// Materialize what the first patch would have created.
	var cur graph.Graph
	nodes := map[string]graph.Node{}
	for _, op := range ops {
		switch op.Op {
		case graph.OpAddNode:
			var pl graph.AddNodePayload
			_ = json.Unmarshal(op.Payload, &pl)
			n := graph.Node{ID: op.TargetID, Key: pl.Key, Type: pl.Type, Name: pl.Name, Description: pl.Description, EvidenceState: pl.EvidenceState, Properties: pl.Properties}
			nodes[n.ID] = n
			cur.Nodes = append(cur.Nodes, n)
		case graph.OpAddEdge:
			var pl graph.AddEdgePayload
			_ = json.Unmarshal(op.Payload, &pl)
			cur.Edges = append(cur.Edges, graph.Edge{ID: op.TargetID, SourceNodeID: pl.SourceNodeID, TargetNodeID: pl.TargetNodeID, RelationType: pl.RelationType, Properties: pl.Properties})
		}
	}
	// Someone else's node with a colliding key is never touched.
	cur.Nodes = append(cur.Nodes, graph.Node{ID: ids.New(), Key: "cap.cart", Type: "component", Properties: map[string]any{"fingerprint_layer": "capability", "website_id": ids.New()}})

	same := BuildFingerprint(w, nil)
	if ops := ReconcileFingerprint(&same, cur); len(ops) != 0 {
		t.Fatalf("unchanged rebuild produced %d ops", len(ops))
	}
	if same.Layers[LayerCapability].Elements[0].NodeID != first.Layers[LayerCapability].Elements[0].NodeID {
		t.Fatal("rebuild did not reuse node ids")
	}

	w.Capabilities = []CapabilityFinding{{Name: "Search", EvidenceClass: "OBSERVED", Confidence: "HIGH", EvidenceRefs: []string{"evidence:b"}, Rationale: "r"}}
	w.Journeys = []string{"Query -> Results"}
	changed := BuildFingerprint(w, nil)
	ops = ReconcileFingerprint(&changed, cur)
	count := map[string]int{}
	removedKeys := []string{}
	for _, op := range ops {
		count[op.Op]++
		if op.Op == graph.OpRemoveNode {
			removedKeys = append(removedKeys, nodes[op.TargetID].Key)
			if string(op.Payload) != `{"cascade_edges":true}` {
				t.Fatalf("remove_node without cascade: %s", op.Payload)
			}
		}
	}
	if count[graph.OpRemoveNode] != 1 || removedKeys[0] != "cap.cart" || count[graph.OpAddNode] != 2 || count[graph.OpAddEdge] != 2 {
		t.Fatalf("reconcile ops: %v removed=%v", count, removedKeys)
	}
	// The edges of retained nodes point at their existing ids.
	for _, layer := range AllFingerprintLayers {
		for _, ed := range changed.Layers[layer].Edges {
			ok := false
			for _, el := range changed.Layers {
				for _, e := range el.Elements {
					ok = ok || e.NodeID == ed.SourceNodeID
				}
			}
			if !ok {
				t.Fatalf("edge %s source %s is not a fingerprint node", ed.EdgeID, ed.SourceNodeID)
			}
		}
	}
}
