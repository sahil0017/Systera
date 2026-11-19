// Package postgres provides the PostgreSQL system-of-record plumbing: connection pool,
// tenant-scoped transactions with enforced Row-Level Security, embedded migrations,
// audit records and readiness checks.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/platform/ids"
)

// AppRole is the non-owner role tenant-scoped transactions assume so RLS policies apply.
const AppRole = "systera_app"

// SystemRole is assumed by background processes (outbox relay, scheduler, workers) that
// legitimately operate across tenants. It has explicit permissive RLS policies.
const SystemRole = "systera_system"

// ErrInvalidTenant is returned when a tenant-scoped operation receives a malformed tenant ID.
var ErrInvalidTenant = errors.New("invalid tenant id")

// Open creates a connection pool and verifies connectivity.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	if cfg.MaxConns < 8 {
		cfg.MaxConns = 8
	}
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}

// WithTenantTx runs fn inside a transaction that has assumed AppRole and bound
// app.current_tenant_id, so every statement is confined to the tenant by RLS. The
// transaction commits only if fn returns nil.
func WithTenantTx(ctx context.Context, pool *pgxpool.Pool, tenantID string, fn func(pgx.Tx) error) error {
	if !ids.Valid(tenantID) {
		return ErrInvalidTenant
	}
	return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+AppRole); err != nil {
			return fmt.Errorf("assume %s: %w", AppRole, err)
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_tenant_id', $1, true)", tenantID); err != nil {
			return fmt.Errorf("bind tenant: %w", err)
		}
		return fn(tx)
	})
}

// GraphRole is assumed only by the graph service's write paths; it is the sole role
// with write privileges on the architecture graph tables.
const GraphRole = "systera_graph"

// WithTenantRoleTx is WithTenantTx under a specific non-owner role (AppRole or GraphRole).
func WithTenantRoleTx(ctx context.Context, pool *pgxpool.Pool, tenantID, role string, fn func(pgx.Tx) error) error {
	if !ids.Valid(tenantID) {
		return ErrInvalidTenant
	}
	if role != AppRole && role != GraphRole {
		return fmt.Errorf("unsupported tenant role %q", role)
	}
	return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
			return fmt.Errorf("assume %s: %w", role, err)
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_tenant_id', $1, true)", tenantID); err != nil {
			return fmt.Errorf("bind tenant: %w", err)
		}
		return fn(tx)
	})
}

// WithSystemTx runs fn in a transaction that has assumed SystemRole. Callers must still
// scope every statement to the tenant/run they are acting on.
func WithSystemTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+SystemRole); err != nil {
			return fmt.Errorf("assume %s: %w", SystemRole, err)
		}
		return fn(tx)
	})
}

// AuditRecord is an immutable audit_events row.
type AuditRecord struct {
	TenantID      string
	ActorID       string
	ActorType     string // USER, AGENT, SYSTEM
	Action        string
	ResourceType  string
	ResourceID    string
	CorrelationID string
	Metadata      map[string]any
}

// InsertAudit writes an audit record inside an existing tenant transaction.
func InsertAudit(ctx context.Context, tx pgx.Tx, rec AuditRecord) error {
	meta := rec.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal audit metadata: %w", err)
	}
	var resourceID any
	if rec.ResourceID != "" {
		resourceID = rec.ResourceID
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO audit_events (tenant_id, actor_id, actor_type, action, resource_type, resource_id, correlation_id, metadata_json)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		rec.TenantID, rec.ActorID, rec.ActorType, rec.Action, rec.ResourceType, resourceID, rec.CorrelationID, metaJSON)
	if err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}

// PingCheck verifies the database answers queries.
func PingCheck(pool *pgxpool.Pool) func(context.Context) error {
	return func(ctx context.Context) error {
		var one int
		return pool.QueryRow(ctx, "SELECT 1").Scan(&one)
	}
}

// SchemaCheck verifies every embedded migration has been applied.
func SchemaCheck(pool *pgxpool.Pool) func(context.Context) error {
	return func(ctx context.Context) error {
		want, err := LatestVersion()
		if err != nil {
			return err
		}
		have, err := CurrentVersion(ctx, pool)
		if err != nil {
			return err
		}
		if have < want {
			return fmt.Errorf("schema at version %d, need %d", have, want)
		}
		return nil
	}
}
