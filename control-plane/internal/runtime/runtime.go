// Package runtime defines the durable agent-task model: task specs submitted as a DAG
// plan, task states, leases, checkpoints and outcomes. It contains no I/O; persistence
// and leasing live in internal/scheduler.
package runtime

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// TaskState is the lifecycle state of an agent task.
type TaskState string

const (
	TaskPending   TaskState = "PENDING"   // waiting on dependencies
	TaskReady     TaskState = "READY"     // claimable (subject to available_at and run budget)
	TaskLeased    TaskState = "LEASED"    // held by a worker under a time-bounded lease
	TaskSucceeded TaskState = "SUCCEEDED" // terminal
	TaskFailed    TaskState = "FAILED"    // terminal: attempts exhausted or non-retryable error
	TaskCancelled TaskState = "CANCELLED" // terminal: run ended or upstream failed
)

func (s TaskState) IsTerminal() bool {
	return s == TaskSucceeded || s == TaskFailed || s == TaskCancelled
}

// Event types emitted to the outbox by the runtime.
const (
	EventTaskReady          = "task.ready"
	EventTaskLeased         = "task.leased"
	EventTaskSucceeded      = "task.succeeded"
	EventTaskFailed         = "task.failed"
	EventTaskRetryScheduled = "task.retry_scheduled"
	EventTaskLeaseExpired   = "task.lease_expired"
	EventRunPlanAccepted    = "run.plan_accepted"
)

// Plan limits.
const (
	MaxTasksPerPlan      = 200
	MaxDependenciesTotal = 2000
	DefaultTaskBudgetUSD = 0.50
	DefaultMaxTokens     = 4096
	DefaultMaxAttempts   = 3
	DefaultTimeout       = 10 * time.Minute
	MaxTaskTimeout       = 2 * time.Hour
	MaxToolCallsPerTask  = 50
)

var (
	ErrInvalidPlan = errors.New("invalid plan")
	taskKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	capPattern     = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
)

// TaskSpec is one node of a submitted plan.
type TaskSpec struct {
	Key                 string         `json:"key"`
	AgentType           string         `json:"agent_type"`
	Objective           string         `json:"objective"`
	Input               map[string]any `json:"input,omitempty"`
	DependsOn           []string       `json:"depends_on,omitempty"`
	GrantedCapabilities []string       `json:"granted_capabilities,omitempty"`
	Priority            int            `json:"priority,omitempty"`
	BudgetUSD           float64        `json:"budget_usd,omitempty"`
	MaxTokens           int            `json:"max_tokens,omitempty"`
	MaxToolCalls        int            `json:"max_tool_calls,omitempty"`
	MaxAttempts         int            `json:"max_attempts,omitempty"`
	TimeoutSeconds      int            `json:"timeout_seconds,omitempty"`
}

// PlanError explains why a plan was rejected.
type PlanError struct{ Reason string }

func (e *PlanError) Error() string { return "invalid plan: " + e.Reason }
func (e *PlanError) Unwrap() error { return ErrInvalidPlan }

func planErr(format string, args ...any) error {
	return &PlanError{Reason: fmt.Sprintf(format, args...)}
}

// ValidatePlan normalizes specs in place (defaults, dedup) and verifies the plan is a
// well-formed DAG: unique keys, known dependencies, no self-edges and no cycles. It
// returns the task keys in a deterministic topological order. maxRunBudget bounds the
// budget of any single task (a task that can never fit the run budget is unrunnable).
func ValidatePlan(specs []TaskSpec, maxRunBudget float64) ([]string, error) {
	if len(specs) == 0 {
		return nil, planErr("plan has no tasks")
	}
	if len(specs) > MaxTasksPerPlan {
		return nil, planErr("plan has %d tasks; the limit is %d", len(specs), MaxTasksPerPlan)
	}

	index := make(map[string]int, len(specs))
	edges := 0
	for i := range specs {
		s := &specs[i]
		s.Key = strings.TrimSpace(s.Key)
		s.Objective = strings.TrimSpace(s.Objective)
		if !taskKeyPattern.MatchString(s.Key) {
			return nil, planErr("task key %q must match %s", s.Key, taskKeyPattern)
		}
		if _, dup := index[s.Key]; dup {
			return nil, planErr("duplicate task key %q", s.Key)
		}
		index[s.Key] = i
		if s.AgentType == "" {
			return nil, planErr("task %q: agent_type is required", s.Key)
		}
		if s.Objective == "" {
			return nil, planErr("task %q: objective is required", s.Key)
		}
		if s.BudgetUSD == 0 {
			s.BudgetUSD = DefaultTaskBudgetUSD
		}
		if s.BudgetUSD < 0 || s.BudgetUSD > maxRunBudget {
			return nil, planErr("task %q: budget_usd %.4f must be within the run budget (%.4f)", s.Key, s.BudgetUSD, maxRunBudget)
		}
		if s.MaxTokens == 0 {
			s.MaxTokens = DefaultMaxTokens
		}
		if s.MaxTokens < 1 || s.MaxTokens > 200000 {
			return nil, planErr("task %q: max_tokens out of range", s.Key)
		}
		if s.MaxToolCalls < 0 || s.MaxToolCalls > MaxToolCallsPerTask {
			return nil, planErr("task %q: max_tool_calls must be 0..%d", s.Key, MaxToolCallsPerTask)
		}
		if s.MaxAttempts == 0 {
			s.MaxAttempts = DefaultMaxAttempts
		}
		if s.MaxAttempts < 1 || s.MaxAttempts > 10 {
			return nil, planErr("task %q: max_attempts must be 1..10", s.Key)
		}
		if s.TimeoutSeconds == 0 {
			s.TimeoutSeconds = int(DefaultTimeout / time.Second)
		}
		if s.TimeoutSeconds < 1 || s.TimeoutSeconds > int(MaxTaskTimeout/time.Second) {
			return nil, planErr("task %q: timeout_seconds must be 1..%d", s.Key, int(MaxTaskTimeout/time.Second))
		}
		for _, c := range s.GrantedCapabilities {
			if !capPattern.MatchString(c) {
				return nil, planErr("task %q: invalid capability %q", s.Key, c)
			}
		}
		s.GrantedCapabilities = dedupSorted(s.GrantedCapabilities)
		s.DependsOn = dedupSorted(s.DependsOn)
		edges += len(s.DependsOn)
		if s.Input == nil {
			s.Input = map[string]any{}
		}
	}
	if edges > MaxDependenciesTotal {
		return nil, planErr("plan has %d dependency edges; the limit is %d", edges, MaxDependenciesTotal)
	}

	// Kahn's algorithm with a sorted frontier for a deterministic order.
	indegree := make(map[string]int, len(specs))
	dependents := make(map[string][]string, len(specs))
	for _, s := range specs {
		indegree[s.Key] += 0
		for _, d := range s.DependsOn {
			if d == s.Key {
				return nil, planErr("task %q depends on itself", s.Key)
			}
			if _, ok := index[d]; !ok {
				return nil, planErr("task %q depends on unknown task %q", s.Key, d)
			}
			indegree[s.Key]++
			dependents[d] = append(dependents[d], s.Key)
		}
	}
	var frontier []string
	for k, n := range indegree {
		if n == 0 {
			frontier = append(frontier, k)
		}
	}
	sort.Strings(frontier)
	order := make([]string, 0, len(specs))
	for len(frontier) > 0 {
		k := frontier[0]
		frontier = frontier[1:]
		order = append(order, k)
		next := dependents[k]
		sort.Strings(next)
		for _, d := range next {
			indegree[d]--
			if indegree[d] == 0 {
				frontier = insertSorted(frontier, d)
			}
		}
	}
	if len(order) != len(specs) {
		var cyclic []string
		for k, n := range indegree {
			if n > 0 {
				cyclic = append(cyclic, k)
			}
		}
		sort.Strings(cyclic)
		return nil, planErr("dependency cycle among tasks %s", strings.Join(cyclic, ", "))
	}
	return order, nil
}

func dedupSorted(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func insertSorted(s []string, v string) []string {
	i := sort.SearchStrings(s, v)
	s = append(s, "")
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

// Task is a persisted agent task.
type Task struct {
	ID                  string         `json:"id"`
	TenantID            string         `json:"tenant_id"`
	RunID               string         `json:"run_id"`
	Key                 string         `json:"key"`
	AgentType           string         `json:"agent_type"`
	Objective           string         `json:"objective"`
	Input               map[string]any `json:"input"`
	DependsOn           []string       `json:"depends_on"`
	GrantedCapabilities []string       `json:"granted_capabilities"`
	State               TaskState      `json:"state"`
	Priority            int            `json:"priority"`
	BudgetUSD           float64        `json:"budget_usd"`
	MaxTokens           int            `json:"max_tokens"`
	MaxToolCalls        int            `json:"max_tool_calls"`
	MaxAttempts         int            `json:"max_attempts"`
	TimeoutSeconds      int            `json:"timeout_seconds"`
	AttemptCount        int            `json:"attempt_count"`
	AvailableAt         time.Time      `json:"available_at"`
	LeaseOwner          string         `json:"lease_owner,omitempty"`
	LeaseExpiresAt      *time.Time     `json:"lease_expires_at,omitempty"`
	SpentUSD            float64        `json:"spent_usd"`
	Result              map[string]any `json:"result,omitempty"`
	LastError           string         `json:"last_error,omitempty"`
	CreatedAt           time.Time      `json:"created_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
	CompletedAt         *time.Time     `json:"completed_at,omitempty"`
}

// RunContext is the run-level context a worker needs to build the agent envelope.
type RunContext struct {
	TenantID           string
	ProjectID          string
	RunID              string
	CorrelationID      string
	Objective          string
	Constraints        map[string]any
	AcceptanceCriteria []string
}

// DependencyResult is the outcome of an upstream task, passed to downstream agents.
type DependencyResult struct {
	TaskID    string         `json:"task_id"`
	Key       string         `json:"key"`
	AgentType string         `json:"agent_type"`
	Result    map[string]any `json:"result"`
}

// Checkpoint is durable intermediate (PROGRESS) or final (RESULT) state of an attempt.
type Checkpoint struct {
	Seq       int64          `json:"seq"`
	AttemptID string         `json:"attempt_id"`
	Kind      string         `json:"kind"`
	State     map[string]any `json:"state"`
	CreatedAt time.Time      `json:"created_at"`
}

const (
	CheckpointProgress = "PROGRESS"
	CheckpointResult   = "RESULT"
)

// Lease grants one worker exclusive, time-bounded execution of a task. Token is the
// fencing token: every heartbeat, checkpoint and completion must present it.
type Lease struct {
	Task          Task
	Token         string
	AttemptNumber int
	WorkerID      string
	ExpiresAt     time.Time
	Deadline      time.Time
	Run           RunContext
	Dependencies  []DependencyResult
	// LastCheckpoint is the most recent checkpoint from any earlier attempt, for resumption.
	LastCheckpoint *Checkpoint
}

// Usage is resource consumption reported for an attempt.
type Usage struct {
	ModelCalls       int     `json:"model_calls"`
	ToolCalls        int     `json:"tool_calls"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// Failure describes a failed attempt.
type Failure struct {
	Message   string
	Retryable bool
	Usage     Usage
	// Result is the attempt's FAILED AgentResult, stored as the task's result_json so the
	// failure is inspectable with the same contract as a success.
	Result map[string]any
}
