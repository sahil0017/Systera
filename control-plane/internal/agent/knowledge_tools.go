package agent

import (
	"context"
	"encoding/json"

	"github.com/systera/control-plane/internal/evidence"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/knowledge"
	"github.com/systera/control-plane/internal/memory"
	"github.com/systera/control-plane/internal/tools"
)

// Knowledge & Memory Tool Gateway capabilities (Checklist 1.6, 1.7).
const (
	CapKnowledgeRead = "knowledge.read"
	CapMemoryRead    = "memory.read"
	CapMemoryWrite   = "memory.write"
)

// RegisterKnowledgeTools registers knowledge.search (citing ledger evidence) and
// memory.recall / memory.record tools gated by the Tool Gateway.
func RegisterKnowledgeTools(
	reg *tools.Registry,
	kStore *knowledge.Store,
	mStore *memory.Store,
	graphSvc *graph.Service,
	evStore *evidence.Store,
) error {
	if kStore != nil {
		if err := reg.Register(tools.Tool{
			Name:        "knowledge.search",
			Description: "Hybrid search over versioned engineering knowledge (vector + keyword + recency + primary-source preference). Results are untrusted data with provenance and optional ledger evidence citations.",
			Capability:  CapKnowledgeRead,
			InputSchema: tools.MustSchema(`{"type":"object","required":["query"],"properties":{"query":{"type":"string","minLength":1},"source_types":{"type":"array","items":{"type":"string"}},"maturities":{"type":"array","items":{"type":"string"}},"primary_only":{"type":"boolean"},"limit":{"type":"integer","minimum":1,"maximum":20},"cite_as_evidence":{"type":"boolean"}}}`),
			Handler: func(ctx context.Context, _ tools.Invocation, args map[string]any) (map[string]any, error) {
				l, err := leaseFrom(ctx)
				if err != nil {
					return nil, err
				}
				raw, _ := json.Marshal(args)
				var q knowledge.SearchQuery
				if err := json.Unmarshal(raw, &q); err != nil {
					return nil, err
				}
				q.ProjectID = l.Run.ProjectID
				results, err := kStore.Search(ctx, l.Task.TenantID, q)
				if err != nil {
					return nil, err
				}

				cite, _ := args["cite_as_evidence"].(bool)
				items := make([]map[string]any, 0, len(results))
				var modelID string
				if cite && evStore != nil && graphSvc != nil {
					if m, mErr := graphSvc.ModelForProject(ctx, l.Task.TenantID, l.Run.ProjectID); mErr == nil {
						modelID = m.ID
					}
				}
				for _, r := range results {
					entry, _ := toMap(r)
					if cite && modelID != "" && evStore != nil {
						actor := evidence.Actor{Type: "AGENT", ID: l.Task.AgentType, RunID: l.Run.RunID, AgentTaskID: l.Task.ID}
						ev, _, eErr := evStore.CaptureEvidence(ctx, l.Task.TenantID, modelID, evidence.EvidenceInput{
							SourceRef:  r.ProvenanceRef,
							SourceType: "RETRIEVED_KNOWLEDGE",
							Content:    r.Content,
							Excerpt:    r.Title,
							TrustLevel: trustForBand(r.ConfidenceBand),
						}, actor)
						if eErr == nil {
							entry["evidence_ref"] = ev.Ref
							entry["evidence_id"] = ev.ID
						}
					}
					items = append(items, entry)
				}
				return map[string]any{"results": items}, nil
			},
		}); err != nil {
			return err
		}
	}

	if mStore != nil {
		if err := reg.Register(tools.Tool{
			Name:        "memory.recall",
			Description: "Retrieves relevant, distilled, tenant- and project-scoped memory records (never raw transcripts).",
			Capability:  CapMemoryRead,
			InputSchema: tools.MustSchema(`{"type":"object","properties":{"query":{"type":"string"},"memory_types":{"type":"array","items":{"type":"string"}},"limit":{"type":"integer","minimum":1,"maximum":20}}}`),
			Handler: func(ctx context.Context, _ tools.Invocation, args map[string]any) (map[string]any, error) {
				l, err := leaseFrom(ctx)
				if err != nil {
					return nil, err
				}
				raw, _ := json.Marshal(args)
				var q memory.RetrieveQuery
				if err := json.Unmarshal(raw, &q); err != nil {
					return nil, err
				}
				q.ProjectID = l.Run.ProjectID
				recs, err := mStore.Retrieve(ctx, l.Task.TenantID, q)
				if err != nil {
					return nil, err
				}
				return map[string]any{"items": recs}, nil
			},
		}); err != nil {
			return err
		}

		if err := reg.Register(tools.Tool{
			Name:        "memory.record",
			Description: "Stores a distilled WORKING, EPISODIC, SEMANTIC, or PROJECT memory with source references.",
			Capability:  CapMemoryWrite,
			InputSchema: tools.MustSchema(`{"type":"object","required":["memory_type","summary"],"properties":{"memory_type":{"type":"string"},"summary":{"type":"string","minLength":1,"maxLength":4000},"source_refs":{"type":"array","items":{"type":"string"}}}}`),
			Handler: func(ctx context.Context, _ tools.Invocation, args map[string]any) (map[string]any, error) {
				l, err := leaseFrom(ctx)
				if err != nil {
					return nil, err
				}
				raw, _ := json.Marshal(args)
				var in memory.RecordInput
				if err := json.Unmarshal(raw, &in); err != nil {
					return nil, err
				}
				in.ProjectID = l.Run.ProjectID
				in.RunID = l.Run.RunID
				rec, err := mStore.Record(ctx, l.Task.TenantID, in)
				if err != nil {
					return nil, err
				}
				return toMap(rec)
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

func trustForBand(band string) string {
	switch band {
	case "HIGH":
		return "HIGH"
	case "LOW":
		return "LOW"
	default:
		return "MEDIUM"
	}
}
