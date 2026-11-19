package learning

import (
	"strings"
	"testing"

	"github.com/systera/control-plane/internal/graph"
)

func inspection() graph.ComponentInspection {
	var insp graph.ComponentInspection
	insp.ModelID, insp.Version = "m1", 4
	insp.Node = graph.XRayNode{Node: graph.Node{ID: "n1", Key: "svc:orders", Type: "service", Name: "Order Service", Description: "Owns order placement", EvidenceState: "PROPOSED"}, Layer: graph.LayerService}
	insp.Responsibilities = []string{"Owns order placement"}
	insp.Dependencies.Upstream = []graph.EdgeNodeRef{{Node: graph.NodeRef{ID: "n0", Name: "Checkout API", Type: "api"}, RelationType: "calls"}}
	insp.Dependencies.Downstream = []graph.EdgeNodeRef{
		{Node: graph.NodeRef{ID: "n2", Name: "Payments Provider", Type: "external_dependency"}, RelationType: "calls", SyncMode: "SYNC"},
	}
	insp.Interfaces = []graph.InterfaceSpec{{Role: "PUBLISHES", RelationType: "publishes", Protocol: "NATS", SyncMode: "ASYNC", Target: graph.NodeRef{Name: "order-events", Type: "topic"}}}
	insp.DataOwnership.Writes = []graph.NodeRef{{Name: "Orders DB", Type: "database"}}
	insp.FailureModes = []graph.FailureMode{{Title: "Synchronous dependency on Payments Provider", Severity: "HIGH", Source: "STRUCTURAL", Description: "Provider outage blocks order placement."}}
	insp.Alternatives = []graph.InspectedAlternative{{Source: "ADR", DecisionID: "d1", Title: "Architecture style", ChosenOption: "Modular Monolith", Option: "Domain Microservices", Pros: "Independent scaling", Cons: "Operational cost", RejectionReason: "Appropriate with > 25 engineers"}}
	return insp
}

func TestBuildLessonCoversEveryLearningOutput(t *testing.T) {
	l := BuildLesson(inspection(), nil, "NONE_FOUND")
	if l.Disclaimer == "" || l.EvidenceState != "PROPOSED" || l.NodeName != "Order Service" {
		t.Fatalf("header: %+v", l)
	}
	if l.WhyItExists.Summary == "" || !strings.Contains(strings.Join(l.WhyItExists.Details, " "), "Checkout API") {
		t.Fatalf("why it exists must cite dependents: %+v", l.WhyItExists)
	}
	if !strings.Contains(strings.Join(l.ProblemSolved.Details, " "), "Orders DB") {
		t.Fatalf("problem solved must cite data ownership: %+v", l.ProblemSolved)
	}
	if len(l.Alternatives) < 2 || l.Alternatives[0].Source != "ADR" || l.Alternatives[0].DecisionID != "d1" {
		t.Fatalf("ADR alternatives come first: %+v", l.Alternatives)
	}
	if l.FailureScenarios[0].Source != "STRUCTURAL" || !strings.Contains(l.FailureScenarios[0].Recovery, "No mitigation recorded") {
		t.Fatalf("graph failure modes come first and flag missing mitigation: %+v", l.FailureScenarios[0])
	}
	qs := ""
	for _, q := range l.InterviewQuestions {
		qs += q.Question + "\n"
		if len(q.GoodAnswersCover) == 0 {
			t.Fatalf("question without answer guidance: %q", q.Question)
		}
	}
	for _, want := range []string{"Order Service is down", "calls Payments Provider synchronously", "chose Modular Monolith over Domain Microservices"} {
		if !strings.Contains(qs, want) {
			t.Fatalf("missing design-derived question %q in:\n%s", want, qs)
		}
	}
	for _, want := range []string{"Transactional outbox publisher", "Circuit breaker and reconciliation"} {
		if !hasExercise(l.Exercises, want) {
			t.Fatalf("missing exercise %q: %+v", want, l.Exercises)
		}
	}
	if l.ResearchStatus != "NONE_FOUND" || l.Research == nil {
		t.Fatalf("research: %v %v", l.ResearchStatus, l.Research)
	}
}

func TestChallengeRevisesExplanationWithoutChangingDesign(t *testing.T) {
	l := BuildLesson(inspection(), nil, "NONE_FOUND")
	cases := map[string]string{
		"Why not use domain microservices instead?":        ConcernAlternative,
		"This seems overkill, can we simplify it?":         ConcernComplexity,
		"What happens when the payments provider is down?": ConcernReliability,
		"Will this be too expensive to run?":               ConcernCost,
		"Can this handle Black Friday traffic?":            ConcernScale,
		"What stops a duplicate order?":                    ConcernConsistency,
		"Is customer PII leaking through the events?":      ConcernSecurity,
		"I don't get it.":                                  ConcernGeneral,
	}
	for text, want := range cases {
		r := ReviseExplanation(l, text)
		if r.Concern != want {
			t.Errorf("%q: concern %s, want %s", text, r.Concern, want)
		}
		if !r.DesignUnchanged || r.Summary == "" || r.WhenYouAreRight == "" || len(r.Response) == 0 {
			t.Errorf("%q: incomplete revision %+v", text, r)
		}
	}
	alt := ReviseExplanation(l, "Why not use domain microservices instead?")
	if len(alt.ConsiderInstead) == 0 || alt.ConsiderInstead[0].Option != "Domain Microservices" || !strings.Contains(alt.WhenYouAreRight, "25 engineers") {
		t.Fatalf("alternative challenge: %+v", alt)
	}
	rel := ReviseExplanation(l, "What happens when the payments provider is down?")
	if len(rel.RelevantFailures) == 0 {
		t.Fatalf("reliability challenge must cite failure scenarios: %+v", rel)
	}
}

func TestNormalizeChallenge(t *testing.T) {
	if _, err := NormalizeChallenge("   "); err == nil {
		t.Fatal("empty challenge accepted")
	}
	if _, err := NormalizeChallenge(strings.Repeat("a", MaxChallengeChars+1)); err == nil {
		t.Fatal("oversized challenge accepted")
	}
	if got, err := NormalizeChallenge("  why?  "); err != nil || got != "why?" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestEveryNodeTypeHasGuidance(t *testing.T) {
	for _, nt := range graph.NodeTypes {
		g := guideFor(nt)
		if g.Purpose == "" || g.Problem == "" || len(g.Questions) == 0 {
			t.Errorf("node type %s lacks learning guidance", nt)
		}
	}
}

func TestOverview(t *testing.T) {
	g := graph.Graph{Version: graph.Version{ModelID: "m1", Number: 2}, Nodes: []graph.Node{
		{ID: "a", Type: "service", Name: "Orders"}, {ID: "b", Type: "service", Name: "Billing"},
		{ID: "c", Type: "topic", Name: "order-events"}, {ID: "d", Type: "test", Name: "t"},
	}}
	ov := BuildOverview(g)
	if len(ov.Components) != 3 || ov.Components[0].OneLiner == "" {
		t.Fatalf("components: %+v", ov.Components)
	}
	if !hasExercise(ov.Exercises, "Transactional outbox publisher") || len(ov.InterviewQuestions) < 3 {
		t.Fatalf("overview questions/exercises: %+v", ov)
	}
}
