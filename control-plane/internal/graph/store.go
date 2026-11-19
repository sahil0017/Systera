package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
)

// Store reads architecture models (as the tenant-bound application role) and performs the
// two non-patch writes, model creation and snapshot publishing, as the graph-writer role.
// All other graph mutation goes through Service.ApplyPatch.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const modelCols = `m.id::text, m.tenant_id::text, m.project_id::text, COALESCE(v.version_number, 0),
	m.schema_version, m.status, m.created_at, m.updated_at`

func scanModel(row pgx.Row) (Model, error) {
	var m Model
	err := row.Scan(&m.ID, &m.TenantID, &m.ProjectID, &m.CurrentVersion, &m.SchemaVersion, &m.Status, &m.CreatedAt, &m.UpdatedAt)
	m.CreatedAt, m.UpdatedAt = m.CreatedAt.UTC(), m.UpdatedAt.UTC()
	return m, err
}

const versionCols = `id::text, model_id::text, version_number, COALESCE(parent_version_id::text, ''), graph_hash,
	node_count, edge_count, change_summary, author_type, author_id, COALESCE(patch_id::text, ''), created_at`

func scanVersion(row pgx.Row) (Version, error) {
	var v Version
	err := row.Scan(&v.ID, &v.ModelID, &v.Number, &v.ParentID, &v.GraphHash, &v.NodeCount, &v.EdgeCount,
		&v.ChangeSummary, &v.AuthorType, &v.AuthorID, &v.PatchID, &v.CreatedAt)
	v.CreatedAt = v.CreatedAt.UTC()
	return v, err
}

// CreateModel creates a project's architecture model with an empty version 0 and emits
// architecture.graph.version.created, all in one transaction.
func (s *Store) CreateModel(ctx context.Context, tenantID, projectID string, author Author, correlationID string) (Model, Version, error) {
	if !ids.Valid(projectID) {
		return Model{}, Version{}, ErrProjectNotFound
	}
	if !ids.Valid(correlationID) {
		correlationID = ids.New()
	}
	var (
		model Model
		ver   Version
	)
	err := postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.GraphRole, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1 AND tenant_id = $2)`, projectID, tenantID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrProjectNotFound
		}
		var modelID string
		err := tx.QueryRow(ctx, `
			INSERT INTO architecture_models (tenant_id, project_id) VALUES ($1, $2)
			ON CONFLICT (project_id) DO NOTHING
			RETURNING id::text`, tenantID, projectID).Scan(&modelID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrModelExists
		}
		if err != nil {
			return fmt.Errorf("insert model: %w", err)
		}
		ver, err = scanVersion(tx.QueryRow(ctx, `
			INSERT INTO architecture_versions (tenant_id, model_id, version_number, graph_hash, change_summary, author_type, author_id)
			VALUES ($1, $2, 0, $3, 'initial empty model', $4, $5)
			RETURNING `+versionCols, tenantID, modelID, Hash(nil, nil), author.Type, author.ID))
		if err != nil {
			return fmt.Errorf("insert version 0: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE architecture_models SET current_version_id = $2 WHERE id = $1`, modelID, ver.ID); err != nil {
			return fmt.Errorf("set current version: %w", err)
		}
		model, err = scanModel(tx.QueryRow(ctx, `
			SELECT `+modelCols+` FROM architecture_models m
			LEFT JOIN architecture_versions v ON v.id = m.current_version_id WHERE m.id = $1`, modelID))
		if err != nil {
			return err
		}
		_, err = outbox.InsertTx(ctx, tx, outbox.EventEnvelope{
			EventType: EventVersionCreated, TenantID: tenantID, ProjectID: projectID, CorrelationID: correlationID,
			Payload: map[string]any{
				"model_id": modelID, "graph_version": 0, "previous_version": nil, "patch_id": nil,
				"graph_hash": ver.GraphHash, "changed_node_ids": []string{}, "invalidated_artifact_ids": []string{},
			},
		})
		return err
	})
	if err != nil {
		return Model{}, Version{}, err
	}
	return model, ver, nil
}

// ModelForProject returns the project's model.
func (s *Store) ModelForProject(ctx context.Context, tenantID, projectID string) (Model, error) {
	if !ids.Valid(projectID) {
		return Model{}, ErrModelNotFound
	}
	var m Model
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var err error
		m, err = scanModel(tx.QueryRow(ctx, `
			SELECT `+modelCols+` FROM architecture_models m
			LEFT JOIN architecture_versions v ON v.id = m.current_version_id
			WHERE m.project_id = $1 AND m.tenant_id = $2`, projectID, tenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Model{}, ErrModelNotFound
	}
	return m, err
}

// resolveVersion maps number (<0 = current) to a version row of the model.
func resolveVersion(ctx context.Context, tx pgx.Tx, tenantID, modelID string, number int) (Version, error) {
	if !ids.Valid(modelID) {
		return Version{}, ErrModelNotFound
	}
	var (
		v   Version
		err error
	)
	if number < 0 {
		v, err = scanVersion(tx.QueryRow(ctx, `
			SELECT `+versionCols+` FROM architecture_versions
			WHERE id = (SELECT current_version_id FROM architecture_models WHERE id = $1 AND tenant_id = $2)`, modelID, tenantID))
	} else {
		v, err = scanVersion(tx.QueryRow(ctx, `
			SELECT `+versionCols+` FROM architecture_versions WHERE model_id = $1 AND tenant_id = $2 AND version_number = $3`,
			modelID, tenantID, number))
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, ErrVersionNotFound
	}
	return v, err
}

// Versions lists a model's versions, newest first.
func (s *Store) Versions(ctx context.Context, tenantID, modelID string, limit int) ([]Version, error) {
	if !ids.Valid(modelID) {
		return nil, ErrModelNotFound
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var out []Version
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+versionCols+` FROM architecture_versions
			WHERE model_id = $1 AND tenant_id = $2 ORDER BY version_number DESC LIMIT $3`, modelID, tenantID, limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Version, error) { return scanVersion(r) })
		return err
	})
	if out == nil {
		out = []Version{}
	}
	return out, err
}

// liveAt is the predicate for rows whose validity range contains version $2.
const liveAt = `valid_from_version <= $2 AND (valid_to_version IS NULL OR valid_to_version > $2)`

const nodeCols = `node_id::text, node_key, node_type, name, description, properties, COALESCE(owner_domain, ''),
	lifecycle_state, evidence_state, revision, valid_from_version`

func scanNode(row pgx.Row) (Node, error) {
	var n Node
	var props []byte
	if err := row.Scan(&n.ID, &n.Key, &n.Type, &n.Name, &n.Description, &props, &n.OwnerDomain,
		&n.LifecycleState, &n.EvidenceState, &n.Revision, &n.ValidFrom); err != nil {
		return Node{}, err
	}
	var err error
	n.Properties, err = decodeObject(props)
	return n, err
}

const edgeCols = `edge_id::text, source_node_id::text, target_node_id::text, relation_type, direction,
	COALESCE(protocol, ''), COALESCE(sync_mode, ''), constraints, properties, revision, valid_from_version`

func scanEdge(row pgx.Row) (Edge, error) {
	var e Edge
	var cons, props []byte
	if err := row.Scan(&e.ID, &e.SourceNodeID, &e.TargetNodeID, &e.RelationType, &e.Direction,
		&e.Protocol, &e.SyncMode, &cons, &props, &e.Revision, &e.ValidFrom); err != nil {
		return Edge{}, err
	}
	var err error
	if e.Constraints, err = decodeObject(cons); err != nil {
		return Edge{}, err
	}
	e.Properties, err = decodeObject(props)
	return e, err
}

// decodeObject keeps numbers as json.Number so canonical hashing is stable.
func decodeObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	m := map[string]any{}
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode jsonb object: %w", err)
	}
	return m, nil
}

// GraphAt returns the model's full content at a version (number < 0 = current), with
// nodes ordered by key and edges by (source, relation, target) for stable output.
func (s *Store) GraphAt(ctx context.Context, tenantID, modelID string, number int) (Graph, error) {
	var g Graph
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var err error
		g, err = graphAtTx(ctx, tx, tenantID, modelID, number)
		return err
	})
	return g, err
}

func graphAtTx(ctx context.Context, tx pgx.Tx, tenantID, modelID string, number int) (Graph, error) {
	var g Graph
	v, err := resolveVersion(ctx, tx, tenantID, modelID, number)
	if err != nil {
		return g, err
	}
	g.Version = v
	rows, err := tx.Query(ctx, `SELECT `+nodeCols+` FROM architecture_nodes
		WHERE model_id = $1 AND `+liveAt+` ORDER BY node_key`, modelID, v.Number)
	if err != nil {
		return g, err
	}
	if g.Nodes, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Node, error) { return scanNode(r) }); err != nil {
		return g, err
	}
	rows, err = tx.Query(ctx, `SELECT `+edgeCols+` FROM architecture_edges
		WHERE model_id = $1 AND `+liveAt+` ORDER BY source_node_id, relation_type, target_node_id, edge_id`, modelID, v.Number)
	if err != nil {
		return g, err
	}
	if g.Edges, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Edge, error) { return scanEdge(r) }); err != nil {
		return g, err
	}
	if g.Nodes == nil {
		g.Nodes = []Node{}
	}
	if g.Edges == nil {
		g.Edges = []Edge{}
	}
	return g, nil
}

// Reachable returns every node reachable from start at a version, following edges in
// the given direction (BIDIRECTIONAL edges are traversable both ways). It is a single
// recursive CTE over the adjacency tables; UNION (not UNION ALL) gives visited-set
// semantics, so the walk terminates on cycles and visits each node once.
func (s *Store) Reachable(ctx context.Context, tenantID, modelID string, number int, start string, q TraversalQuery) (TraversalResult, error) {
	switch q.Direction {
	case Outbound, Inbound, Both:
	case "":
		q.Direction = Outbound
	default:
		return TraversalResult{}, fmt.Errorf("%w: direction must be out, in or both", ErrInvalidQuery)
	}
	if q.MaxNodes <= 0 {
		q.MaxNodes = 500
	}
	if q.MaxNodes > 10000 {
		return TraversalResult{}, fmt.Errorf("%w: max_nodes must be <= 10000", ErrInvalidQuery)
	}
	if !ids.Valid(start) {
		return TraversalResult{}, ErrNodeNotFound
	}
	if q.RelationTypes == nil {
		q.RelationTypes = []string{}
	}

	res := TraversalResult{Start: start, Nodes: []Node{}}
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		v, err := resolveVersion(ctx, tx, tenantID, modelID, number)
		if err != nil {
			return err
		}
		res.Version = v.Number
		var live bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM architecture_nodes WHERE model_id = $1 AND node_id = $3 AND `+liveAt+`)`,
			modelID, v.Number, start).Scan(&live); err != nil {
			return err
		}
		if !live {
			return ErrNodeNotFound
		}
		res.Nodes, err = reachableTx(ctx, tx, modelID, v.Number, []string{start}, q.Direction, q.RelationTypes, q.MaxNodes+1, false)
		return err
	})
	if err != nil {
		return TraversalResult{}, err
	}
	if len(res.Nodes) > q.MaxNodes {
		res.Nodes, res.Truncated = res.Nodes[:q.MaxNodes], true
	}
	if res.Nodes == nil {
		res.Nodes = []Node{}
	}
	return res, nil
}

// reachableTx walks from every start node (visited-set recursive CTE). includeStarts
// keeps the start nodes in the result.
func reachableTx(ctx context.Context, tx pgx.Tx, modelID string, version int, starts []string, dir Direction, relations []string, limit int, includeStarts bool) ([]Node, error) {
	if relations == nil {
		relations = []string{}
	}
	rows, err := tx.Query(ctx, `
		WITH RECURSIVE
		live_edges AS (
			SELECT source_node_id, target_node_id, direction FROM architecture_edges
			WHERE model_id = $1 AND `+liveAt+`
			  AND (cardinality($5::text[]) = 0 OR relation_type = ANY($5::text[]))
		),
		steps (from_id, to_id) AS (
			SELECT source_node_id, target_node_id FROM live_edges
			WHERE $4 IN ('out', 'both') OR direction = 'BIDIRECTIONAL'
			UNION ALL
			SELECT target_node_id, source_node_id FROM live_edges
			WHERE $4 IN ('in', 'both') OR direction = 'BIDIRECTIONAL'
		),
		reach (node_id) AS (
			SELECT unnest($3::uuid[])
			UNION
			SELECT s.to_id FROM reach r JOIN steps s ON s.from_id = r.node_id
		)
		SELECT `+nodeCols+` FROM architecture_nodes
		WHERE model_id = $1 AND `+liveAt+`
		  AND node_id IN (SELECT node_id FROM reach)
		  AND ($7 OR NOT (node_id = ANY($3::uuid[])))
		ORDER BY node_key
		LIMIT $6`, modelID, version, starts, string(dir), relations, limit, includeStarts)
	if err != nil {
		return nil, err
	}
	nodes, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Node, error) { return scanNode(r) })
	if nodes == nil {
		nodes = []Node{}
	}
	return nodes, err
}

// PublishSnapshot marks a version as published. Snapshots are immutable and bound to the
// version's graph hash; publishing the same version twice returns the existing snapshot.
func (s *Store) PublishSnapshot(ctx context.Context, tenantID, modelID string, number int, label, publishedBy string) (Snapshot, error) {
	var snap Snapshot
	err := postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.GraphRole, func(tx pgx.Tx) error {
		v, err := resolveVersion(ctx, tx, tenantID, modelID, number)
		if err != nil {
			return err
		}
		const cols = `id::text, model_id::text, version_id::text, version_number, graph_hash, label, published_by, published_at`
		scan := func(row pgx.Row) error {
			err := row.Scan(&snap.ID, &snap.ModelID, &snap.VersionID, &snap.VersionNumber, &snap.GraphHash, &snap.Label, &snap.PublishedBy, &snap.PublishedAt)
			snap.PublishedAt = snap.PublishedAt.UTC()
			return err
		}
		err = scan(tx.QueryRow(ctx, `
			INSERT INTO graph_snapshots (tenant_id, model_id, version_id, version_number, graph_hash, label, published_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (version_id) DO NOTHING
			RETURNING `+cols, tenantID, modelID, v.ID, v.Number, v.GraphHash, label, publishedBy))
		if errors.Is(err, pgx.ErrNoRows) {
			return scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM graph_snapshots WHERE version_id = $1`, v.ID))
		}
		return err
	})
	return snap, err
}

// IsIntegrityViolation reports whether err came from a graph integrity rule enforced by
// the database (append-only history, version chain, endpoints, overlapping revisions).
func IsIntegrityViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "23001", "23503", "23514", "23P01": // restrict, foreign key, check, exclusion
		return true
	}
	return false
}
