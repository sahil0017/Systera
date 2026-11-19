package graph

import (
	"slices"
	"testing"
)

func TestAllNodeTypesMapToEightXRayLayers(t *testing.T) {
	if len(AllXRayLayers) != 8 || len(XRayLayerDefinitions) != 8 {
		t.Fatalf("expected 8 X-Ray layers, got %d / %d", len(AllXRayLayers), len(XRayLayerDefinitions))
	}
	seen := map[XRayLayer]bool{}
	for _, nt := range NodeTypes {
		l := DefaultLayerForNodeType(nt)
		if !slices.Contains(AllXRayLayers, l) {
			t.Fatalf("node type %q mapped to invalid layer %q", nt, l)
		}
		seen[l] = true
	}
	for _, l := range AllXRayLayers {
		if !seen[l] {
			t.Fatalf("X-Ray layer %q has no default node types mapped to it", l)
		}
	}

	// Explicit properties["layer"] and properties["fingerprint_layer"] overrides.
	n1 := Node{Type: "component", Properties: map[string]any{"layer": "security/reliability"}}
	if got := ResolveXRayLayer(n1); got != LayerSecurityReliability {
		t.Fatalf("expected explicit layer override %q, got %q", LayerSecurityReliability, got)
	}
	n2 := Node{Type: "component", Properties: map[string]any{"fingerprint_layer": "event_flow"}}
	if got := ResolveXRayLayer(n2); got != LayerEvents {
		t.Fatalf("expected fingerprint_layer override %q, got %q", LayerEvents, got)
	}
}

func TestBuildXRayViewAndComponentInspection(t *testing.T) {
	g := Graph{
		Version: Version{ModelID: "m1", Number: 3, GraphHash: "abc"},
		Nodes: []Node{
			{ID: "n-actor", Key: "actor:shopper", Type: "actor", Name: "Shopper", EvidenceState: "OBSERVED"},
			{ID: "n-dom", Key: "domain:orders", Type: "domain", Name: "Orders Domain", EvidenceState: "PROPOSED"},
			{ID: "n-svc", Key: "svc:checkout", Type: "service", Name: "Checkout Service", Description: "Coordinates checkout and payment capture", OwnerDomain: "Orders", EvidenceState: "PROPOSED", Properties: map[string]any{"responsibilities": []any{"Idempotent order creation"}}},
			{ID: "n-api", Key: "api:checkout", Type: "api", Name: "POST /v1/checkout", EvidenceState: "OBSERVED"},
			{ID: "n-db", Key: "db:orders", Type: "database", Name: "Orders Postgres", EvidenceState: "PROPOSED"},
			{ID: "n-ev", Key: "ev:order-placed", Type: "topic", Name: "orders.placed", EvidenceState: "PROPOSED"},
			{ID: "n-dep", Key: "dep:k8s", Type: "deployment_unit", Name: "eks-checkout", EvidenceState: "PROPOSED"},
			{ID: "n-slo", Key: "slo:checkout", Type: "slo", Name: "p99 < 250ms", Description: "Checkout latency SLO", EvidenceState: "PROPOSED"},
			{ID: "n-threat", Key: "threat:replay", Type: "threat", Name: "Duplicate payment replay", Description: "Missing idempotency key", EvidenceState: "PROPOSED"},
		},
		Edges: []Edge{
			{ID: "e1", SourceNodeID: "n-actor", TargetNodeID: "n-api", RelationType: "calls", Protocol: "HTTPS", SyncMode: "SYNC"},
			{ID: "e2", SourceNodeID: "n-dom", TargetNodeID: "n-svc", RelationType: "owns"},
			{ID: "e3", SourceNodeID: "n-svc", TargetNodeID: "n-api", RelationType: "owns", Protocol: "HTTPS"},
			{ID: "e4", SourceNodeID: "n-svc", TargetNodeID: "n-db", RelationType: "writes", Protocol: "PostgreSQL"},
			{ID: "e5", SourceNodeID: "n-svc", TargetNodeID: "n-ev", RelationType: "publishes", SyncMode: "ASYNC"},
			{ID: "e6", SourceNodeID: "n-svc", TargetNodeID: "n-dep", RelationType: "deploys_to"},
			{ID: "e7", SourceNodeID: "n-svc", TargetNodeID: "n-slo", RelationType: "owns"},
			{ID: "e8", SourceNodeID: "n-threat", TargetNodeID: "n-svc", RelationType: "depends_on"},
		},
	}

	view := BuildXRayView(g, nil)
	if len(view.Layers) != 8 {
		t.Fatalf("expected 8 layers, got %d", len(view.Layers))
	}
	if view.TotalNodes != 9 {
		t.Fatalf("expected 9 total nodes, got %d", view.TotalNodes)
	}
	if len(view.CrossLayerEdges) != 8 {
		t.Fatalf("expected 8 cross-layer edges, got %d", len(view.CrossLayerEdges))
	}

	// Filter to 2 layers.
	filtered := BuildXRayView(g, []XRayLayer{LayerService, LayerData})
	if len(filtered.Layers) != 2 {
		t.Fatalf("expected 2 filtered layers, got %d", len(filtered.Layers))
	}
	if len(filtered.CrossLayerEdges) != 1 || filtered.CrossLayerEdges[0].ID != "e4" {
		t.Fatalf("expected 1 cross-layer edge (e4), got %+v", filtered.CrossLayerEdges)
	}

	// Component inspection for Checkout Service.
	insp := BuildComponentInspectionFromGraph(g, g.Nodes[2])
	if insp.Node.Layer != LayerService {
		t.Fatalf("expected service layer, got %s", insp.Node.Layer)
	}
	if len(insp.Responsibilities) < 3 {
		t.Fatalf("expected >=3 responsibilities, got %v", insp.Responsibilities)
	}
	if len(insp.Interfaces) != 2 {
		t.Fatalf("expected 2 interfaces (exposes API + publishes topic), got %+v", insp.Interfaces)
	}
	if len(insp.DataOwnership.Writes) != 1 || insp.DataOwnership.Writes[0].ID != "n-db" {
		t.Fatalf("expected write ownership of n-db, got %+v", insp.DataOwnership)
	}
	if len(insp.Dependencies.Upstream) != 2 {
		t.Fatalf("expected 2 upstream edges (domain owns, threat depends_on), got %+v", insp.Dependencies.Upstream)
	}
	if len(insp.FailureModes) < 3 {
		t.Fatalf("expected >=3 failure modes (SLO + threat + structural SPOF), got %+v", insp.FailureModes)
	}
}
