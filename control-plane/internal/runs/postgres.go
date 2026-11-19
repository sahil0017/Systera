package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
)

// PostgresRepository persists design runs; every run change and its outbox event share
// one transaction.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const runColumns = `id::text, tenant_id::text, project_id::text, COALESCE(idempotency_key, ''), objective,
	constraints_json, acceptance_criteria, state, max_budget_usd::float8, spent_budget_usd::float8,
	correlation_id::text, created_by::text, created_at, updated_at, COALESCE(request_fingerprint, ''),
	reserved_budget_usd::float8, COALESCE(failure_reason, '')`

func scanRun(row pgx.Row) (DesignRun, error) {
	var r DesignRun
	var constraints, criteria []byte
	if err := row.Scan(&r.ID, &r.TenantID, &r.ProjectID, &r.IdempotencyKey, &r.Objective,
		&constraints, &criteria, &r.State, &r.MaxBudgetUSD, &r.SpentBudgetUSD,
		&r.CorrelationID, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt, &r.RequestFingerprint,
		&r.ReservedBudgetUSD, &r.FailureReason); err != nil {
		return DesignRun{}, err
	}
	if err := json.Unmarshal(constraints, &r.Constraints); err != nil {
		return DesignRun{}, fmt.Errorf("decode constraints: %w", err)
	}
	if err := json.Unmarshal(criteria, &r.AcceptanceCriteria); err != nil {
		return DesignRun{}, fmt.Errorf("decode acceptance criteria: %w", err)
	}
	if r.Constraints == nil {
		r.Constraints = map[string]any{}
	}
	if r.AcceptanceCriteria == nil {
		r.AcceptanceCriteria = []string{}
	}
	r.CreatedAt, r.UpdatedAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	return r, nil
}

func (r *PostgresRepository) CreateIdempotent(ctx context.Context, run DesignRun) (DesignRun, bool, error) {
	run, err := Prepare(run)
	if err != nil {
		return DesignRun{}, false, err
	}
	constraints, err := json.Marshal(run.Constraints)
	if err != nil {
		return DesignRun{}, false, fmt.Errorf("encode constraints: %w", err)
	}
	criteria, err := json.Marshal(run.AcceptanceCriteria)
	if err != nil {
		return DesignRun{}, false, fmt.Errorf("encode acceptance criteria: %w", err)
	}
	var idemKey any
	if run.IdempotencyKey != "" {
		idemKey = run.IdempotencyKey
	}

	var (
		result  DesignRun
		created bool
	)
	err = postgres.WithTenantTx(ctx, r.pool, run.TenantID, func(tx pgx.Tx) error {
		// ON CONFLICT DO NOTHING waits for a concurrent insert of the same key to settle,
		// so exactly one request creates the run and the rest observe it below.
		inserted, err := scanRun(tx.QueryRow(ctx, `
			INSERT INTO design_runs (tenant_id, project_id, idempotency_key, objective, constraints_json,
			                         acceptance_criteria, max_budget_usd, correlation_id, created_by, request_fingerprint)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (tenant_id, project_id, idempotency_key) DO NOTHING
			RETURNING `+runColumns,
			run.TenantID, run.ProjectID, idemKey, run.Objective, constraints, criteria,
			run.MaxBudgetUSD, run.CorrelationID, run.CreatedBy, run.RequestFingerprint))
		if errors.Is(err, pgx.ErrNoRows) {
			existing, err := scanRun(tx.QueryRow(ctx, `
				SELECT `+runColumns+` FROM design_runs
				WHERE tenant_id = $1 AND project_id = $2 AND idempotency_key = $3`,
				run.TenantID, run.ProjectID, run.IdempotencyKey))
			if err != nil {
				return fmt.Errorf("load idempotent run: %w", err)
			}
			result, created, err = checkReplay(existing, run)
			return err
		}
		if err != nil {
			return fmt.Errorf("insert design run: %w", err)
		}

		if _, err := outbox.InsertTx(ctx, tx, createdEvent(inserted)); err != nil {
			return err
		}
		if err := postgres.InsertAudit(ctx, tx, postgres.AuditRecord{
			TenantID: inserted.TenantID, ActorID: inserted.CreatedBy, ActorType: "USER",
			Action: "run.created", ResourceType: "design_run", ResourceID: inserted.ID,
			CorrelationID: inserted.CorrelationID,
			Metadata:      map[string]any{"project_id": inserted.ProjectID, "idempotency_key": inserted.IdempotencyKey},
		}); err != nil {
			return err
		}
		result, created = inserted, true
		return nil
	})
	if err != nil {
		return DesignRun{}, false, err
	}
	return result, created, nil
}

func (r *PostgresRepository) GetByID(ctx context.Context, tenantID, runID string) (DesignRun, error) {
	if !ids.Valid(runID) {
		return DesignRun{}, ErrNotFound
	}
	var run DesignRun
	err := postgres.WithTenantTx(ctx, r.pool, tenantID, func(tx pgx.Tx) error {
		var err error
		run, err = scanRun(tx.QueryRow(ctx,
			`SELECT `+runColumns+` FROM design_runs WHERE id = $1 AND tenant_id = $2`, runID, tenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DesignRun{}, ErrNotFound
	}
	return run, err
}

func (r *PostgresRepository) UpdateState(ctx context.Context, tenantID, runID string, next State) (DesignRun, error) {
	if !ids.Valid(runID) {
		return DesignRun{}, ErrNotFound
	}
	var updated DesignRun
	err := postgres.WithTenantTx(ctx, r.pool, tenantID, func(tx pgx.Tx) error {
		var err error
		updated, err = TransitionTx(ctx, tx, runID, next, "")
		return err
	})
	return updated, err
}

// LockTx loads a run with FOR UPDATE inside the caller's transaction. Run rows are always
// locked before their tasks, which keeps lock ordering consistent across the API,
// scheduler and workers.
func LockTx(ctx context.Context, tx pgx.Tx, runID string) (DesignRun, error) {
	run, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM design_runs WHERE id = $1 FOR UPDATE`, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return DesignRun{}, ErrNotFound
	}
	return run, err
}

// TransitionTx applies a validated state transition inside the caller's transaction (tenant
// or system scoped) and records run.state_changed atomically. Entering a terminal state
// also cancels the run's unfinished tasks and releases their budget reservation.
func TransitionTx(ctx context.Context, tx pgx.Tx, runID string, next State, reason string) (DesignRun, error) {
	current, err := LockTx(ctx, tx, runID)
	if err != nil {
		return DesignRun{}, err
	}
	if !isValidTransition(current.State, next) {
		return DesignRun{}, ErrInvalidTransition
	}
	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}
	updated, err := scanRun(tx.QueryRow(ctx, `
		UPDATE design_runs
		SET state = $2, updated_at = NOW(), failure_reason = COALESCE($3, failure_reason),
		    reserved_budget_usd = CASE WHEN $4 THEN 0 ELSE reserved_budget_usd END
		WHERE id = $1
		RETURNING `+runColumns, runID, next, reasonArg, next.IsTerminal()))
	if err != nil {
		return DesignRun{}, err
	}
	if next.IsTerminal() {
		if err := cancelOpenTasks(ctx, tx, updated); err != nil {
			return DesignRun{}, err
		}
	}
	if _, err := outbox.InsertTx(ctx, tx, stateChangedEvent(updated, current.State)); err != nil {
		return DesignRun{}, err
	}
	return updated, nil
}

func cancelOpenTasks(ctx context.Context, tx pgx.Tx, run DesignRun) error {
	rows, err := tx.Query(ctx, `
		WITH cancelled AS (
			UPDATE agent_tasks
			SET state = 'CANCELLED', lease_owner = NULL, lease_token = NULL, lease_expires_at = NULL,
			    updated_at = NOW(), completed_at = NOW()
			WHERE run_id = $1 AND state IN ('PENDING', 'READY', 'LEASED')
			RETURNING id, task_key
		), attempts AS (
			UPDATE task_attempts SET state = 'CANCELLED', finished_at = NOW()
			WHERE task_id IN (SELECT id FROM cancelled) AND state = 'RUNNING'
		)
		SELECT id::text, task_key FROM cancelled`, run.ID)
	if err != nil {
		return fmt.Errorf("cancel tasks: %w", err)
	}
	type ref struct{ id, key string }
	refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ref, error) {
		var r ref
		return r, row.Scan(&r.id, &r.key)
	})
	if err != nil {
		return err
	}
	for _, t := range refs {
		if _, err := outbox.InsertTx(ctx, tx, outbox.EventEnvelope{
			EventType: "task.cancelled", TenantID: run.TenantID, ProjectID: run.ProjectID, RunID: run.ID,
			CorrelationID: run.CorrelationID,
			Payload:       map[string]any{"task_id": t.id, "task_key": t.key, "task_state": "CANCELLED", "reason": "run_" + strings.ToLower(string(run.State))},
		}); err != nil {
			return err
		}
	}
	return nil
}
