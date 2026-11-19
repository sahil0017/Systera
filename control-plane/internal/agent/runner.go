package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/systera/control-plane/internal/models"
	"github.com/systera/control-plane/internal/runtime"
	"github.com/systera/control-plane/internal/scheduler"
	"github.com/systera/control-plane/internal/tools"
)

// Queue is the scheduler surface a worker uses (implemented by *scheduler.Store).
type Queue interface {
	Claim(ctx context.Context, req scheduler.ClaimRequest) (*runtime.Lease, error)
	Heartbeat(ctx context.Context, lease *runtime.Lease, ttl time.Duration) error
	Checkpoint(ctx context.Context, lease *runtime.Lease, state map[string]any) error
	Complete(ctx context.Context, lease *runtime.Lease, result map[string]any, usage runtime.Usage) error
	Fail(ctx context.Context, lease *runtime.Lease, f runtime.Failure) error
}

// Config tunes a Runner.
type Config struct {
	WorkerID          string
	AgentTypes        []string
	LeaseTTL          time.Duration // default 60s
	HeartbeatInterval time.Duration // default LeaseTTL/3
	PollInterval      time.Duration // idle wait between claims, default 1s
	Temperature       *float64
}

// GraphVersionResolver returns the current canonical graph version for a project.
type GraphVersionResolver interface {
	CurrentGraphVersion(ctx context.Context, tenantID, projectID string) int
}

// TaskSubmitter enqueues corrective follow-up tasks in the DAG scheduler (Checklist 8.3).
type TaskSubmitter interface {
	SubmitPlan(ctx context.Context, tenantID, runID string, specs []runtime.TaskSpec) ([]runtime.Task, error)
}

// Runner is the specialist agent worker loop.
type Runner struct {
	cfg           Config
	queue         Queue
	catalog       Catalog
	models        *models.Gateway
	tools         *tools.Gateway
	graphVersions GraphVersionResolver
	taskSubmitter TaskSubmitter
	advisor       KnowledgeAdvisor
	logger        *slog.Logger
	now           func() time.Time
}

// SetGraphVersionResolver wires live graph_version lookup into AgentTaskEnvelope construction.
func (r *Runner) SetGraphVersionResolver(g GraphVersionResolver) {
	r.graphVersions = g
}

// SetTaskSubmitter wires automatic corrective task creation on unresolved patch conflicts.
func (r *Runner) SetTaskSubmitter(s TaskSubmitter) {
	r.taskSubmitter = s
}

func NewRunner(cfg Config, q Queue, c Catalog, m *models.Gateway, t *tools.Gateway, logger *slog.Logger) *Runner {
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = time.Minute
	}
	if cfg.HeartbeatInterval <= 0 || cfg.HeartbeatInterval >= cfg.LeaseTTL {
		cfg.HeartbeatInterval = cfg.LeaseTTL / 3
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	return &Runner{cfg: cfg, queue: q, catalog: c, models: m, tools: t, logger: logger, now: time.Now}
}

// Run claims and executes tasks until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	r.logger.Info("agent worker started", "worker_id", r.cfg.WorkerID, "agent_types", r.cfg.AgentTypes)
	for ctx.Err() == nil {
		claimed, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			r.logger.Error("worker iteration failed", "error", err)
		}
		if claimed && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(r.cfg.PollInterval):
		}
	}
	return nil
}

// RunOnce claims at most one task and processes it to completion. It reports whether a
// task was claimed.
func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	lease, err := r.queue.Claim(ctx, scheduler.ClaimRequest{WorkerID: r.cfg.WorkerID, AgentTypes: r.cfg.AgentTypes, LeaseTTL: r.cfg.LeaseTTL})
	if err != nil || lease == nil {
		return false, err
	}
	return true, r.process(ctx, lease)
}

// errLeaseLost aborts an attempt whose lease was taken away.
var errLeaseLost = errors.New("lease lost during execution")

func (r *Runner) process(ctx context.Context, lease *runtime.Lease) error {
	log := r.logger.With("task_id", lease.Task.ID, "task_key", lease.Task.Key, "run_id", lease.Run.RunID,
		"agent_type", lease.Task.AgentType, "attempt", lease.AttemptNumber)
	log.Info("task leased")

	attemptCtx, cancel := context.WithDeadline(withLease(ctx, lease), lease.Deadline)
	defer cancel()

	var lost atomic.Bool
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		t := time.NewTicker(r.cfg.HeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-attemptCtx.Done():
				return
			case <-t.C:
				if err := r.queue.Heartbeat(attemptCtx, lease, r.cfg.LeaseTTL); err != nil {
					if errors.Is(err, scheduler.ErrLeaseLost) {
						lost.Store(true)
						cancel()
						return
					}
					if attemptCtx.Err() == nil {
						log.Warn("heartbeat failed; will retry", "error", err)
					}
				}
			}
		}
	}()

	result, usage, execErr := r.execute(attemptCtx, lease)
	cancel()
	<-hbDone

	// Report with a fresh context: the attempt context may be past its deadline, and a
	// shutting-down worker should still hand the task back promptly.
	reportCtx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer rcancel()

	if lost.Load() || errors.Is(execErr, errLeaseLost) {
		log.Warn("lease lost; abandoning attempt without reporting")
		return nil
	}
	if execErr != nil {
		retryable := isRetryable(execErr)
		code := failureCode(execErr)
		switch {
		case ctx.Err() != nil:
			execErr, retryable, code = fmt.Errorf("worker shutting down: %w", execErr), true, "worker_shutdown"
		case errors.Is(attemptCtx.Err(), context.DeadlineExceeded) && errors.Is(execErr, context.DeadlineExceeded):
			execErr, retryable, code = fmt.Errorf("attempt exceeded its %ds timeout", lease.Task.TimeoutSeconds), true, "attempt_timeout"
		}
		log.Warn("task attempt failed", "error", execErr, "retryable", retryable, "code", code)
		failed, err := toMap(r.failedResult(lease, result, usage, code, execErr))
		if err != nil {
			return err
		}
		err = r.queue.Fail(reportCtx, lease, runtime.Failure{Message: execErr.Error(), Retryable: retryable, Usage: usage, Result: failed})
		if errors.Is(err, scheduler.ErrLeaseLost) {
			return nil
		}
		return err
	}

	payload, err := toMap(result)
	if err != nil {
		return err
	}
	if err := r.queue.Complete(reportCtx, lease, payload, usage); err != nil {
		if errors.Is(err, scheduler.ErrLeaseLost) {
			log.Warn("lease lost before completion was recorded")
			return nil
		}
		return err
	}
	log.Info("task succeeded", "cost_usd", usage.CostUSD, "model_calls", usage.ModelCalls, "tool_calls", usage.ToolCalls)
	return nil
}

// nonRetryableError marks failures that will not succeed on retry (agent-declared
// failure, policy violations, budget exhaustion).
type nonRetryableError struct{ err error }

func (e nonRetryableError) Error() string { return e.err.Error() }
func (e nonRetryableError) Unwrap() error { return e.err }

func isRetryable(err error) bool {
	var nr nonRetryableError
	if errors.As(err, &nr) {
		return false
	}
	return models.IsRetryable(err)
}

// reply is the JSON protocol the agent model speaks. Confidence, evidence and
// open_questions are also accepted beside "result" (a common small-model slip) and
// hoisted into it.
type reply struct {
	Action        string     `json:"action"`
	Confidence    *float64   `json:"confidence"`
	Evidence      []Evidence `json:"evidence"`
	OpenQuestions []string   `json:"open_questions"`
	ToolCalls     []struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"tool_calls"`
	Result *struct {
		Status        string         `json:"status"`
		Summary       string         `json:"summary"`
		Output        map[string]any `json:"output"`
		Confidence    *float64       `json:"confidence"`
		Evidence      []Evidence     `json:"evidence"`
		OpenQuestions []string       `json:"open_questions"`
		Error         *ResultError   `json:"error"`
	} `json:"result"`
}

func protocolPrompt(available []tools.Descriptor) string {
	var b strings.Builder
	b.WriteString(`

## Response protocol
Respond with a single JSON object and nothing else.
To finish, respond:
{"action":"final","result":{"status":"SUCCEEDED","summary":"<one paragraph>","output":{...structured findings...},"confidence":0.0-1.0,"evidence":[{"claim":"...","source":"..."}],"open_questions":["..."]}}
If the task cannot be completed from the information available, respond with status "FAILED" and
"error":{"code":"<short_code>","message":"<why>"}.`)
	if len(available) > 0 {
		b.WriteString(`
To call tools first, respond:
{"action":"tool_calls","tool_calls":[{"name":"<tool>","arguments":{...}}]}
Tool results will be sent back to you. Available tools:
`)
		for _, d := range available {
			schema, _ := json.Marshal(d.InputSchema)
			fmt.Fprintf(&b, "- %s: %s Arguments schema: %s\n", d.Name, d.Description, schema)
		}
	} else {
		b.WriteString("\nNo tools are available for this task.")
	}
	return b.String()
}

// execute drives the model/tool loop for one attempt.
func (r *Runner) execute(ctx context.Context, lease *runtime.Lease) (Result, runtime.Usage, error) {
	meter := &models.Meter{}
	var session *tools.Session
	usage := func() runtime.Usage {
		calls, p, c, cost := meter.Snapshot()
		u := runtime.Usage{ModelCalls: calls, PromptTokens: p, CompletionTokens: c, CostUSD: cost}
		if session != nil {
			u.ToolCalls = session.Calls()
		}
		return u
	}

	def, err := r.catalog.Get(ctx, lease.Task.AgentType)
	if err != nil {
		return Result{}, usage(), nonRetryableError{err}
	}
	model := def.Model
	if model == "" {
		model = r.models.DefaultModel()
	}
	session = r.tools.NewSession(
		tools.Invocation{TenantID: lease.Run.TenantID, RunID: lease.Run.RunID, TaskID: lease.Task.ID, AgentType: def.AgentType},
		tools.Policy{AllowedTools: def.AllowedTools, GrantedCapabilities: lease.Task.GrantedCapabilities, MaxCalls: lease.Task.MaxToolCalls},
	)

	gv := 0
	if r.graphVersions != nil {
		gv = r.graphVersions.CurrentGraphVersion(ctx, lease.Run.TenantID, lease.Run.ProjectID)
	}
	userMsg, err := buildUserMessage(lease, gv)
	if err != nil {
		return Result{}, usage(), err
	}
	userMsg = insertBeforeEnvelope(userMsg, r.knowledgeSection(ctx, lease))
	messages := []models.Message{
		{Role: "system", Content: def.SystemPrompt + protocolPrompt(session.Available())},
		{Role: "user", Content: userMsg},
	}

	tokensLeft := lease.Task.MaxTokens
	if def.MaxTokens > 0 {
		tokensLeft = min(tokensLeft, def.MaxTokens)
	}
	maxModelCalls := lease.Task.MaxToolCalls + 2 // tool rounds + final answer + one format repair
	repaired := false
	var lastAction string
	unresolvedConflict := false
	var conflictVersion any

	for call := 0; call < maxModelCalls; call++ {
		_, _, _, spent := meter.Snapshot()
		resp, err := r.models.Complete(ctx, models.Request{
			Model: model, Messages: messages, MaxTokens: tokensLeft, Temperature: r.cfg.Temperature, JSON: true,
		}, models.Budget{MaxTokens: tokensLeft, MaxCostUSD: lease.Task.BudgetUSD - spent})
		if err != nil {
			if errors.Is(err, models.ErrBudgetExceeded) || errors.Is(err, models.ErrUnpricedModel) {
				return Result{}, usage(), nonRetryableError{err}
			}
			return Result{}, usage(), err
		}
		meter.Add(resp)
		tokensLeft -= resp.Usage.CompletionTokens

		rep, perr := parseReply(resp.Content)
		if perr == nil {
			lastAction = rep.Action
		}
		if err := r.queue.Checkpoint(ctx, lease, map[string]any{
			"model_calls": call + 1, "tool_calls": session.Calls(), "last_action": lastAction,
			"finish_reason": resp.FinishReason, "cost_usd": usage().CostUSD,
		}); err != nil {
			if errors.Is(err, scheduler.ErrLeaseLost) {
				return Result{}, usage(), errLeaseLost
			}
			return Result{}, usage(), err
		}

		if perr != nil {
			r.logger.Warn("agent reply rejected", "task_id", lease.Task.ID, "error", perr, "reply_excerpt", excerpt(resp.Content, 400))
			if repaired {
				return Result{}, usage(), fmt.Errorf("%w twice: %v", errInvalidResponse, perr)
			}
			repaired = true
			messages = append(messages,
				models.Message{Role: "assistant", Content: resp.Content},
				models.Message{Role: "user", Content: "Your previous reply was invalid: " + perr.Error() + ". Reply again with a single JSON object that follows the response protocol exactly."})
			continue
		}

		switch rep.Action {
		case "final":
			res := Result{
				SchemaVersion: "1.0", TaskID: lease.Task.ID, RunID: lease.Run.RunID, AgentType: lease.Task.AgentType,
				Attempt: lease.AttemptNumber, Status: rep.Result.Status, Summary: rep.Result.Summary, Output: rep.Result.Output,
				Confidence: rep.Result.Confidence, Evidence: rep.Result.Evidence, OpenQuestions: rep.Result.OpenQuestions,
				Error: rep.Result.Error, Model: model, Usage: usage(), CompletedAt: r.now().UTC(),
			}
			if res.Status == ResultFailed {
				err := errAgentFailed
				if res.Error != nil && res.Error.Message != "" {
					err = fmt.Errorf("%w: %s: %s", errAgentFailed, res.Error.Code, res.Error.Message)
				}
				return res, res.Usage, nonRetryableError{err}
			}
			// Seam 3 (Checklist 8.3): Auto-enqueue a corrective task if a GraphPatch conflict remained unresolved.
			if unresolvedConflict && r.taskSubmitter != nil {
				_, _ = r.taskSubmitter.SubmitPlan(ctx, lease.Task.TenantID, lease.Run.RunID, []runtime.TaskSpec{{
					Key:                 fmt.Sprintf("resolve-conflict-%s-%d", lease.Task.Key, lease.AttemptNumber),
					AgentType:           lease.Task.AgentType,
					Objective:           fmt.Sprintf("Re-base and resubmit GraphPatch onto current_graph_version %v to resolve conflict from task %s.", conflictVersion, lease.Task.Key),
					GrantedCapabilities: lease.Task.GrantedCapabilities,
					BudgetUSD:           lease.Task.BudgetUSD * 0.5,
					MaxTokens:           lease.Task.MaxTokens,
					MaxToolCalls:        lease.Task.MaxToolCalls,
				}})
			}
			return res, res.Usage, nil

		case "tool_calls":
			results := make([]map[string]any, 0, len(rep.ToolCalls))
			for _, tc := range rep.ToolCalls {
				out, err := session.Execute(ctx, tc.Name, tc.Arguments)
				entry := map[string]any{"name": tc.Name}
				if err != nil {
					entry["error"] = err.Error()
				} else {
					entry["output"] = out
					if tc.Name == "graph.propose_patch" {
						if st, _ := out["status"].(string); st == "CONFLICT" {
							unresolvedConflict = true
							conflictVersion = out["current_graph_version"]
						} else if st == "ACCEPTED" {
							unresolvedConflict = false
						}
					}
				}
				results = append(results, entry)
			}
			toolMsg := map[string]any{"tool_results": results}
			if session.Calls() >= lease.Task.MaxToolCalls || call+2 >= maxModelCalls {
				toolMsg["instruction"] = `No tool calls remain. Your next reply MUST be {"action":"final","result":{...}} using what you have.`
			}
			resultsJSON, _ := json.Marshal(toolMsg)
			messages = append(messages,
				models.Message{Role: "assistant", Content: resp.Content},
				models.Message{Role: "user", Content: string(resultsJSON)})
		}
	}
	return Result{}, usage(), nonRetryableError{fmt.Errorf("%w (%d)", errMaxModelCalls, maxModelCalls)}
}

func parseReply(content string) (reply, error) {
	content = strings.TrimSpace(content)
	// Tolerate a fenced code block around the JSON.
	if strings.HasPrefix(content, "```") {
		content = strings.TrimPrefix(content, "```json")
		content = strings.TrimPrefix(content, "```")
		content = strings.TrimSuffix(strings.TrimSpace(content), "```")
	}
	var rep reply
	if err := json.Unmarshal([]byte(content), &rep); err != nil {
		return reply{}, fmt.Errorf("not a JSON object: %v", err)
	}
	switch rep.Action {
	case "final":
		if rep.Result == nil {
			return reply{}, errors.New(`"final" requires "result"`)
		}
		if rep.Result.Status != ResultSucceeded && rep.Result.Status != ResultFailed {
			return reply{}, errors.New(`result.status must be "SUCCEEDED" or "FAILED"`)
		}
		if rep.Result.Confidence == nil {
			rep.Result.Confidence = rep.Confidence
		}
		if rep.Result.Evidence == nil {
			rep.Result.Evidence = rep.Evidence
		}
		if rep.Result.OpenQuestions == nil {
			rep.Result.OpenQuestions = rep.OpenQuestions
		}
		if rep.Result.Status == ResultFailed && strings.TrimSpace(rep.Result.Summary) == "" {
			if rep.Result.Error == nil || strings.TrimSpace(rep.Result.Error.Message) == "" {
				return reply{}, errors.New(`a FAILED result requires "summary" or "error.message"`)
			}
			rep.Result.Summary = rep.Result.Error.Message
		}
		if strings.TrimSpace(rep.Result.Summary) == "" {
			return reply{}, errors.New("result.summary is required")
		}
		if rep.Result.Output == nil {
			rep.Result.Output = map[string]any{} // the summary carries the content
		}
		if c := rep.Result.Confidence; c != nil && (*c < 0 || *c > 1) {
			return reply{}, errors.New("result.confidence must be within 0..1")
		}
	case "tool_calls":
		if len(rep.ToolCalls) == 0 {
			return reply{}, errors.New(`"tool_calls" requires at least one call`)
		}
	default:
		return reply{}, fmt.Errorf(`action must be "final" or "tool_calls", got %q`, rep.Action)
	}
	return rep, nil
}

// buildUserMessage presents the task as labelled sections (easy for any model to follow)
// followed by the full AgentTaskEnvelope as a fenced JSON block.
func buildUserMessage(l *runtime.Lease, graphVersion ...int) (string, error) {
	js := func(v any) string {
		b, _ := json.MarshalIndent(v, "", "  ")
		return string(b)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Your task\n%s\n\n", l.Task.Objective)
	fmt.Fprintf(&b, "# Overall design objective\n%s\n\n", l.Run.Objective)
	if len(l.Run.Constraints) > 0 {
		fmt.Fprintf(&b, "# Constraints\n%s\n\n", js(l.Run.Constraints))
	}
	if len(l.Run.AcceptanceCriteria) > 0 {
		b.WriteString("# Acceptance criteria\n")
		for _, c := range l.Run.AcceptanceCriteria {
			fmt.Fprintf(&b, "- %s\n", c)
		}
		b.WriteString("\n")
	}
	if len(l.Task.Input) > 0 {
		fmt.Fprintf(&b, "# Task input\n%s\n\n", js(l.Task.Input))
	}
	if len(l.Dependencies) > 0 {
		fmt.Fprintf(&b, "# Results of upstream tasks\n%s\n\n", js(l.Dependencies))
	}
	if l.LastCheckpoint != nil {
		fmt.Fprintf(&b, "# Note\nThis is attempt %d; the previous attempt stopped at: %s\n\n", l.AttemptNumber, js(l.LastCheckpoint.State))
	}
	env, err := json.Marshal(BuildEnvelope(l, graphVersion...))
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "# Task envelope (AgentTaskEnvelope v1)\n```json\n%s\n```\n", env)
	return b.String(), nil
}

var (
	errInvalidResponse  = errors.New("agent returned an invalid response")
	errMaxModelCalls    = errors.New("agent did not finish within its model call limit")
	errAgentFailed      = errors.New("agent reported failure")
	errorCodeFromResult = "agent_declared_failure"
)

// failureCode classifies an attempt error for AgentResult.error.code.
func failureCode(err error) string {
	var pe *models.ProviderError
	switch {
	case errors.Is(err, errAgentFailed):
		return errorCodeFromResult
	case errors.Is(err, models.ErrBudgetExceeded):
		return "budget_exceeded"
	case errors.Is(err, models.ErrUnpricedModel):
		return "unpriced_model"
	case errors.Is(err, ErrAgentNotFound):
		return "agent_not_found"
	case errors.Is(err, errInvalidResponse):
		return "invalid_agent_response"
	case errors.Is(err, errMaxModelCalls):
		return "model_call_limit"
	case errors.As(err, &pe):
		return "provider_error"
	case errors.Is(err, context.DeadlineExceeded):
		return "attempt_timeout"
	default:
		return "execution_error"
	}
}

// failedResult builds the FAILED AgentResult for an attempt. An agent-declared failure
// keeps the agent's own result (summary, output, evidence, open questions).
func (r *Runner) failedResult(l *runtime.Lease, res Result, u runtime.Usage, code string, err error) Result {
	if res.Status == ResultFailed {
		if res.Error == nil {
			res.Error = &ResultError{Code: code, Message: err.Error()}
		}
		res.Usage = u
		return res
	}
	return Result{
		SchemaVersion: "1.0", TaskID: l.Task.ID, RunID: l.Run.RunID, AgentType: l.Task.AgentType,
		Attempt: l.AttemptNumber, Status: ResultFailed, Summary: err.Error(), Output: map[string]any{},
		Error: &ResultError{Code: code, Message: err.Error()}, Usage: u, CompletedAt: r.now().UTC(),
	}
}

func excerpt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(b, &m)
}
