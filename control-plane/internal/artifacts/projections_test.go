package artifacts

import (
	"context"
	"strings"
	"testing"

	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/objectstore"
)

func sampleURLShortenerGraph() graph.Graph {
	svcID, apiID, tblID := ids.New(), ids.New(), ids.New()
	return graph.Graph{
		Version: graph.Version{ModelID: ids.New(), Number: 1, GraphHash: "deadbeef"},
		Nodes: []graph.Node{
			{ID: svcID, Key: "svc.shortener", Type: "service", Name: "Shortener Service", LifecycleState: "PROPOSED", EvidenceState: "PROPOSED", OwnerDomain: "urls"},
			{ID: apiID, Key: "api.shorten", Type: "api", Name: "Create Short URL", LifecycleState: "PROPOSED", EvidenceState: "OBSERVED"},
			{ID: tblID, Key: "table.short_urls", Type: "table", Name: "short_urls", LifecycleState: "PROPOSED", EvidenceState: "INFERRED"},
		},
		Edges: []graph.Edge{
			{ID: ids.New(), SourceNodeID: svcID, TargetNodeID: apiID, RelationType: "owns"},
			{ID: ids.New(), SourceNodeID: svcID, TargetNodeID: tblID, RelationType: "writes"},
		},
	}
}

func TestProjectionsCoverAllFourTypes(t *testing.T) {
	g := sampleURLShortenerGraph()
	proj := NewProjector(NewRLPolicy(0.5, 0.5, 0.2))

	for _, pt := range []ProjectionType{ProjectionCatalogue, ProjectionSQL, ProjectionOpenAPI, ProjectionMermaid} {
		art, err := proj.Generate(g, pt)
		if err != nil {
			t.Fatalf("generate %s: %v", pt, err)
		}
		if art.Content == "" || art.ContentHash == "" || art.StrategyArm == "" {
			t.Fatalf("incomplete artifact for %s: %+v", pt, art)
		}
	}

	sqlArt, _ := proj.Generate(g, ProjectionSQL)
	if !strings.Contains(sqlArt.Content, "CREATE TABLE IF NOT EXISTS table_short_urls") {
		t.Fatalf("unexpected SQL projection:\n%s", sqlArt.Content)
	}

	mmdArt, _ := proj.Generate(g, ProjectionMermaid)
	if !strings.Contains(mmdArt.Content, "flowchart TD") || !strings.Contains(mmdArt.Content, "svc_shortener -->|writes| table_short_urls") {
		t.Fatalf("unexpected Mermaid projection:\n%s", mmdArt.Content)
	}
}

func TestRLPolicyNonDeterministicExplorationAndConvergence(t *testing.T) {
	// 1. With epsilon=1.0 (full exploration), multiple strategy arms are sampled non-deterministically.
	explorePolicy := NewRLPolicy(1.0, 0.5, 0.2)
	seen := make(map[StrategyArm]bool)
	for i := 0; i < 40; i++ {
		sel := explorePolicy.SelectArm(ProjectionCatalogue)
		if !sel.Explored {
			t.Fatal("expected Explored=true when epsilon=1.0")
		}
		seen[sel.Arm] = true
	}
	if len(seen) < 2 {
		t.Fatalf("expected non-deterministic exploration of multiple arms, saw only %v", seen)
	}

	// 2. After positive reward feedback on ArmDetailed and zero reward on others,
	// greedy exploitation (epsilon=0) converges to ArmDetailed.
	policy := NewRLPolicy(0.0, 0.25, 0.5)
	for i := 0; i < 10; i++ {
		policy.RecordReward(RewardSignal{ProjectionType: ProjectionOpenAPI, Arm: ArmDetailed, Reward: 1.0})
		policy.RecordReward(RewardSignal{ProjectionType: ProjectionOpenAPI, Arm: ArmConcise, Reward: 0.0})
		policy.RecordReward(RewardSignal{ProjectionType: ProjectionOpenAPI, Arm: ArmStandard, Reward: 0.1})
		policy.RecordReward(RewardSignal{ProjectionType: ProjectionOpenAPI, Arm: ArmLearningFocused, Reward: 0.2})
	}
	sel := policy.SelectArm(ProjectionOpenAPI)
	if sel.Arm != ArmDetailed {
		t.Fatalf("expected RL policy to converge to %s, got %s", ArmDetailed, sel.Arm)
	}
}

func TestAsyncProjectorValidatesAndUploadsToObjectStore(t *testing.T) {
	ctx := context.Background()
	g := sampleURLShortenerGraph()
	mem := objectstore.NewMemoryStore("systera-artifacts")
	ap := NewAsyncProjector(NewProjector(NewRLPolicy(0.0, 0.25, 0.2)), mem, nil, nil, nil)

	for _, pt := range []ProjectionType{ProjectionCatalogue, ProjectionSQL, ProjectionOpenAPI, ProjectionMermaid} {
		res, err := ap.PersistFromGraph(ctx, "tenant-1", g, pt, Actor{Type: "USER", ID: "u1"})
		if err != nil {
			t.Fatalf("PersistFromGraph %s: %v", pt, err)
		}
		if !res.Validation.Valid || !strings.HasPrefix(res.ContentRef, "s3://systera-artifacts/tenant-1/") {
			t.Fatalf("unexpected persisted projection for %s: %+v", pt, res)
		}
	}
}
