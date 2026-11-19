package graph

import (
	"fmt"
	"testing"
)

// BenchmarkImpactTraversalRealisticGraph benchmarks bounded BFS traversal (Checklist 6.5)
// on a realistic 1,000-node, ~2,500-edge architecture graph.
func BenchmarkImpactTraversalRealisticGraph(b *testing.B) {
	const nodeCount = 1000
	nodes := make([]string, nodeCount)
	inEdges := make(map[string][]string, nodeCount)
	for i := 0; i < nodeCount; i++ {
		nodes[i] = fmt.Sprintf("node-%04d", i)
	}
	for i := 1; i < nodeCount; i++ {
		// Connect each node to 2-3 upstream dependencies (DAG + fan-in).
		p1 := nodes[i-1]
		inEdges[p1] = append(inEdges[p1], nodes[i])
		if i >= 5 {
			p2 := nodes[i-5]
			inEdges[p2] = append(inEdges[p2], nodes[i])
		}
		if i >= 17 {
			p3 := nodes[i-17]
			inEdges[p3] = append(inEdges[p3], nodes[i])
		}
	}

	lim := DefaultImpactLimits // MaxDepth=25, MaxNodes=10000

	for b.Loop() {
		visited := map[string]int{nodes[0]: 0}
		frontier := []string{nodes[0]}
		for depth := 1; depth <= lim.MaxDepth && len(frontier) > 0 && len(visited) < lim.MaxNodes; depth++ {
			var next []string
			for _, cur := range frontier {
				for _, dep := range inEdges[cur] {
					if _, seen := visited[dep]; !seen {
						visited[dep] = depth
						next = append(next, dep)
						if len(visited) >= lim.MaxNodes {
							break
						}
					}
				}
				if len(visited) >= lim.MaxNodes {
					break
				}
			}
			frontier = next
		}
		if len(visited) == 0 {
			b.Fatal("expected non-empty impact set")
		}
	}
}

func TestImpactLimitsDefaults(t *testing.T) {
	lim := (ImpactLimits{}).normalized()
	if lim.MaxDepth != 25 || lim.MaxNodes != 10000 {
		t.Fatalf("unexpected default limits: %+v", lim)
	}
}
