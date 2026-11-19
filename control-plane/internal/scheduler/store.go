// Package scheduler persists task DAGs and hands out time-bounded, fenced leases.
//
// Concurrency model:
//   - Workers claim READY tasks with SELECT ... FOR UPDATE SKIP LOCKED, so any number of
//     workers (and scheduler replicas) can run concurrently without double-claiming.
//   - Each lease carries a fencing token (task_attempts.id). Heartbeats, checkpoints and
//     completions are accepted only while the token is current; once a lease is reclaimed,
//     the old holder's writes are rejected with ErrLeaseLost.
//   - Budget is reserved on claim (design_runs.reserved_budget_usd += task.budget_usd) and
//     settled on completion, so concurrent leases can never commit a run beyond its budget.
//   - Lock order is always design_runs row, then agent_tasks rows, which keeps the API,
//     scheduler and workers deadlock-free.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/runs"
	"github.com/systera/control-plane/internal/runtime"
)

var (
	// ErrLeaseLost means the caller's lease token is no longer current (expired and
	// reclaimed, run cancelled, or already completed). The caller must stop work.
	ErrLeaseLost = errors.New("task lease lost")
	// ErrRunNotPlannable means a plan was submitted for a run that is not in CREATED.
	ErrRunNotPlannable = errors.New("run already has a plan or is no longer active")
	// ErrUnknownAgent means a plan references an agent type that is not registered/active.
	ErrUnknownAgent = errors.New("unknown or disabled agent type")
)

const (
	FailureReasonBudgetExceeded  = "budget_exceeded"
	FailureReasonBudgetExhausted = "budget_exhausted"
	maxClaimCandidates           = 5
	maxRetryBackoff              = time.Minute
)

// Store is the PostgreSQL-backed task scheduler.
type Store struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
	// now is used only to compute backoffs for events; all lease arithmetic uses DB time.
	now func() time.Time
}

func NewStore(pool *pgxpool.Pool, logger *slog.Logger) *Store {
	return &Store{pool: pool, logger: logger, now: time.Now}
}

func taskCols(a string) string {
	return a + `.id::text, ` + a + `.tenant_id::text, ` + a + `.run_id::text, ` + a + `.task_key, ` + a + `.agent_type, ` +
		a + `.objective, ` + a + `.input_json, ` + a + `.granted_capabilities, ` + a + `.state, ` + a + `.priority, ` +
		a + `.budget_usd::float8, ` + a + `.max_tokens, ` + a + `.max_tool_calls, ` + a + `.max_attempts, ` +
		a + `.timeout_seconds, ` + a + `.attempt_count, ` + a + `.available_at, COALESCE(` + a + `.lease_owner, ''), ` +
		a + `.lease_expires_at, ` + a + `.spent_usd::float8, ` + a + `.result_json, COALESCE(` + a + `.last_error, ''), ` +
		a + `.created_at, ` + a + `.updated_at, ` + a + `.completed_at, ` +
		`ARRAY(SELECT p.task_key FROM task_dependencies d JOIN agent_tasks p ON p.id = d.depends_on_task_id WHERE d.task_id = ` + a + `.id ORDER BY p.task_key)`
}

func scanTask(row pgx.Row) (runtime.Task, error) {
	var t runtime.Task
	var input, result []byte
	err := row.Scan(&t.ID, &t.TenantID, &t.RunID, &t.Key, &t.AgentType, &t.Objective, &input, &t.GrantedCapabilities,
		&t.State, &t.Priority, &t.BudgetUSD, &t.MaxTokens, &t.MaxToolCalls, &t.MaxAttempts, &t.TimeoutSeconds,
		&t.AttemptCount, &t.AvailableAt, &t.LeaseOwner, &t.LeaseExpiresAt, &t.SpentUSD, &result, &t.LastError,
		&t.CreatedAt, &t.UpdatedAt, &t.CompletedAt, &t.DependsOn)
	if err != nil {
		return runtime.Task{}, err
	}
	if err := json.Unmarshal(input, &t.Input); err != nil {
		return runtime.Task{}, fmt.Errorf("decode task input: %w", err)
	}
	if len(result) > 0 {
		if err := json.Unmarshal(result, &t.Result); err != nil {
			return runtime.Task{}, fmt.Errorf("decode task result: %w", err)
		}
	}
	if t.GrantedCapabilities == nil {
		t.GrantedCapabilities = []string{}
	}
	if t.DependsOn == nil {
		t.DependsOn = []string{}
	}
	t.AvailableAt, t.CreatedAt, t.UpdatedAt = t.AvailableAt.UTC(), t.CreatedAt.UTC(), t.UpdatedAt.UTC()
	return t, nil
}

func taskEvent(run runs.DesignRun, t runtime.Task, eventType string, extra map[string]any) outbox.EventEnvelope {
	payload := map[string]any{
		"task_id":    t.ID,
		"task_key":   t.Key,
		"agent_type": t.AgentType,
		"task_state": string(t.State),
		"attempt":    t.AttemptCount,
	}
	for k, v := range extra {
		payload[k] = v
	}
	return outbox.EventEnvelope{
		EventType:     eventType,
		SchemaVersion: "1.0",
		TenantID:      run.TenantID,
		ProjectID:     run.ProjectID,
		RunID:         run.ID,
		CorrelationID: run.CorrelationID,
		Payload:       payload,
	}
}

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

// SubmitPlan validates and persists a task DAG for a CREATED run and starts it. The run
// walks CREATED -> PLANNING -> PLAN_VALIDATING -> READY -> RUNNING in the same
// transaction, with every transition and every root task.ready event recorded.
func (s *Store) SubmitPlan(ctx context.Context, tenantID, runID string, specs []runtime.TaskSpec) ([]runtime.Task, error) {
	if !ids.Valid(runID) {
		return nil, runs.ErrNotFound
	}
	var tasks []runtime.Task
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		run, err := runs.LockTx(ctx, tx, runID)
		if err != nil {
			return err
		}
		if run.State != runs.StateCreated {
			return ErrRunNotPlannable
		}
		order, err := runtime.ValidatePlan(specs, run.MaxBudgetUSD)
		if err != nil {
			return err
		}
		if err := s.checkAgents(ctx, tx, specs); err != nil {
			return err
		}

		for _, st := range []runs.State{runs.StatePlanning, runs.StatePlanValidating, runs.StateReady} {
			if run, err = runs.TransitionTx(ctx, tx, runID, st, ""); err != nil {
				return err
			}
		}

		byKey := make(map[string]runtime.TaskSpec, len(specs))
		for _, sp := range specs {
			byKey[sp.Key] = sp
		}
		idByKey := make(map[string]string, len(specs))
		var roots []string
		for _, key := range order {
			sp := byKey[key]
			state := runtime.TaskPending
			if len(sp.DependsOn) == 0 {
				state = runtime.TaskReady
				roots = append(roots, key)
			}
			input, err := json.Marshal(sp.Input)
			if err != nil {
				return fmt.Errorf("encode task input: %w", err)
			}
			var id string
			if err := tx.QueryRow(ctx, `
				INSERT INTO agent_tasks (tenant_id, run_id, task_key, agent_type, objective, input_json, granted_capabilities,
				                         state, priority, budget_usd, max_tokens, max_tool_calls, max_attempts, timeout_seconds)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
				RETURNING id::text`,
				run.TenantID, run.ID, sp.Key, sp.AgentType, sp.Objective, input, sp.GrantedCapabilities,
				state, sp.Priority, sp.BudgetUSD, sp.MaxTokens, sp.MaxToolCalls, sp.MaxAttempts, sp.TimeoutSeconds,
			).Scan(&id); err != nil {
				return fmt.Errorf("insert task %q: %w", key, err)
			}
			idByKey[key] = id
			for _, dep := range sp.DependsOn {
				if _, err := tx.Exec(ctx, `
					INSERT INTO task_dependencies (tenant_id, run_id, task_id, depends_on_task_id) VALUES ($1, $2, $3, $4)`,
					run.TenantID, run.ID, id, idByKey[dep]); err != nil {
					return fmt.Errorf("insert dependency %s -> %s: %w", key, dep, err)
				}
			}
		}

		if tasks, err = listTasksTx(ctx, tx, runID); err != nil {
			return err
		}
		if _, err := outbox.InsertTx(ctx, tx, outbox.EventEnvelope{
			EventType: runtime.EventRunPlanAccepted, TenantID: run.TenantID, ProjectID: run.ProjectID, RunID: run.ID,
			CorrelationID: run.CorrelationID,
			Payload:       map[string]any{"task_count": len(tasks), "root_tasks": roots, "order": order},
		}); err != nil {
			return err
		}
		for _, t := range tasks {
			if t.State == runtime.TaskReady {
				if _, err := outbox.InsertTx(ctx, tx, taskEvent(run, t, runtime.EventTaskReady, nil)); err != nil {
					return err
				}
			}
		}
		_, err = runs.TransitionTx(ctx, tx, runID, runs.StateRunning, "")
		return err
	})
	if err != nil {
		return nil, err
	}
	return tasks, nil
}

func (s *Store) checkAgents(ctx context.Context, tx pgx.Tx, specs []runtime.TaskSpec) error {
	want := map[string]bool{}
	for _, sp := range specs {
		want[sp.AgentType] = true
	}
	types := make([]string, 0, len(want))
	for t := range want {
		types = append(types, t)
	}
	rows, err := tx.Query(ctx, `SELECT agent_type FROM agents WHERE status = 'ACTIVE' AND agent_type = ANY($1)`, types)
	if err != nil {
		return err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, f := range found {
		delete(want, f)
	}
	for missing := range want {
		return fmt.Errorf("%w: %q", ErrUnknownAgent, missing)
	}
	return nil
}

// ListTasks returns a run's tasks (tenant-scoped).
func (s *Store) ListTasks(ctx context.Context, tenantID, runID string) ([]runtime.Task, error) {
	if !ids.Valid(runID) {
		return nil, runs.ErrNotFound
	}
	var tasks []runtime.Task
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM design_runs WHERE id = $1)`, runID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return runs.ErrNotFound
		}
		var err error
		tasks, err = listTasksTx(ctx, tx, runID)
		return err
	})
	return tasks, err
}

func listTasksTx(ctx context.Context, tx pgx.Tx, runID string) ([]runtime.Task, error) {
	rows, err := tx.Query(ctx, `SELECT `+taskCols("t")+` FROM agent_tasks t WHERE t.run_id = $1 ORDER BY t.created_at, t.task_key`, runID)
	if err != nil {
		return nil, err
	}
	tasks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (runtime.Task, error) { return scanTask(r) })
	if err != nil {
		return nil, err
	}
	if tasks == nil {
		tasks = []runtime.Task{}
	}
	return tasks, nil
}

// ---------------------------------------------------------------------------
// Leasing
// ---------------------------------------------------------------------------

// ClaimRequest describes what a worker can execute.
type ClaimRequest struct {
	WorkerID   string
	AgentTypes []string
	LeaseTTL   time.Duration
}

// Claim leases the highest-priority READY task the worker can run whose budget fits
// the run. It returns (nil, nil) when nothing is claimable.
func (s *Store) Claim(ctx context.Context, req ClaimRequest) (*runtime.Lease, error) {
	if req.WorkerID == "" || len(req.AgentTypes) == 0 || req.LeaseTTL <= 0 {
		return nil, errors.New("claim: worker id, agent types and lease ttl are required")
	}
	skip := []string{}
	for i := 0; i < maxClaimCandidates; i++ {
		var (
			lease   *runtime.Lease
			skipped string
		)
		err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
			var taskID, runID string
			err := tx.QueryRow(ctx, `
				SELECT t.id::text, t.run_id::text
				FROM agent_tasks t JOIN design_runs r ON r.id = t.run_id
				WHERE t.state = 'READY' AND t.available_at <= NOW() AND t.agent_type = ANY($1)
				  AND r.state = 'RUNNING'
				  AND r.spent_budget_usd + r.reserved_budget_usd + t.budget_usd <= r.max_budget_usd
				  AND NOT (t.id::text = ANY($2))
				ORDER BY t.priority DESC, t.created_at, t.id
				LIMIT 1`, req.AgentTypes, skip).Scan(&taskID, &runID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}

			run, err := runs.LockTx(ctx, tx, runID)
			if err != nil {
				return err
			}
			var budget float64
			err = tx.QueryRow(ctx, `
				SELECT budget_usd::float8 FROM agent_tasks
				WHERE id = $1 AND state = 'READY' AND available_at <= NOW()
				FOR UPDATE SKIP LOCKED`, taskID).Scan(&budget)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if errors.Is(err, pgx.ErrNoRows) || run.State != runs.StateRunning ||
				run.SpentBudgetUSD+run.ReservedBudgetUSD+budget > run.MaxBudgetUSD+1e-9 {
				skipped = taskID // raced with another worker or the budget moved; try the next candidate
				return nil
			}

			token := ids.New()
			task, err := scanTask(tx.QueryRow(ctx, `
				UPDATE agent_tasks t
				SET state = 'LEASED', attempt_count = attempt_count + 1, lease_owner = $2, lease_token = $3,
				    lease_expires_at = NOW() + make_interval(secs => $4), updated_at = NOW()
				WHERE t.id = $1
				RETURNING `+taskCols("t"), taskID, req.WorkerID, token, req.LeaseTTL.Seconds()))
			if err != nil {
				return fmt.Errorf("lease task: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE design_runs SET reserved_budget_usd = reserved_budget_usd + $2 WHERE id = $1`, runID, task.BudgetUSD); err != nil {
				return fmt.Errorf("reserve budget: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO task_attempts (id, tenant_id, task_id, attempt_number, worker_id, state)
				VALUES ($1, $2, $3, $4, $5, 'RUNNING')`, token, task.TenantID, task.ID, task.AttemptCount, req.WorkerID); err != nil {
				return fmt.Errorf("record attempt: %w", err)
			}
			if _, err := outbox.InsertTx(ctx, tx, taskEvent(run, task, runtime.EventTaskLeased, map[string]any{
				"worker_id": req.WorkerID, "lease_expires_at": task.LeaseExpiresAt,
			})); err != nil {
				return err
			}

			deps, err := dependencyResults(ctx, tx, task.ID)
			if err != nil {
				return err
			}
			cp, err := latestCheckpoint(ctx, tx, task.ID)
			if err != nil {
				return err
			}
			lease = &runtime.Lease{
				Task:          task,
				Token:         token,
				AttemptNumber: task.AttemptCount,
				WorkerID:      req.WorkerID,
				ExpiresAt:     task.LeaseExpiresAt.UTC(),
				Deadline:      time.Now().Add(time.Duration(task.TimeoutSeconds) * time.Second),
				Run: runtime.RunContext{
					TenantID: run.TenantID, ProjectID: run.ProjectID, RunID: run.ID, CorrelationID: run.CorrelationID,
					Objective: run.Objective, Constraints: run.Constraints, AcceptanceCriteria: run.AcceptanceCriteria,
				},
				Dependencies:   deps,
				LastCheckpoint: cp,
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if lease != nil || skipped == "" {
			return lease, nil
		}
		skip = append(skip, skipped)
	}
	return nil, nil
}

func dependencyResults(ctx context.Context, tx pgx.Tx, taskID string) ([]runtime.DependencyResult, error) {
	rows, err := tx.Query(ctx, `
		SELECT p.id::text, p.task_key, p.agent_type, COALESCE(p.result_json, '{}'::jsonb)
		FROM task_dependencies d JOIN agent_tasks p ON p.id = d.depends_on_task_id
		WHERE d.task_id = $1 ORDER BY p.task_key`, taskID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (runtime.DependencyResult, error) {
		var d runtime.DependencyResult
		var raw []byte
		if err := r.Scan(&d.TaskID, &d.Key, &d.AgentType, &raw); err != nil {
			return d, err
		}
		return d, json.Unmarshal(raw, &d.Result)
	})
	if out == nil {
		out = []runtime.DependencyResult{}
	}
	return out, err
}

func latestCheckpoint(ctx context.Context, tx pgx.Tx, taskID string) (*runtime.Checkpoint, error) {
	var cp runtime.Checkpoint
	var raw []byte
	err := tx.QueryRow(ctx, `
		SELECT seq, attempt_id::text, kind, state_json, created_at FROM task_checkpoints
		WHERE task_id = $1 ORDER BY seq DESC LIMIT 1`, taskID).Scan(&cp.Seq, &cp.AttemptID, &cp.Kind, &raw, &cp.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &cp.State); err != nil {
		return nil, err
	}
	return &cp, nil
}

// Heartbeat extends a lease. It fails with ErrLeaseLost if the token is no longer current.
func (s *Store) Heartbeat(ctx context.Context, lease *runtime.Lease, ttl time.Duration) error {
	return postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		var expires time.Time
		err := tx.QueryRow(ctx, `
			UPDATE agent_tasks SET lease_expires_at = NOW() + make_interval(secs => $3), updated_at = NOW()
			WHERE id = $1 AND lease_token = $2 AND state = 'LEASED'
			RETURNING lease_expires_at`, lease.Task.ID, lease.Token, ttl.Seconds()).Scan(&expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE task_attempts SET last_heartbeat_at = NOW() WHERE id = $1`, lease.Token); err != nil {
			return err
		}
		lease.ExpiresAt = expires.UTC()
		return nil
	})
}

// Checkpoint durably records intermediate attempt state under the current lease.
func (s *Store) Checkpoint(ctx context.Context, lease *runtime.Lease, state map[string]any) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode checkpoint: %w", err)
	}
	return postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO task_checkpoints (tenant_id, task_id, attempt_id, kind, state_json)
			SELECT tenant_id, id, $2, 'PROGRESS', $3 FROM agent_tasks
			WHERE id = $1 AND lease_token = $2 AND state = 'LEASED'`, lease.Task.ID, lease.Token, raw)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrLeaseLost
		}
		return nil
	})
}

// lockLeased locks the run, then the task, verifying the lease token is current.
func lockLeased(ctx context.Context, tx pgx.Tx, lease *runtime.Lease) (runs.DesignRun, runtime.Task, error) {
	run, err := runs.LockTx(ctx, tx, lease.Task.RunID)
	if err != nil {
		return runs.DesignRun{}, runtime.Task{}, err
	}
	task, err := scanTask(tx.QueryRow(ctx, `
		SELECT `+taskCols("t")+` FROM agent_tasks t
		WHERE t.id = $1 AND t.lease_token = $2 AND t.state = 'LEASED'
		FOR UPDATE OF t`, lease.Task.ID, lease.Token))
	if errors.Is(err, pgx.ErrNoRows) {
		return runs.DesignRun{}, runtime.Task{}, ErrLeaseLost
	}
	return run, task, err
}

// Complete records a successful attempt, settles its budget, unblocks dependents and
// advances the run (EVALUATING when every task succeeded; FAILED if over budget).
func (s *Store) Complete(ctx context.Context, lease *runtime.Lease, result map[string]any, usage runtime.Usage) error {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	return postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		run, task, err := lockLeased(ctx, tx, lease)
		if err != nil {
			return err
		}
		task, err = scanTask(tx.QueryRow(ctx, `
			UPDATE agent_tasks t
			SET state = 'SUCCEEDED', result_json = $2, spent_usd = spent_usd + $3, last_error = NULL,
			    lease_owner = NULL, lease_token = NULL, lease_expires_at = NULL, updated_at = NOW(), completed_at = NOW()
			WHERE t.id = $1
			RETURNING `+taskCols("t"), task.ID, resultJSON, usage.CostUSD))
		if err != nil {
			return err
		}
		if err := finishAttempt(ctx, tx, lease.Token, "SUCCEEDED", "", usage); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO task_checkpoints (tenant_id, task_id, attempt_id, kind, state_json) VALUES ($1, $2, $3, 'RESULT', $4)`,
			task.TenantID, task.ID, lease.Token, resultJSON); err != nil {
			return err
		}
		if run, err = settleBudget(ctx, tx, run, task.BudgetUSD, usage.CostUSD); err != nil {
			return err
		}
		if _, err := outbox.InsertTx(ctx, tx, taskEvent(run, task, runtime.EventTaskSucceeded, map[string]any{"usage": usage})); err != nil {
			return err
		}
		if err := promoteDependents(ctx, tx, run, task.ID); err != nil {
			return err
		}
		return advanceRun(ctx, tx, run)
	})
}

// Fail records a failed attempt; retryable failures are re-queued with backoff until
// max_attempts, after which the task fails and the run fails with it.
func (s *Store) Fail(ctx context.Context, lease *runtime.Lease, f runtime.Failure) error {
	return postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		run, task, err := lockLeased(ctx, tx, lease)
		if err != nil {
			return err
		}
		return s.failLockedTx(ctx, tx, run, task, lease.Token, "FAILED", f, "")
	})
}

func (s *Store) failLockedTx(ctx context.Context, tx pgx.Tx, run runs.DesignRun, task runtime.Task, token, attemptState string, f runtime.Failure, extraEvent string) error {
	if err := finishAttempt(ctx, tx, token, attemptState, f.Message, f.Usage); err != nil {
		return err
	}
	if f.Result != nil {
		raw, err := json.Marshal(f.Result)
		if err != nil {
			return fmt.Errorf("encode failed result: %w", err)
		}
		// result_json holds the latest attempt's AgentResult; a later success overwrites it.
		if _, err := tx.Exec(ctx, `UPDATE agent_tasks SET result_json = $2 WHERE id = $1`, task.ID, raw); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO task_checkpoints (tenant_id, task_id, attempt_id, kind, state_json) VALUES ($1, $2, $3, 'RESULT', $4)`,
			task.TenantID, task.ID, token, raw); err != nil {
			return err
		}
	}
	run, err := settleBudget(ctx, tx, run, task.BudgetUSD, f.Usage.CostUSD)
	if err != nil {
		return err
	}
	if extraEvent != "" {
		if _, err := outbox.InsertTx(ctx, tx, taskEvent(run, task, extraEvent, map[string]any{"error": f.Message})); err != nil {
			return err
		}
	}

	retry := f.Retryable && task.AttemptCount < task.MaxAttempts && run.SpentBudgetUSD <= run.MaxBudgetUSD
	if retry {
		backoff := retryBackoff(task.AttemptCount)
		task, err = scanTask(tx.QueryRow(ctx, `
			UPDATE agent_tasks t
			SET state = 'READY', available_at = NOW() + make_interval(secs => $3), last_error = $2, spent_usd = spent_usd + $4,
			    lease_owner = NULL, lease_token = NULL, lease_expires_at = NULL, updated_at = NOW()
			WHERE t.id = $1
			RETURNING `+taskCols("t"), task.ID, f.Message, backoff.Seconds(), f.Usage.CostUSD))
		if err != nil {
			return err
		}
		_, err = outbox.InsertTx(ctx, tx, taskEvent(run, task, runtime.EventTaskRetryScheduled, map[string]any{
			"error": f.Message, "retry_in_seconds": backoff.Seconds(),
		}))
		return err
	}

	task, err = scanTask(tx.QueryRow(ctx, `
		UPDATE agent_tasks t
		SET state = 'FAILED', last_error = $2, spent_usd = spent_usd + $3,
		    lease_owner = NULL, lease_token = NULL, lease_expires_at = NULL, updated_at = NOW(), completed_at = NOW()
		WHERE t.id = $1
		RETURNING `+taskCols("t"), task.ID, f.Message, f.Usage.CostUSD))
	if err != nil {
		return err
	}
	if _, err := outbox.InsertTx(ctx, tx, taskEvent(run, task, runtime.EventTaskFailed, map[string]any{
		"error": f.Message, "retryable": f.Retryable,
	})); err != nil {
		return err
	}
	reason := "task_failed:" + task.Key
	if run.SpentBudgetUSD > run.MaxBudgetUSD {
		reason = FailureReasonBudgetExceeded
	}
	_, err = runs.TransitionTx(ctx, tx, run.ID, runs.StateFailed, reason)
	return err
}

func retryBackoff(attempt int) time.Duration {
	d := time.Duration(math.Pow(2, float64(max(attempt-1, 0)))) * 2 * time.Second
	return min(d, maxRetryBackoff)
}

func finishAttempt(ctx context.Context, tx pgx.Tx, token, state, errMsg string, u runtime.Usage) error {
	var errArg any
	if errMsg != "" {
		errArg = errMsg
	}
	_, err := tx.Exec(ctx, `
		UPDATE task_attempts
		SET state = $2, error = $3, finished_at = NOW(), model_calls = $4, tool_calls = $5,
		    prompt_tokens = $6, completion_tokens = $7, cost_usd = $8
		WHERE id = $1`, token, state, errArg, u.ModelCalls, u.ToolCalls, u.PromptTokens, u.CompletionTokens, u.CostUSD)
	return err
}

// settleBudget releases a lease's reservation and charges the actual cost.
func settleBudget(ctx context.Context, tx pgx.Tx, run runs.DesignRun, reserved, cost float64) (runs.DesignRun, error) {
	err := tx.QueryRow(ctx, `
		UPDATE design_runs
		SET reserved_budget_usd = GREATEST(reserved_budget_usd - $2, 0), spent_budget_usd = spent_budget_usd + $3, updated_at = NOW()
		WHERE id = $1
		RETURNING reserved_budget_usd::float8, spent_budget_usd::float8`, run.ID, reserved, cost).
		Scan(&run.ReservedBudgetUSD, &run.SpentBudgetUSD)
	return run, err
}

// promoteDependents moves PENDING tasks whose dependencies have all succeeded to READY.
func promoteDependents(ctx context.Context, tx pgx.Tx, run runs.DesignRun, taskID string) error {
	rows, err := tx.Query(ctx, `
		UPDATE agent_tasks t SET state = 'READY', available_at = NOW(), updated_at = NOW()
		WHERE t.state = 'PENDING'
		  AND t.id IN (SELECT task_id FROM task_dependencies WHERE depends_on_task_id = $1)
		  AND NOT EXISTS (
		      SELECT 1 FROM task_dependencies d JOIN agent_tasks p ON p.id = d.depends_on_task_id
		      WHERE d.task_id = t.id AND p.state <> 'SUCCEEDED')
		RETURNING `+taskCols("t"), taskID)
	if err != nil {
		return err
	}
	ready, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (runtime.Task, error) { return scanTask(r) })
	if err != nil {
		return err
	}
	for _, t := range ready {
		if _, err := outbox.InsertTx(ctx, tx, taskEvent(run, t, runtime.EventTaskReady, nil)); err != nil {
			return err
		}
	}
	return nil
}

// advanceRun moves a RUNNING run forward once its tasks allow it.
func advanceRun(ctx context.Context, tx pgx.Tx, run runs.DesignRun) error {
	if run.State != runs.StateRunning {
		return nil
	}
	if run.SpentBudgetUSD > run.MaxBudgetUSD+1e-9 {
		_, err := runs.TransitionTx(ctx, tx, run.ID, runs.StateFailed, FailureReasonBudgetExceeded)
		return err
	}
	var open int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_tasks WHERE run_id = $1 AND state <> 'SUCCEEDED'`, run.ID).Scan(&open); err != nil {
		return err
	}
	if open == 0 {
		_, err := runs.TransitionTx(ctx, tx, run.ID, runs.StateEvaluating, "")
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Maintenance (run by cmd/scheduler)
// ---------------------------------------------------------------------------

// ReclaimExpired takes back leases whose holders stopped heartbeating. Each reclaimed
// attempt is marked EXPIRED and the task is retried (or failed once attempts run out).
func (s *Store) ReclaimExpired(ctx context.Context, limit int) (int, error) {
	type cand struct{ taskID, runID, token string }
	var cands []cand
	err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id::text, run_id::text, lease_token::text FROM agent_tasks
			WHERE state = 'LEASED' AND lease_expires_at < NOW()
			ORDER BY lease_expires_at LIMIT $1`, limit)
		if err != nil {
			return err
		}
		cands, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (cand, error) {
			var c cand
			return c, r.Scan(&c.taskID, &c.runID, &c.token)
		})
		return err
	})
	if err != nil {
		return 0, err
	}
	reclaimed := 0
	for _, c := range cands {
		err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
			run, err := runs.LockTx(ctx, tx, c.runID)
			if err != nil {
				return err
			}
			task, err := scanTask(tx.QueryRow(ctx, `
				SELECT `+taskCols("t")+` FROM agent_tasks t
				WHERE t.id = $1 AND t.lease_token = $2 AND t.state = 'LEASED' AND t.lease_expires_at < NOW()
				FOR UPDATE OF t SKIP LOCKED`, c.taskID, c.token))
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // heartbeated, completed or reclaimed concurrently
			}
			if err != nil {
				return err
			}
			reclaimed++
			return s.failLockedTx(ctx, tx, run, task, c.token, "EXPIRED",
				runtime.Failure{Message: "lease expired without heartbeat", Retryable: true}, runtime.EventTaskLeaseExpired)
		})
		if err != nil {
			return reclaimed, err
		}
	}
	return reclaimed, nil
}

// FailStarvedRuns fails RUNNING runs that can make no further progress because no
// remaining task fits the remaining budget and nothing is in flight.
func (s *Store) FailStarvedRuns(ctx context.Context) (int, error) {
	const starved = `
		r.state = 'RUNNING'
		AND NOT EXISTS (SELECT 1 FROM agent_tasks t WHERE t.run_id = r.id AND t.state = 'LEASED')
		AND EXISTS (SELECT 1 FROM agent_tasks t WHERE t.run_id = r.id AND t.state IN ('READY', 'PENDING'))
		AND NOT EXISTS (SELECT 1 FROM agent_tasks t WHERE t.run_id = r.id AND t.state = 'READY'
		                AND r.spent_budget_usd + r.reserved_budget_usd + t.budget_usd <= r.max_budget_usd)`
	var runIDs []string
	err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT r.id::text FROM design_runs r WHERE `+starved+` LIMIT 100`)
		if err != nil {
			return err
		}
		runIDs, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return 0, err
	}
	failed := 0
	for _, id := range runIDs {
		err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
			if _, err := runs.LockTx(ctx, tx, id); err != nil {
				return err
			}
			var still bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM design_runs r WHERE r.id = $1 AND `+starved+`)`, id).Scan(&still); err != nil {
				return err
			}
			if !still {
				return nil
			}
			failed++
			_, err := runs.TransitionTx(ctx, tx, id, runs.StateFailed, FailureReasonBudgetExhausted)
			return err
		})
		if err != nil {
			return failed, err
		}
	}
	return failed, nil
}
