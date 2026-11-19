package memory

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/models"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/postgres"
)

// Store persists and retrieves tenant- and project-scoped memory records in PostgreSQL + pgvector.
type Store struct {
	pool           *pgxpool.Pool
	embedder       models.EmbeddingProvider
	embeddingModel string
}

func NewStore(pool *pgxpool.Pool, embedder models.EmbeddingProvider, embeddingModel string) *Store {
	if embedder == nil {
		embedder = models.NewDeterministicHashEmbedder(models.DefaultEmbeddingDimensions)
	}
	if embeddingModel == "" {
		embeddingModel = "text-embedding-3-small"
	}
	return &Store{pool: pool, embedder: embedder, embeddingModel: embeddingModel}
}

// Record embeds and stores a distilled memory record.
func (s *Store) Record(ctx context.Context, tenantID string, in RecordInput) (Record, error) {
	if err := ValidateRecord(&in); err != nil {
		return Record{}, err
	}
	vecs, err := s.embedder.Embed(ctx, s.embeddingModel, []string{in.Summary})
	if err != nil {
		return Record{}, fmt.Errorf("embed memory: %w", err)
	}
	vecLit := models.FormatPGVector(vecs[0])

	var out Record
	err = postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.AppRole, func(tx pgx.Tx) error {
		contentJSON, _ := json.Marshal(in.Content)
		if len(contentJSON) == 0 || string(contentJSON) == "null" {
			contentJSON = []byte("{}")
		}
		var projPtr, runPtr *string
		if in.ProjectID != "" {
			if !ids.Valid(in.ProjectID) {
				return fmt.Errorf("%w: project_id must be a UUID", ErrInvalidInput)
			}
			projPtr = &in.ProjectID
		}
		if in.RunID != "" {
			if !ids.Valid(in.RunID) {
				return fmt.Errorf("%w: run_id must be a UUID", ErrInvalidInput)
			}
			runPtr = &in.RunID
		}
		// Foreign keys ignore row-level security, so check visibility explicitly: memory may
		// only reference this tenant's project and that project's runs.
		var ok bool
		if err := tx.QueryRow(ctx, `
			SELECT ($1::uuid IS NULL OR EXISTS (SELECT 1 FROM projects WHERE id = $1))
			   AND ($2::uuid IS NULL OR EXISTS (SELECT 1 FROM design_runs WHERE id = $2 AND ($1::uuid IS NULL OR project_id = $1)))`,
			projPtr, runPtr).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		id := ids.New()
		err := tx.QueryRow(ctx, `
			INSERT INTO memory_records (
				id, tenant_id, project_id, run_id, memory_type, summary, content_json,
				source_refs, embedding, relevance_score, access_policy, expires_at
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7,
				$8, $9::vector, $10, $11, $12
			)
			RETURNING created_at`,
			id, tenantID, projPtr, runPtr, string(in.MemoryType), in.Summary, contentJSON,
			in.SourceRefs, vecLit, in.RelevanceScore, in.AccessPolicy, in.ExpiresAt,
		).Scan(&out.CreatedAt)
		if err != nil {
			return err
		}
		out.ID = id
		out.TenantID = tenantID
		out.ProjectID = in.ProjectID
		out.RunID = in.RunID
		out.MemoryType = in.MemoryType
		out.Summary = in.Summary
		out.Content = in.Content
		out.SourceRefs = in.SourceRefs
		out.RelevanceScore = in.RelevanceScore
		out.AccessPolicy = in.AccessPolicy
		out.ExpiresAt = in.ExpiresAt
		return nil
	})
	return out, err
}

// Link creates a typed relationship between a memory record and a graph node, ADR, claim, or artifact.
func (s *Store) Link(ctx context.Context, tenantID, memoryID, targetType, targetID, relation string) error {
	if !ids.Valid(memoryID) || !ids.Valid(targetID) {
		return ErrInvalidInput
	}
	return postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.AppRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO memory_links (tenant_id, memory_id, target_type, target_id, relation)
			VALUES ($1, $2, $3, $4, $5)`,
			tenantID, memoryID, targetType, targetID, relation,
		)
		return err
	})
}

// Retrieve returns non-expired, permission-checked memories scoped to the tenant and project.
func (s *Store) Retrieve(ctx context.Context, tenantID string, q RetrieveQuery) ([]Record, error) {
	if q.Limit <= 0 || q.Limit > 50 {
		q.Limit = 10
	}
	types := make([]string, 0, len(q.MemoryTypes))
	for _, t := range q.MemoryTypes {
		types = append(types, string(t))
	}

	var vecLit string
	if q.Query != "" {
		vecs, err := s.embedder.Embed(ctx, s.embeddingModel, []string{q.Query})
		if err != nil {
			return nil, err
		}
		vecLit = models.FormatPGVector(vecs[0])
	}

	var out []Record
	err := postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.AppRole, func(tx pgx.Tx) error {
		var rows pgx.Rows
		var err error
		if vecLit != "" {
			rows, err = tx.Query(ctx, `
				SELECT id::text, tenant_id::text, COALESCE(project_id::text, ''), COALESCE(run_id::text, ''),
				       memory_type, summary, content_json, source_refs, relevance_score, access_policy, expires_at, created_at
				FROM memory_records
				WHERE tenant_id = $1
				  AND ($2 = '' OR project_id::text = $2 OR access_policy = 'TENANT')
				  AND (cardinality($3::text[]) = 0 OR memory_type = ANY($3::text[]))
				  AND (expires_at IS NULL OR expires_at > NOW())
				ORDER BY (0.7 * COALESCE(1.0 - (embedding <=> $4::vector), 0.0) + 0.3 * relevance_score) DESC, created_at DESC
				LIMIT $5`, tenantID, q.ProjectID, types, vecLit, q.Limit)
		} else {
			rows, err = tx.Query(ctx, `
				SELECT id::text, tenant_id::text, COALESCE(project_id::text, ''), COALESCE(run_id::text, ''),
				       memory_type, summary, content_json, source_refs, relevance_score, access_policy, expires_at, created_at
				FROM memory_records
				WHERE tenant_id = $1
				  AND ($2 = '' OR project_id::text = $2 OR access_policy = 'TENANT')
				  AND (cardinality($3::text[]) = 0 OR memory_type = ANY($3::text[]))
				  AND (expires_at IS NULL OR expires_at > NOW())
				ORDER BY relevance_score DESC, created_at DESC
				LIMIT $4`, tenantID, q.ProjectID, types, q.Limit)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Record
			var mt string
			var rawContent []byte
			if err := rows.Scan(&r.ID, &r.TenantID, &r.ProjectID, &r.RunID,
				&mt, &r.Summary, &rawContent, &r.SourceRefs, &r.RelevanceScore, &r.AccessPolicy, &r.ExpiresAt, &r.CreatedAt); err != nil {
				return err
			}
			r.MemoryType = MemoryType(mt)
			_ = json.Unmarshal(rawContent, &r.Content)
			out = append(out, r)
		}
		return rows.Err()
	})
	if out == nil {
		out = []Record{}
	}
	return out, err
}
