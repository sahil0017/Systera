package decisions

import (
	"testing"

	"github.com/systera/control-plane/internal/platform/ids"
)

func TestADRStateMachine(t *testing.T) {
	allowed := [][2]Status{
		{StatusProposed, StatusAccepted},
		{StatusProposed, StatusRejected},
		{StatusProposed, StatusSuperseded},
		{StatusProposed, StatusNeedsReview},
		{StatusAccepted, StatusSuperseded},
		{StatusAccepted, StatusNeedsReview},
		{StatusNeedsReview, StatusAccepted},
		{StatusNeedsReview, StatusRejected},
		{StatusNeedsReview, StatusSuperseded},
	}
	for _, tc := range allowed {
		if !CanTransition(tc[0], tc[1]) {
			t.Errorf("expected transition %s -> %s to be allowed", tc[0], tc[1])
		}
	}

	forbidden := [][2]Status{
		{StatusAccepted, StatusProposed},
		{StatusAccepted, StatusRejected},
		{StatusRejected, StatusAccepted},
		{StatusRejected, StatusSuperseded},
		{StatusSuperseded, StatusAccepted},
		{StatusSuperseded, StatusNeedsReview},
		{StatusNeedsReview, StatusProposed},
	}
	for _, tc := range forbidden {
		if CanTransition(tc[0], tc[1]) {
			t.Errorf("expected transition %s -> %s to be rejected", tc[0], tc[1])
		}
	}
}

func TestValidateCreateADRFields(t *testing.T) {
	nodeID := ids.New()
	good := CreateInput{
		ModelVersion:   1,
		Title:          "Use PostgreSQL + Redis for URL Shortener",
		Context:        "Need 100k read QPS and durable short-code mappings.",
		ChosenOption:   "PostgreSQL primary store with Redis read-through cache",
		Alternatives:   []Alternative{{Option: "DynamoDB only", Cons: "Harder ad-hoc analytics"}},
		Rationale:      "Strong consistency for alias uniqueness plus sub-ms cached redirects.",
		TradeOffs:      "Requires cache invalidation discipline on alias updates.",
		Consequences:   "Operate Redis cluster alongside RDS.",
		EvidenceRefs:   []string{"ev-capacity-1"},
		ConfidenceBand: "HIGH",
		AffectedNodes:  []string{nodeID},
	}
	if err := ValidateCreate(&good); err != nil {
		t.Fatalf("expected valid ADR input, got %v", err)
	}

	missingContext := good
	missingContext.Context = ""
	if err := ValidateCreate(&missingContext); err == nil {
		t.Fatal("expected error for missing context")
	}

	badNode := good
	badNode.AffectedNodes = []string{"not-a-uuid"}
	if err := ValidateCreate(&badNode); err == nil {
		t.Fatal("expected error for invalid affected_nodes UUID")
	}

	badConf := good
	badConf.ConfidenceBand = "CERTAIN"
	if err := ValidateCreate(&badConf); err == nil {
		t.Fatal("expected error for invalid confidence_band")
	}
}
