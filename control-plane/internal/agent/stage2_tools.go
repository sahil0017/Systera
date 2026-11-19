package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/systera/control-plane/internal/artifacts"
	"github.com/systera/control-plane/internal/decisions"
	"github.com/systera/control-plane/internal/evidence"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/policy"
	"github.com/systera/control-plane/internal/tools"
)

// Stage 2 capabilities gated by the Tool Gateway.
const (
	CapGraphRead         = "graph.read"
	CapGraphProposePatch = "graph.propose_patch"
	CapEvidenceWrite     = "evidence.write"
	CapDecisionRead      = "decision.read"
	CapDecisionPropose   = "decision.propose"
	CapArtifactCreate    = "artifact.create"
)

// RegisterStage2Tools registers graph, evidence-ledger, and decision-ledger tools
// for specialist agents. Every tool scopes writes to the caller's verified lease.
func RegisterStage2Tools(
	reg *tools.Registry,
	graphSvc *graph.Service,
	evStore *evidence.Store,
	decStore *decisions.Store,
	approvals ...*policy.ApprovalStore,
) error {
	var approvalStore *policy.ApprovalStore
	if len(approvals) > 0 {
		approvalStore = approvals[0]
	}
	if graphSvc != nil {
		if err := registerGraphTools(reg, graphSvc, approvalStore); err != nil {
			return err
		}
	}
	if evStore != nil && graphSvc != nil {
		if err := registerEvidenceTools(reg, graphSvc, evStore); err != nil {
			return err
		}
	}
	if decStore != nil && graphSvc != nil {
		if err := registerDecisionTools(reg, graphSvc, decStore); err != nil {
			return err
		}
	}
	return nil
}

func resolveProjectModel(ctx context.Context, graphSvc *graph.Service, tenantID, projectID string, args map[string]any) (graph.Model, error) {
	m, err := graphSvc.ModelForProject(ctx, tenantID, projectID)
	if err != nil {
		return graph.Model{}, err
	}
	if reqID, _ := args["model_id"].(string); reqID != "" && reqID != m.ID {
		return graph.Model{}, fmt.Errorf("model_id %s does not belong to this run's project", reqID)
	}
	return m, nil
}

func registerGraphTools(reg *tools.Registry, graphSvc *graph.Service, approvalStore *policy.ApprovalStore) error {
	if err := reg.Register(tools.Tool{
		Name:        "graph.read",
		Description: "Reads the canonical architecture graph for this project at the current (or specified) version.",
		Capability:  CapGraphRead,
		InputSchema: tools.MustSchema(`{"type":"object","properties":{"version":{"type":"integer","minimum":0}}}`),
		Handler: func(ctx context.Context, _ tools.Invocation, args map[string]any) (map[string]any, error) {
			l, err := leaseFrom(ctx)
			if err != nil {
				return nil, err
			}
			m, err := resolveProjectModel(ctx, graphSvc, l.Task.TenantID, l.Run.ProjectID, args)
			if err != nil {
				return nil, err
			}
			ver := m.CurrentVersion
			if v, ok := args["version"].(float64); ok {
				ver = int(v)
			}
			g, err := graphSvc.GraphAt(ctx, l.Task.TenantID, m.ID, ver)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"model_id":   m.ID,
				"version":    g.Version.Number,
				"graph_hash": g.Version.GraphHash,
				"nodes":      g.Nodes,
				"edges":      g.Edges,
			}, nil
		},
	}); err != nil {
		return err
	}

	if err := reg.Register(tools.Tool{
		Name:        "graph.query",
		Description: "Queries the neighbourhood reachable from a node in the canonical architecture graph.",
		Capability:  CapGraphRead,
		InputSchema: tools.MustSchema(`{"type":"object","required":["start_node_id"],"properties":{"start_node_id":{"type":"string","minLength":1},"direction":{"type":"string"},"max_depth":{"type":"integer","minimum":1,"maximum":10},"version":{"type":"integer","minimum":0}}}`),
		Handler: func(ctx context.Context, _ tools.Invocation, args map[string]any) (map[string]any, error) {
			l, err := leaseFrom(ctx)
			if err != nil {
				return nil, err
			}
			m, err := resolveProjectModel(ctx, graphSvc, l.Task.TenantID, l.Run.ProjectID, args)
			if err != nil {
				return nil, err
			}
			ver := m.CurrentVersion
			if v, ok := args["version"].(float64); ok {
				ver = int(v)
			}
			start, _ := args["start_node_id"].(string)
			dir, _ := args["direction"].(string)
			if dir == "" {
				dir = "both"
			}
			depth := 2
			if d, ok := args["max_depth"].(float64); ok {
				depth = int(d)
			}
			res, err := graphSvc.Reachable(ctx, l.Task.TenantID, m.ID, ver, start, graph.TraversalQuery{
				Direction: graph.Direction(dir),
				MaxNodes:  depth * 100,
			})
			if err != nil {
				return nil, err
			}
			return toMap(res)
		},
	}); err != nil {
		return err
	}

	return reg.Register(tools.Tool{
		Name:        "graph.propose_patch",
		Description: "Submits a GraphPatch to the Architecture Intelligence Core with optimistic concurrency and policy risk gating.",
		Capability:  CapGraphProposePatch,
		InputSchema: tools.MustSchema(`{"type":"object","required":["base_graph_version","idempotency_key","operations"],"properties":{"base_graph_version":{"type":"integer","minimum":0},"idempotency_key":{"type":"string","minLength":1},"expected_invariants":{"type":"array","items":{"type":"string"}},"operations":{"type":"array","minItems":1}}}`),
		Handler: func(ctx context.Context, _ tools.Invocation, args map[string]any) (map[string]any, error) {
			l, err := leaseFrom(ctx)
			if err != nil {
				return nil, err
			}
			m, err := resolveProjectModel(ctx, graphSvc, l.Task.TenantID, l.Run.ProjectID, args)
			if err != nil {
				return nil, err
			}
			raw, err := json.Marshal(args)
			if err != nil {
				return nil, err
			}
			var patch graph.Patch
			if err := json.Unmarshal(raw, &patch); err != nil {
				return nil, err
			}
			patch.RunID = l.Run.RunID
			patch.AgentTaskID = l.Task.ID

			// Checklist 8.5: High-risk change sets require human approval unless an approved ID is supplied.
			risk := policy.EvaluatePatchRisk(patch.Operations)
			if risk.RequiresApproval && approvalStore != nil {
				fp, _ := patch.Fingerprint()
				req, aErr := approvalStore.RequestApproval(ctx, l.Task.TenantID, m.ID, l.Run.RunID, m.ID, fp, l.Task.AgentType, risk, raw)
				if aErr != nil {
					return nil, aErr
				}
				return map[string]any{
					"accepted":        false,
					"status":          "APPROVAL_REQUIRED",
					"approval_id":     req.ID,
					"risk_level":      risk.Level,
					"risk_reasons":    risk.Reasons,
					"model_id":        m.ID,
					"current_version": m.CurrentVersion,
				}, nil
			}

			author := graph.Author{Type: "AGENT", ID: l.Task.AgentType}
			res, err := graphSvc.ApplyPatch(ctx, l.Task.TenantID, m.ID, patch, author, l.Run.CorrelationID)
			if err != nil {
				return nil, err
			}
			out, err := toMap(res)
			if err != nil {
				return nil, err
			}
			if res.Status == graph.PatchConflict {
				// Checklist 8.3: Surface conflict so the agent/orchestrator spawns a corrective task.
				out["corrective_task_needed"] = true
				out["recommended_action"] = fmt.Sprintf("Re-base patch onto current_graph_version %d and resubmit", res.CurrentGraphVersion)
			}
			return out, nil
		},
	})
}

func registerEvidenceTools(reg *tools.Registry, graphSvc *graph.Service, evStore *evidence.Store) error {
	if err := reg.Register(tools.Tool{
		Name:        "evidence.capture",
		Description: "Captures a provenance-tracked evidence record for the architecture model.",
		Capability:  CapEvidenceWrite,
		InputSchema: tools.MustSchema(`{"type":"object","required":["source_ref","source_type","trust_level"],"properties":{"source_ref":{"type":"string","minLength":1},"source_type":{"type":"string"},"content":{"type":"string"},"content_hash":{"type":"string"},"excerpt":{"type":"string"},"trust_level":{"type":"string"},"access_policy":{"type":"string"}}}`),
		Handler: func(ctx context.Context, _ tools.Invocation, args map[string]any) (map[string]any, error) {
			l, err := leaseFrom(ctx)
			if err != nil {
				return nil, err
			}
			m, err := resolveProjectModel(ctx, graphSvc, l.Task.TenantID, l.Run.ProjectID, args)
			if err != nil {
				return nil, err
			}
			raw, _ := json.Marshal(args)
			var in evidence.EvidenceInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			actor := evidence.Actor{Type: "AGENT", ID: l.Task.AgentType, RunID: l.Run.RunID, AgentTaskID: l.Task.ID}
			ev, created, err := evStore.CaptureEvidence(ctx, l.Task.TenantID, m.ID, in, actor)
			if err != nil {
				return nil, err
			}
			out, err := toMap(ev)
			if err != nil {
				return nil, err
			}
			out["created"] = created
			return out, nil
		},
	}); err != nil {
		return err
	}

	if err := reg.Register(tools.Tool{
		Name:        "evidence.make_claim",
		Description: "Records a classified (OBSERVED/INFERRED/PROPOSED) claim linked to evidence and affected nodes.",
		Capability:  CapEvidenceWrite,
		InputSchema: tools.MustSchema(`{"type":"object","required":["statement","claim_class","confidence_band"],"properties":{"statement":{"type":"string","minLength":1},"claim_class":{"type":"string"},"reasoning":{"type":"string"},"confidence_band":{"type":"string"},"subject_node_ids":{"type":"array","items":{"type":"string"}},"evidence":{"type":"array"},"assumption_ids":{"type":"array","items":{"type":"string"}}}}`),
		Handler: func(ctx context.Context, _ tools.Invocation, args map[string]any) (map[string]any, error) {
			l, err := leaseFrom(ctx)
			if err != nil {
				return nil, err
			}
			m, err := resolveProjectModel(ctx, graphSvc, l.Task.TenantID, l.Run.ProjectID, args)
			if err != nil {
				return nil, err
			}
			raw, _ := json.Marshal(args)
			var in evidence.ClaimInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			actor := evidence.Actor{Type: "AGENT", ID: l.Task.AgentType, RunID: l.Run.RunID, AgentTaskID: l.Task.ID}
			cl, err := evStore.MakeClaim(ctx, l.Task.TenantID, m.ID, in, actor)
			if err != nil {
				return nil, err
			}
			return toMap(cl)
		},
	}); err != nil {
		return err
	}

	return reg.Register(tools.Tool{
		Name:        "evidence.record_assumption",
		Description: "Records an explicit architectural assumption with risk and validation method.",
		Capability:  CapEvidenceWrite,
		InputSchema: tools.MustSchema(`{"type":"object","required":["statement","rationale","risk_if_wrong"],"properties":{"statement":{"type":"string","minLength":1},"rationale":{"type":"string","minLength":1},"risk_if_wrong":{"type":"string","minLength":1},"validation_method":{"type":"string"},"subject_node_ids":{"type":"array","items":{"type":"string"}}}}`),
		Handler: func(ctx context.Context, _ tools.Invocation, args map[string]any) (map[string]any, error) {
			l, err := leaseFrom(ctx)
			if err != nil {
				return nil, err
			}
			m, err := resolveProjectModel(ctx, graphSvc, l.Task.TenantID, l.Run.ProjectID, args)
			if err != nil {
				return nil, err
			}
			raw, _ := json.Marshal(args)
			var in evidence.AssumptionInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			actor := evidence.Actor{Type: "AGENT", ID: l.Task.AgentType, RunID: l.Run.RunID, AgentTaskID: l.Task.ID}
			a, err := evStore.RecordAssumption(ctx, l.Task.TenantID, m.ID, in, actor)
			if err != nil {
				return nil, err
			}
			return toMap(a)
		},
	})
}

func registerDecisionTools(reg *tools.Registry, graphSvc *graph.Service, decStore *decisions.Store) error {
	if err := reg.Register(tools.Tool{
		Name:        "decision.propose",
		Description: "Proposes an Architecture Decision Record (ADR) with alternatives, trade-offs, and affected nodes.",
		Capability:  CapDecisionPropose,
		InputSchema: tools.MustSchema(`{"type":"object","required":["title","context","chosen_option","rationale"],"properties":{"model_version":{"type":"integer","minimum":0},"title":{"type":"string","minLength":1},"context":{"type":"string","minLength":1},"chosen_option":{"type":"string","minLength":1},"rationale":{"type":"string","minLength":1},"trade_offs":{"type":"string"},"consequences":{"type":"string"},"confidence_band":{"type":"string"},"alternatives":{"type":"array"},"evidence_refs":{"type":"array","items":{"type":"string"}},"affected_nodes":{"type":"array","items":{"type":"string"}}}}`),
		Handler: func(ctx context.Context, _ tools.Invocation, args map[string]any) (map[string]any, error) {
			l, err := leaseFrom(ctx)
			if err != nil {
				return nil, err
			}
			m, err := resolveProjectModel(ctx, graphSvc, l.Task.TenantID, l.Run.ProjectID, args)
			if err != nil {
				return nil, err
			}
			raw, _ := json.Marshal(args)
			var in decisions.CreateInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			if in.ModelVersion == 0 {
				in.ModelVersion = m.CurrentVersion
			}
			actor := decisions.Actor{Type: "AGENT", ID: l.Task.AgentType, RunID: l.Run.RunID}
			d, err := decStore.Create(ctx, l.Task.TenantID, m.ID, in, actor)
			if err != nil {
				return nil, err
			}
			return toMap(d)
		},
	}); err != nil {
		return err
	}

	return reg.Register(tools.Tool{
		Name:        "decision.list",
		Description: "Lists Architecture Decision Records (ADRs) for this project's architecture model.",
		Capability:  CapDecisionRead,
		InputSchema: tools.MustSchema(`{"type":"object","properties":{"status":{"type":"string"}}}`),
		Handler: func(ctx context.Context, _ tools.Invocation, args map[string]any) (map[string]any, error) {
			l, err := leaseFrom(ctx)
			if err != nil {
				return nil, err
			}
			m, err := resolveProjectModel(ctx, graphSvc, l.Task.TenantID, l.Run.ProjectID, args)
			if err != nil {
				return nil, err
			}
			status, _ := args["status"].(string)
			list, err := decStore.List(ctx, l.Task.TenantID, m.ID, decisions.Status(status))
			if err != nil {
				return nil, err
			}
			return map[string]any{"items": list}, nil
		},
	})
}

// RegisterProjectionTool registers artifact.project so agents can generate RL-guided projections.
func RegisterProjectionTool(reg *tools.Registry, graphSvc *graph.Service, proj *artifacts.Projector) error {
	if reg == nil || graphSvc == nil || proj == nil {
		return nil
	}
	return reg.Register(tools.Tool{
		Name:        "artifact.project",
		Description: "Projects the canonical architecture graph into CATALOGUE, SQL, OPENAPI, or MERMAID using the RL policy.",
		Capability:  CapArtifactCreate,
		InputSchema: tools.MustSchema(`{"type":"object","required":["projection_type"],"properties":{"projection_type":{"type":"string"},"version":{"type":"integer","minimum":0}}}`),
		Handler: func(ctx context.Context, _ tools.Invocation, args map[string]any) (map[string]any, error) {
			l, err := leaseFrom(ctx)
			if err != nil {
				return nil, err
			}
			m, err := resolveProjectModel(ctx, graphSvc, l.Task.TenantID, l.Run.ProjectID, args)
			if err != nil {
				return nil, err
			}
			ver := m.CurrentVersion
			if v, ok := args["version"].(float64); ok {
				ver = int(v)
			}
			g, err := graphSvc.GraphAt(ctx, l.Task.TenantID, m.ID, ver)
			if err != nil {
				return nil, err
			}
			ptStr, _ := args["projection_type"].(string)
			art, err := proj.Generate(g, artifacts.ProjectionType(ptStr))
			if err != nil {
				return nil, err
			}
			return toMap(art)
		},
	})
}
