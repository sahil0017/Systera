// Package artifacts generates architecture projections (service catalogue, ERD/SQL DDL,
// OpenAPI skeleton, Mermaid diagram) from the canonical Architecture Graph using an
// adaptive, non-deterministic reinforcement learning policy (§7, §23.12).
package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/systera/control-plane/internal/graph"
)

// ProjectionType enumerates the first-class artifact projections (§7.2).
type ProjectionType string

const (
	ProjectionCatalogue ProjectionType = "CATALOGUE"
	ProjectionSQL       ProjectionType = "SQL"
	ProjectionOpenAPI   ProjectionType = "OPENAPI"
	ProjectionMermaid   ProjectionType = "MERMAID"
)

// ProjectedArtifact is an immutable projection generated from a specific canonical graph version.
type ProjectedArtifact struct {
	ModelID        string         `json:"model_id"`
	GraphVersion   int            `json:"graph_version"`
	GraphHash      string         `json:"graph_hash"`
	ProjectionType ProjectionType `json:"projection_type"`
	StrategyArm    StrategyArm    `json:"strategy_arm"`
	Explored       bool           `json:"explored"`
	PolicyVersion  int            `json:"policy_version"`
	ContentHash    string         `json:"content_hash"`
	Content        string         `json:"content"`
}

// Projector generates graph-driven artifacts guided by an RLPolicy.
type Projector struct {
	policy *RLPolicy
}

func NewProjector(policy *RLPolicy) *Projector {
	if policy == nil {
		policy = NewRLPolicy(0.2, 0.25, 0.2)
	}
	return &Projector{policy: policy}
}

// RecordReward forwards an evaluation/user reward signal to the RL policy.
func (p *Projector) RecordReward(sig RewardSignal) ArmStats {
	return p.policy.RecordReward(sig)
}

// Generate selects a strategy arm via the RL policy and projects the canonical graph
// into the requested artifact type.
func (p *Projector) Generate(g graph.Graph, pt ProjectionType) (ProjectedArtifact, error) {
	sel := p.policy.SelectArm(pt)
	var content string
	switch pt {
	case ProjectionCatalogue:
		content = renderCatalogue(g, sel.Arm)
	case ProjectionSQL:
		content = renderSQL(g, sel.Arm)
	case ProjectionOpenAPI:
		content = renderOpenAPI(g, sel.Arm)
	case ProjectionMermaid:
		content = renderMermaid(g, sel.Arm)
	default:
		return ProjectedArtifact{}, fmt.Errorf("unsupported projection type %q", pt)
	}

	sum := sha256.Sum256([]byte(content))
	return ProjectedArtifact{
		ModelID:        g.Version.ModelID,
		GraphVersion:   g.Version.Number,
		GraphHash:      g.Version.GraphHash,
		ProjectionType: pt,
		StrategyArm:    sel.Arm,
		Explored:       sel.Explored,
		PolicyVersion:  sel.PolicyVersion,
		ContentHash:    hex.EncodeToString(sum[:]),
		Content:        content,
	}, nil
}

func sortedNodes(g graph.Graph) []graph.Node {
	nodes := make([]graph.Node, len(g.Nodes))
	copy(nodes, g.Nodes)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Key < nodes[j].Key })
	return nodes
}

func renderCatalogue(g graph.Graph, arm StrategyArm) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Component & Service Catalogue (Graph v%d | Strategy: %s)\n\n", g.Version.Number, arm)
	for _, n := range sortedNodes(g) {
		fmt.Fprintf(&b, "- **%s** (`%s`, type=`%s`)", n.Name, n.Key, n.Type)
		if arm == ArmDetailed || arm == ArmLearningFocused {
			fmt.Fprintf(&b, " — evidence=`%s`, lifecycle=`%s`", n.EvidenceState, n.LifecycleState)
			if n.OwnerDomain != "" {
				fmt.Fprintf(&b, ", domain=`%s`", n.OwnerDomain)
			}
		}
		if n.Description != "" && arm != ArmConcise {
			fmt.Fprintf(&b, ": %s", n.Description)
		}
		b.WriteByte('\n')
		if arm == ArmLearningFocused {
			fmt.Fprintf(&b, "  - *Trade-off / Failure check*: verify scaling, timeouts, and ownership boundaries for `%s`.\n", n.Key)
		}
	}
	return b.String()
}

func renderSQL(g graph.Graph, arm StrategyArm) string {
	var b strings.Builder
	fmt.Fprintf(&b, "-- Canonical ERD / SQL DDL (Graph v%d | Strategy: %s)\n\n", g.Version.Number, arm)
	for _, n := range sortedNodes(g) {
		if n.Type != "table" && n.Type != "database" {
			continue
		}
		tbl := sanitizeIdent(n.Key)
		if arm == ArmDetailed || arm == ArmLearningFocused {
			fmt.Fprintf(&b, "-- Source node: %s (%s, evidence=%s)\n", n.Name, n.ID, n.EvidenceState)
		}
		fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS %s (\n", tbl)
		b.WriteString("    id UUID PRIMARY KEY,\n")
		b.WriteString("    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()\n")
		b.WriteString(");\n\n")
	}
	return b.String()
}

func renderOpenAPI(g graph.Graph, arm StrategyArm) string {
	var b strings.Builder
	b.WriteString("openapi: 3.1.0\ninfo:\n")
	fmt.Fprintf(&b, "  title: Systera Projected API (v%d)\n", g.Version.Number)
	fmt.Fprintf(&b, "  version: \"0.%d.0\"\n", g.Version.Number)
	if arm == ArmDetailed || arm == ArmLearningFocused {
		fmt.Fprintf(&b, "  description: \"Generated from canonical graph %s using RL strategy %s\"\n", g.Version.GraphHash, arm)
	}
	b.WriteString("paths:\n")
	hasPath := false
	for _, n := range sortedNodes(g) {
		if n.Type != "api" {
			continue
		}
		hasPath = true
		path := "/" + strings.ReplaceAll(strings.TrimPrefix(n.Key, "api."), ".", "/")
		fmt.Fprintf(&b, "  %s:\n    get:\n      summary: %s\n", path, n.Name)
		if arm == ArmDetailed || arm == ArmLearningFocused {
			fmt.Fprintf(&b, "      x-evidence-state: %s\n", n.EvidenceState)
		}
		b.WriteString("      responses:\n        \"200\":\n          description: OK\n")
	}
	if !hasPath {
		b.WriteString("  {}\n")
	}
	return b.String()
}

func renderMermaid(g graph.Graph, arm StrategyArm) string {
	var b strings.Builder
	b.WriteString("flowchart TD\n")
	byID := make(map[string]graph.Node, len(g.Nodes))
	for _, n := range sortedNodes(g) {
		byID[n.ID] = n
		id := sanitizeIdent(n.Key)
		if arm == ArmConcise {
			fmt.Fprintf(&b, "    %s[\"%s\"]\n", id, n.Name)
		} else {
			fmt.Fprintf(&b, "    %s[\"%s (%s)\"]\n", id, n.Name, n.Type)
		}
	}
	edges := make([]graph.Edge, len(g.Edges))
	copy(edges, g.Edges)
	sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })
	for _, e := range edges {
		src, sok := byID[e.SourceNodeID]
		dst, dok := byID[e.TargetNodeID]
		if !sok || !dok {
			continue
		}
		fmt.Fprintf(&b, "    %s -->|%s| %s\n", sanitizeIdent(src.Key), e.RelationType, sanitizeIdent(dst.Key))
	}
	return b.String()
}

func sanitizeIdent(s string) string {
	r := strings.NewReplacer(".", "_", "-", "_", ":", "_")
	return r.Replace(s)
}
