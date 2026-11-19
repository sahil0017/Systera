package policy

import (
	"encoding/json"
	"testing"

	"github.com/systera/control-plane/internal/graph"
)

func TestEvaluatePatchRisk(t *testing.T) {
	low := EvaluatePatchRisk([]graph.Operation{
		{Op: graph.OpAddNode, TargetID: "n1", Payload: json.RawMessage(`{"type":"service"}`)},
	})
	if low.Level != RiskLow || low.RequiresApproval {
		t.Fatalf("expected LOW risk without approval, got %+v", low)
	}

	removeHigh := EvaluatePatchRisk([]graph.Operation{
		{Op: graph.OpRemoveNode, TargetID: "n1", Payload: json.RawMessage(`{}`)},
	})
	if removeHigh.Level != RiskHigh || !removeHigh.RequiresApproval {
		t.Fatalf("expected HIGH risk for node removal, got %+v", removeHigh)
	}

	secretHigh := EvaluatePatchRisk([]graph.Operation{
		{Op: graph.OpAddNode, TargetID: "n2", Payload: json.RawMessage(`{"type":"secret_boundary"}`)},
	})
	if secretHigh.Level != RiskHigh || !secretHigh.RequiresApproval {
		t.Fatalf("expected HIGH risk for secret_boundary node, got %+v", secretHigh)
	}
}
