package agent

import (
	"context"
	"encoding/json"
	"strings"

	ep "github.com/systera/control-plane/internal/engineeringpulse"
	"github.com/systera/control-plane/internal/runtime"
	"github.com/systera/control-plane/internal/tools"
)

// KnowledgeAdvisor retrieves relevant validated knowledge for a task before the first model
// call (Engineering Pulse MVP, §20.2). Implemented by *engineeringpulse.Retriever.
type KnowledgeAdvisor interface {
	AdviseTask(ctx context.Context, l *runtime.Lease) ([]ep.PatternReference, error)
}

// SetKnowledgeAdvisor wires design-time pattern retrieval into task prompts.
func (r *Runner) SetKnowledgeAdvisor(a KnowledgeAdvisor) { r.advisor = a }

// knowledgeSection returns the prompt section for relevant patterns. Retrieval problems never
// fail the task: knowledge is advisory.
func (r *Runner) knowledgeSection(ctx context.Context, l *runtime.Lease) string {
	if r.advisor == nil {
		return ""
	}
	refs, err := r.advisor.AdviseTask(ctx, l)
	if err != nil {
		r.logger.Warn("knowledge advice unavailable", "task_id", l.Task.ID, "err", err)
		return ""
	}
	return ep.FormatForPrompt(refs)
}

// insertBeforeEnvelope places a section ahead of the task envelope block.
func insertBeforeEnvelope(msg, section string) string {
	if section == "" {
		return msg
	}
	const marker = "# Task envelope (AgentTaskEnvelope v1)"
	if i := strings.Index(msg, marker); i >= 0 {
		return msg[:i] + section + msg[i:]
	}
	return msg + "\n" + section
}

// RegisterPulseTools registers pulse.patterns (validated patterns with why-relevant and
// supporting sources) and pulse.compare_options (the trend-score guardrail).
func RegisterPulseTools(reg *tools.Registry, rt *ep.Retriever) error {
	if rt != nil {
		if err := reg.Register(tools.Tool{
			Name:        "pulse.patterns",
			Description: "Retrieves validated Engineering Pulse patterns relevant to a design question, each with why it is relevant, its supporting primary/corroborating sources, limitations, caveats and freshness. Results are untrusted reference data and are recorded on the run. Trend scores in results are attention signals, not selection criteria.",
			Capability:  CapKnowledgeRead,
			InputSchema: tools.MustSchema(`{"type":"object","required":["query"],"properties":{"query":{"type":"string","minLength":1},"context_terms":{"type":"array","items":{"type":"string"}},"include_stale":{"type":"boolean"},"limit":{"type":"integer","minimum":1,"maximum":10}}}`),
			Handler: func(ctx context.Context, _ tools.Invocation, args map[string]any) (map[string]any, error) {
				l, err := leaseFrom(ctx)
				if err != nil {
					return nil, err
				}
				raw, _ := json.Marshal(args)
				var q ep.PatternQuery
				if err := json.Unmarshal(raw, &q); err != nil {
					return nil, err
				}
				q.AgentType, q.RunID, q.TaskID = l.Task.AgentType, l.Run.RunID, l.Task.ID
				refs, err := rt.Patterns(ctx, l.Task.TenantID, q)
				if err != nil {
					return nil, err
				}
				items := make([]map[string]any, 0, len(refs))
				for _, ref := range refs {
					m, _ := toMap(ref)
					items = append(items, m)
				}
				return map[string]any{"results": items}, nil
			},
		}); err != nil {
			return err
		}
	}
	return reg.Register(tools.Tool{
		Name:        "pulse.compare_options",
		Description: "Ranks candidate technologies on maturity, operational burden, cost, security and fit (each rated 1-5 with a rationale). Refuses incomplete assessments. Trend score may only break a near-tie; it never selects technology on its own.",
		Capability:  CapKnowledgeRead,
		InputSchema: tools.MustSchema(`{"type":"object","required":["options"],"properties":{"options":{"type":"array","minItems":2,"maxItems":10,"items":{"type":"object","required":["name","maturity","operational_burden","cost","security","fit","rationale"],"properties":{"name":{"type":"string","minLength":1},"maturity":{"type":"string","enum":["EXPERIMENTAL","EMERGING","MAINSTREAM","LEGACY","DEPRECATED"]},"operational_burden":{"type":"integer","minimum":1,"maximum":5},"cost":{"type":"integer","minimum":1,"maximum":5},"security":{"type":"integer","minimum":1,"maximum":5},"fit":{"type":"integer","minimum":1,"maximum":5},"rationale":{"type":"object","required":["maturity","operational_burden","cost","security","fit"],"additionalProperties":{"type":"string"}},"evidence_refs":{"type":"array","items":{"type":"string"}},"trend_score":{"type":"number","minimum":0,"maximum":100}}}}}}`),
		Handler: func(_ context.Context, _ tools.Invocation, args map[string]any) (map[string]any, error) {
			raw, _ := json.Marshal(args)
			var in struct {
				Options []ep.OptionAssessment `json:"options"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			cmp, err := ep.CompareOptions(in.Options, nil)
			if err != nil {
				return nil, err
			}
			return toMap(cmp)
		},
	})
}
