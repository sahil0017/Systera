package twin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
)

// EventBuilt is emitted (via the outbox) when a new twin build is recorded.
const EventBuilt = "architecture.twin.built"

// ErrNotFound reports an unknown twin build.
var ErrNotFound = errors.New("twin build not found")

// GraphReader is the part of graph.Service the builder reads.
type GraphReader interface {
	GraphAt(ctx context.Context, tenantID, modelID string, number int) (graph.Graph, error)
}

// BuildRecord is a recorded twin build.
type BuildRecord struct {
	ID              string      `json:"id"`
	ModelID         string      `json:"model_id"`
	GraphVersion    int         `json:"graph_version"`
	GraphHash       string      `json:"graph_hash"`
	BuilderVersion  string      `json:"builder_version"`
	AssumptionsHash string      `json:"assumptions_hash"`
	Basis           string      `json:"basis"`
	Status          string      `json:"status"`
	ErrorCount      int         `json:"error_count"`
	WarningCount    int         `json:"warning_count"`
	Assumptions     Assumptions `json:"assumptions"`
	CreatedBy       string      `json:"created_by"`
	CreatedAt       time.Time   `json:"created_at"`
	// Twin and Report are omitted from list responses.
	Twin   *Twin   `json:"twin,omitempty"`
	Report *Report `json:"report,omitempty"`
}

// Store builds, records and serves twins.
type Store struct {
	pool  *pgxpool.Pool
	graph GraphReader
}

func NewStore(pool *pgxpool.Pool, g GraphReader) *Store { return &Store{pool: pool, graph: g} }

func (s *Store) appTx(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.AppRole, fn)
}

// Preview builds and validates a twin without recording it.
func (s *Store) Preview(ctx context.Context, tenantID, modelID string, version int, a Assumptions) (BuildRecord, error) {
	rec, _, err := s.compute(ctx, tenantID, modelID, version, a)
	return rec, err
}

func (s *Store) compute(ctx context.Context, tenantID, modelID string, version int, a Assumptions) (BuildRecord, graph.Graph, error) {
	if !ids.Valid(modelID) {
		return BuildRecord{}, graph.Graph{}, graph.ErrModelNotFound
	}
	g, err := s.graph.GraphAt(ctx, tenantID, modelID, version)
	if err != nil {
		return BuildRecord{}, graph.Graph{}, err
	}
	na, hash, err := NormalizeAssumptions(a, g)
	if err != nil {
		return BuildRecord{}, graph.Graph{}, err
	}
	tw := Build(modelID, g, na, hash)
	rep := Validate(tw, g)
	return BuildRecord{
		ModelID: modelID, GraphVersion: g.Version.Number, GraphHash: g.Version.GraphHash, BuilderVersion: BuilderVersion,
		AssumptionsHash: hash, Basis: Basis, Status: rep.Status, ErrorCount: rep.Errors, WarningCount: rep.Warnings,
		Assumptions: na, Twin: &tw, Report: &rep,
	}, g, nil
}

// BuildTwin builds a twin for a graph version (-1 = current) and records it. Builds are
// deterministic: an existing build for the same graph version, assumptions and builder
// version is returned instead (created = false).
func (s *Store) BuildTwin(ctx context.Context, tenantID, modelID string, version int, a Assumptions, actorID string) (BuildRecord, bool, error) {
	rec, _, err := s.compute(ctx, tenantID, modelID, version, a)
	if err != nil {
		return BuildRecord{}, false, err
	}
	assumptions, _ := json.Marshal(rec.Assumptions)
	twinJSON, _ := json.Marshal(rec.Twin)
	reportJSON, _ := json.Marshal(rec.Report)
	created := false
	err = s.appTx(ctx, tenantID, func(tx pgx.Tx) error {
		id := ids.New()
		var got string
		err := tx.QueryRow(ctx, `
			INSERT INTO digital_twins (id, tenant_id, model_id, graph_version, graph_hash, builder_version, assumptions_hash, basis,
			                           status, error_count, warning_count, assumptions_json, twin_json, report_json, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			ON CONFLICT (model_id, graph_version, assumptions_hash, builder_version) DO NOTHING
			RETURNING id::text`,
			id, tenantID, modelID, rec.GraphVersion, rec.GraphHash, rec.BuilderVersion, rec.AssumptionsHash, rec.Basis,
			rec.Status, rec.ErrorCount, rec.WarningCount, assumptions, twinJSON, reportJSON, actorID).Scan(&got)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // an identical build exists
		}
		if err != nil {
			return err
		}
		created = true
		_, err = outbox.InsertTx(ctx, tx, outbox.EventEnvelope{EventType: EventBuilt, TenantID: tenantID, CorrelationID: ids.New(),
			Payload: map[string]any{"twin_id": got, "model_id": modelID, "graph_version": rec.GraphVersion, "graph_hash": rec.GraphHash,
				"assumptions_hash": rec.AssumptionsHash, "builder_version": rec.BuilderVersion, "status": rec.Status,
				"error_count": rec.ErrorCount, "warning_count": rec.WarningCount, "basis": rec.Basis}})
		return err
	})
	if err != nil {
		return BuildRecord{}, false, fmt.Errorf("record twin build: %w", err)
	}
	out, err := s.find(ctx, tenantID, `model_id = $1 AND graph_version = $2 AND assumptions_hash = $3 AND builder_version = $4`,
		modelID, rec.GraphVersion, rec.AssumptionsHash, rec.BuilderVersion)
	return out, created, err
}

const cols = `id::text, model_id::text, graph_version, graph_hash, builder_version, assumptions_hash, basis, status,
	error_count, warning_count, assumptions_json, created_by, created_at`

func scan(row pgx.Row, full bool) (BuildRecord, error) {
	var r BuildRecord
	var a, tw, rep []byte
	dest := []any{&r.ID, &r.ModelID, &r.GraphVersion, &r.GraphHash, &r.BuilderVersion, &r.AssumptionsHash, &r.Basis, &r.Status,
		&r.ErrorCount, &r.WarningCount, &a, &r.CreatedBy, &r.CreatedAt}
	if full {
		dest = append(dest, &tw, &rep)
	}
	if err := row.Scan(dest...); err != nil {
		return BuildRecord{}, err
	}
	_ = json.Unmarshal(a, &r.Assumptions)
	if full {
		r.Twin, r.Report = &Twin{}, &Report{}
		if err := json.Unmarshal(tw, r.Twin); err != nil {
			return BuildRecord{}, fmt.Errorf("decode twin: %w", err)
		}
		if err := json.Unmarshal(rep, r.Report); err != nil {
			return BuildRecord{}, fmt.Errorf("decode report: %w", err)
		}
	}
	return r, nil
}

func (s *Store) find(ctx context.Context, tenantID, where string, args ...any) (BuildRecord, error) {
	var out BuildRecord
	err := s.appTx(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scan(tx.QueryRow(ctx, `SELECT `+cols+`, twin_json, report_json FROM digital_twins WHERE `+where, args...), true)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return BuildRecord{}, ErrNotFound
	}
	return out, err
}

// Get returns one build with its twin and report.
func (s *Store) Get(ctx context.Context, tenantID, id string) (BuildRecord, error) {
	if !ids.Valid(id) {
		return BuildRecord{}, ErrNotFound
	}
	return s.find(ctx, tenantID, `id = $1`, id)
}

// List returns a model's builds, newest first, without twin and report bodies.
func (s *Store) List(ctx context.Context, tenantID, modelID string, limit int) ([]BuildRecord, error) {
	out := []BuildRecord{}
	if !ids.Valid(modelID) {
		return out, graph.ErrModelNotFound
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	err := s.appTx(ctx, tenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM architecture_models WHERE id = $1)`, modelID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return graph.ErrModelNotFound
		}
		rows, err := tx.Query(ctx, `SELECT `+cols+` FROM digital_twins WHERE model_id = $1 ORDER BY created_at DESC, id LIMIT $2`, modelID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scan(rows, false)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}
