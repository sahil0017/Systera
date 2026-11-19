package counterfactual

import (
	"encoding/json"
	"testing"

	"github.com/systera/control-plane/internal/graph"
)

func TestGenerateCounterfactualSetAndDecisionMatrix(t *testing.T) {
	// Must reject < 2 alternatives.
	if _, err := GenerateCounterfactualSet("Shop", []Style{StyleModularMonolith}, ConstraintProfile{}); err == nil {
		t.Fatal("expected error when fewer than 2 alternatives requested")
	}

	// Default generation produces 3 viable designs scored across all 8 constraints.
	set, err := GenerateCounterfactualSet("Commerce Platform", nil, ConstraintProfile{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(set.Alternatives) != 3 {
		t.Fatalf("expected 3 alternatives, got %d", len(set.Alternatives))
	}
	if len(set.DecisionMatrix.Constraints) != 8 || len(set.DecisionMatrix.Rows) != 8 {
		t.Fatalf("expected 8 constraints in decision matrix, got %d / %d", len(set.DecisionMatrix.Constraints), len(set.DecisionMatrix.Rows))
	}
	for _, a := range set.Alternatives {
		if a.WhenAppropriate == "" {
			t.Fatalf("alternative %s missing when_appropriate explanation", a.Style)
		}
		if len(a.Scores) != 8 {
			t.Fatalf("alternative %s missing constraint scores: got %d", a.Style, len(a.Scores))
		}
	}

	// Heavily weighting scale + compliance + availability should recommend microservices or event-driven over monolith.
	heavyScale := ConstraintProfile{
		Weights: map[Constraint]float64{
			ConstraintScale:               5.0,
			ConstraintAvailability:        5.0,
			ConstraintCompliance:          5.0,
			ConstraintCost:                0.1,
			ConstraintLatency:             0.1,
			ConstraintOperationalMaturity: 0.1,
			ConstraintMigrationComplexity: 0.1,
			ConstraintTeamSize:            3.0,
		},
	}
	scaledSet, err := GenerateCounterfactualSet("Global Payments", nil, heavyScale)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if scaledSet.DecisionMatrix.RecommendedStyle != StyleDomainMicroservices {
		t.Fatalf("expected domain_microservices to win under heavy scale/compliance weights, got %s", scaledSet.DecisionMatrix.RecommendedStyle)
	}
}

func TestBuildAlternativePatchesPassGraphRelationRules(t *testing.T) {
	set, err := GenerateCounterfactualSet("Orders", nil, ConstraintProfile{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, alt := range set.Alternatives {
		patch := BuildAlternativePatch(0, "Orders", alt, "idem-"+string(alt.Style))
		nodeTypes := map[string]string{}
		for _, op := range patch.Operations {
			if op.Op == graph.OpAddNode {
				var p graph.AddNodePayload
				if err := json.Unmarshal(op.Payload, &p); err != nil {
					t.Fatalf("%s add_node unmarshal: %v", alt.Style, err)
				}
				nodeTypes[op.TargetID] = p.Type
			}
		}
		edgeCount := 0
		for _, op := range patch.Operations {
			if op.Op == graph.OpAddEdge {
				edgeCount++
				var p graph.AddEdgePayload
				if err := json.Unmarshal(op.Payload, &p); err != nil {
					t.Fatalf("%s add_edge unmarshal: %v", alt.Style, err)
				}
				if err := graph.CheckRelation(p.RelationType, nodeTypes[p.SourceNodeID], nodeTypes[p.TargetNodeID]); err != nil {
					t.Fatalf("%s invalid relation %s (%s -> %s): %v", alt.Style, p.RelationType, nodeTypes[p.SourceNodeID], nodeTypes[p.TargetNodeID], err)
				}
			}
		}
		if len(nodeTypes) < 5 || edgeCount < 4 {
			t.Fatalf("%s expected rich topology, got %d nodes and %d edges", alt.Style, len(nodeTypes), edgeCount)
		}
	}
}
