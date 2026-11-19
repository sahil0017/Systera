// Package agent executes leased tasks: it builds the AgentTaskEnvelope, drives the
// specialist agent through the Model Gateway (and Tool Gateway), checkpoints progress
// and reports an AgentResult back to the scheduler.
package agent

import (
	"time"

	"github.com/systera/control-plane/internal/runtime"
)

// TaskBudget mirrors AgentTaskEnvelope.budget.
type TaskBudget struct {
	MaxTokens    int     `json:"max_tokens"`
	MaxCostUSD   float64 `json:"max_cost_usd"`
	MaxToolCalls int     `json:"max_tool_calls"`
}

// TaskEnvelope is contracts/agents/agent-task-envelope.v1.json.
type TaskEnvelope struct {
	SchemaVersion       string         `json:"schema_version"`
	TaskID              string         `json:"task_id"`
	RunID               string         `json:"run_id"`
	TenantID            string         `json:"tenant_id"`
	ProjectID           string         `json:"project_id"`
	AgentType           string         `json:"agent_type"`
	Objective           string         `json:"objective"`
	ContextRefs         []string       `json:"context_refs"`
	GraphVersion        int            `json:"graph_version"`
	Constraints         map[string]any `json:"constraints"`
	AcceptanceCriteria  []string       `json:"acceptance_criteria"`
	EvidencePolicy      map[string]any `json:"evidence_policy"`
	GrantedCapabilities []string       `json:"granted_capabilities"`
	Budget              TaskBudget     `json:"budget"`
	Deadline            time.Time      `json:"deadline"`
	CorrelationID       string         `json:"correlation_id"`
}

// BuildEnvelope derives the envelope for a lease using the project's live graph_version
// (defaulting to 0 when the project does not yet have an architecture model).
func BuildEnvelope(l *runtime.Lease, graphVersion ...int) TaskEnvelope {
	gv := 0
	if len(graphVersion) > 0 && graphVersion[0] >= 0 {
		gv = graphVersion[0]
	}
	refs := []string{"run:" + l.Run.RunID}
	for _, d := range l.Dependencies {
		refs = append(refs, "task:"+d.TaskID)
	}
	constraints := l.Run.Constraints
	if constraints == nil {
		constraints = map[string]any{}
	}
	criteria := l.Run.AcceptanceCriteria
	if criteria == nil {
		criteria = []string{}
	}
	return TaskEnvelope{
		SchemaVersion:       "1.0",
		TaskID:              l.Task.ID,
		RunID:               l.Run.RunID,
		TenantID:            l.Run.TenantID,
		ProjectID:           l.Run.ProjectID,
		AgentType:           l.Task.AgentType,
		Objective:           l.Task.Objective,
		ContextRefs:         refs,
		GraphVersion:        gv,
		Constraints:         constraints,
		AcceptanceCriteria:  criteria,
		EvidencePolicy:      map[string]any{"label_assumptions": true},
		GrantedCapabilities: l.Task.GrantedCapabilities,
		Budget:              TaskBudget{MaxTokens: l.Task.MaxTokens, MaxCostUSD: l.Task.BudgetUSD, MaxToolCalls: l.Task.MaxToolCalls},
		Deadline:            l.Deadline.UTC(),
		CorrelationID:       l.Run.CorrelationID,
	}
}

// Evidence supports a claim in a result.
type Evidence struct {
	Claim  string `json:"claim"`
	Source string `json:"source,omitempty"`
}

// ResultError describes a FAILED result.
type ResultError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Result is contracts/agents/agent-result.v1.json.
type Result struct {
	SchemaVersion string         `json:"schema_version"`
	TaskID        string         `json:"task_id"`
	RunID         string         `json:"run_id"`
	AgentType     string         `json:"agent_type"`
	Attempt       int            `json:"attempt"`
	Status        string         `json:"status"`
	Summary       string         `json:"summary"`
	Output        map[string]any `json:"output"`
	Confidence    *float64       `json:"confidence,omitempty"`
	Evidence      []Evidence     `json:"evidence,omitempty"`
	OpenQuestions []string       `json:"open_questions,omitempty"`
	Error         *ResultError   `json:"error,omitempty"`
	Model         string         `json:"model,omitempty"`
	Usage         runtime.Usage  `json:"usage"`
	CompletedAt   time.Time      `json:"completed_at"`
}

const (
	ResultSucceeded = "SUCCEEDED"
	ResultFailed    = "FAILED"
)
