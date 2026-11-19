package projects

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/postgres"
)

// PostgresRepository persists projects in PostgreSQL under tenant RLS.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const projectColumns = `id::text, tenant_id::text, name, description, created_by::text, status, created_at, updated_at`

func scanProject(row pgx.Row) (Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.TenantID, &p.Name, &p.Description, &p.CreatedBy, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	p.CreatedAt, p.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	return p, err
}

func (r *PostgresRepository) Create(ctx context.Context, p Project) (Project, error) {
	if err := validate(&p); err != nil {
		return Project{}, err
	}
	var created Project
	err := postgres.WithTenantTx(ctx, r.pool, p.TenantID, func(tx pgx.Tx) error {
		var err error
		created, err = scanProject(tx.QueryRow(ctx, `
			INSERT INTO projects (tenant_id, name, description, created_by)
			VALUES ($1, $2, $3, $4)
			RETURNING `+projectColumns, p.TenantID, p.Name, p.Description, p.CreatedBy))
		if err != nil {
			return err
		}
		return postgres.InsertAudit(ctx, tx, postgres.AuditRecord{
			TenantID: p.TenantID, ActorID: p.CreatedBy, ActorType: "USER",
			Action: "project.created", ResourceType: "project", ResourceID: created.ID,
			CorrelationID: correlationOrNew(p.CorrelationID),
			Metadata:      map[string]any{"name": created.Name},
		})
	})
	return created, err
}

func (r *PostgresRepository) GetByID(ctx context.Context, tenantID, projectID string) (Project, error) {
	if !ids.Valid(projectID) {
		return Project{}, ErrNotFound
	}
	var p Project
	err := postgres.WithTenantTx(ctx, r.pool, tenantID, func(tx pgx.Tx) error {
		var err error
		p, err = scanProject(tx.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = $1 AND tenant_id = $2`, projectID, tenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

func (r *PostgresRepository) ListByTenant(ctx context.Context, tenantID string) ([]Project, error) {
	out := []Project{}
	err := postgres.WithTenantTx(ctx, r.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+projectColumns+` FROM projects WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Project, error) { return scanProject(row) })
		if err != nil {
			return err
		}
		out = append(out, list...)
		return nil
	})
	return out, err
}

func correlationOrNew(id string) string {
	if ids.Valid(id) {
		return id
	}
	return ids.New()
}
