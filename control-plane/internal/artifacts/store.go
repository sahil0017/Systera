package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/runs"
	"github.com/systera/control-plane/internal/runtime"
	"github.com/systera/control-plane/internal/scheduler"
)

var (
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:/-]{0,127}$`)
	hexHash     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Closure bounds for NODE_AND_DEPENDENCIES bindings (checklist 6.5).
const (
	defaultClosureDepth = 10
	maxClosureNodes     = 5000
)

// Content hashes of non-graph dependencies (computed in SQL so binding, verification and
// the drift sweep use exactly the same definition).
const (
	decisionHashSQL = `encode(digest(concat_ws('|', d.id::text, d.status, d.chosen_option, d.rationale, d.updated_at::text), 'sha256'), 'hex')`
	claimHashSQL    = `encode(digest(concat_ws('|', c.id::text, c.lifecycle, c.review_status, c.statement, c.last_validated_at::text), 'sha256'), 'hex')`
)

// Store persists artifacts and runs the artifact side of the dependency engine.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const artifactCols = `id::text, model_id::text, name, projection_type, owner_type, owner_id, status,
	COALESCE(current_version_id::text, ''), COALESCE(stale_reason, ''), stale_since, stale_graph_version,
	COALESCE(revalidation_run_id::text, ''), created_at, updated_at`

func scanArtifact(row pgx.Row) (Artifact, error) {
	var a Artifact
	err := row.Scan(&a.ID, &a.ModelID, &a.Name, &a.ProjectionType, &a.OwnerType, &a.OwnerID, &a.Status,
		&a.CurrentVersionID, &a.StaleReason, &a.StaleSince, &a.StaleGraphVersion, &a.RevalidationRunID, &a.CreatedAt, &a.UpdatedAt)
	a.CreatedAt, a.UpdatedAt = a.CreatedAt.UTC(), a.UpdatedAt.UTC()
	return a, err
}

const versionCols = `id::text, artifact_id::text, version_number, checksum, content_ref, media_type, size_bytes, graph_version,
	graph_hash, generated_from, COALESCE(generation_run_id::text, ''), COALESCE(agent_task_id::text, ''), evidence_refs,
	created_by_type, created_by, created_at`

func scanVersion(row pgx.Row) (Version, error) {
	var v Version
	var gen []byte
	err := row.Scan(&v.ID, &v.ArtifactID, &v.Number, &v.Checksum, &v.ContentRef, &v.MediaType, &v.SizeBytes, &v.GraphVersion,
		&v.GraphHash, &gen, &v.GenerationRunID, &v.AgentTaskID, &v.EvidenceRefs, &v.CreatedByType, &v.CreatedBy, &v.CreatedAt)
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(gen, &v.GeneratedFrom); err != nil {
		return v, err
	}
	if v.EvidenceRefs == nil {
		v.EvidenceRefs = []string{}
	}
	v.Dependencies = []Dependency{}
	v.CreatedAt = v.CreatedAt.UTC()
	return v, nil
}

func loadDeps(ctx context.Context, tx pgx.Tx, v *Version) error {
	rows, err := tx.Query(ctx, `SELECT dep_type, target_id::text, target_hash, via FROM artifact_dependencies
		WHERE artifact_version_id = $1 ORDER BY dep_type, target_id`, v.ID)
	if err != nil {
		return err
	}
	v.Dependencies, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Dependency, error) {
		var d Dependency
		return d, r.Scan(&d.Type, &d.TargetID, &d.TargetHash, &d.Via)
	})
	if v.Dependencies == nil {
		v.Dependencies = []Dependency{}
	}
	return err
}

func emit(ctx context.Context, tx pgx.Tx, tenantID, projectID, runID, eventType string, payload map[string]any) error {
	_, err := outbox.InsertTx(ctx, tx, outbox.EventEnvelope{
		EventType: eventType, TenantID: tenantID, ProjectID: projectID, RunID: runID, CorrelationID: ids.New(), Payload: payload,
	})
	return err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------------------
// Recording versions (6.1, 6.2)
// ---------------------------------------------------------------------------

type dep struct {
	typ, target, hash, via string
}

// Record records a new version of an artifact (artifactID == "" creates the artifact).
// The version is bound to the model's current graph version; every dependency's content
// hash is recorded; artifacts downstream of this one go stale.
func (s *Store) Record(ctx context.Context, tenantID, modelID, artifactID string, in RecordInput, a Actor) (RecordResult, error) {
	if !slices.Contains([]string{"USER", "AGENT", "SYSTEM"}, a.Type) || a.ID == "" {
		return RecordResult{}, fmt.Errorf("%w: actor type and id are required", ErrInvalidInput)
	}
	if !ids.Valid(modelID) || (artifactID != "" && !ids.Valid(artifactID)) {
		return RecordResult{}, ErrNotFound
	}
	var res RecordResult
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		cur, err := graph.CurrentVersionTx(ctx, tx, tenantID, modelID)
		if errors.Is(err, graph.ErrVersionNotFound) || errors.Is(err, graph.ErrModelNotFound) {
			return ErrModelNotFound
		}
		if err != nil {
			return err
		}
		var projectID string
		if err := tx.QueryRow(ctx, `SELECT project_id::text FROM architecture_models WHERE id = $1`, modelID).Scan(&projectID); err != nil {
			return err
		}
		if in.GraphVersion != nil && *in.GraphVersion != cur.Number {
			return fmt.Errorf("%w (current is %d)", ErrStaleGraphVersion, cur.Number)
		}

		var fs []Finding
		bad := func(field, code, format string, args ...any) {
			fs = append(fs, Finding{Field: field, Code: code, Message: fmt.Sprintf(format, args...)})
		}

		// The artifact itself.
		var art Artifact
		if artifactID == "" {
			if !namePattern.MatchString(in.Name) {
				bad("name", "invalid_name", "name must match %s", namePattern)
			}
			if !slices.Contains(ProjectionTypes, in.ProjectionType) {
				bad("projection_type", "invalid_projection_type", "projection_type must be one of %v", ProjectionTypes)
			}
			if len(fs) > 0 {
				return &ValidationError{Findings: fs}
			}
			art, err = scanArtifact(tx.QueryRow(ctx, `
				INSERT INTO artifacts (tenant_id, model_id, name, projection_type, owner_type, owner_id)
				VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (model_id, name) DO NOTHING RETURNING `+artifactCols,
				tenantID, modelID, in.Name, in.ProjectionType, a.Type, a.ID))
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrExists
			}
			if err != nil {
				return err
			}
		} else {
			art, err = scanArtifact(tx.QueryRow(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE id = $1 AND model_id = $2 FOR UPDATE`, artifactID, modelID))
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if art.Status == "RETIRED" {
				return ErrRetired
			}
			if (in.Name != "" && in.Name != art.Name) || (in.ProjectionType != "" && in.ProjectionType != art.ProjectionType) {
				bad("name", "identity_mismatch", "name and projection_type of an existing artifact cannot change")
			}
		}

		// Content.
		if in.Checksum == "" && in.Content != "" {
			sum := sha256.Sum256([]byte(in.Content))
			in.Checksum = hex.EncodeToString(sum[:])
			if in.SizeBytes == 0 {
				in.SizeBytes = int64(len(in.Content))
			}
		}
		in.Checksum = strings.ToLower(in.Checksum)
		if !hexHash.MatchString(in.Checksum) {
			bad("checksum", "invalid_checksum", "checksum (hex SHA-256 of the content) or content is required")
		}
		if in.MediaType == "" {
			in.MediaType = "application/octet-stream"
		}
		if len(in.ContentRef) > 2000 || in.SizeBytes < 0 {
			bad("content_ref", "invalid_content_ref", "content_ref is limited to 2000 characters and size_bytes must be >= 0")
		}
		if in.GeneratedFrom == nil {
			in.GeneratedFrom = map[string]any{}
		}
		fs = append(fs, evidenceRefFindings(ctx, tx, modelID, in.EvidenceRefs)...)

		deps, depFindings, err := resolveDependencies(ctx, tx, modelID, art.ID, cur.Number, in.Dependencies)
		if err != nil {
			return err
		}
		fs = append(fs, depFindings...)
		if len(fs) > 0 {
			return &ValidationError{Findings: fs}
		}

		// The version.
		var next int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version_number), 0) + 1 FROM artifact_versions WHERE artifact_id = $1`, art.ID).Scan(&next); err != nil {
			return err
		}
		gen, _ := json.Marshal(in.GeneratedFrom)
		refs := in.EvidenceRefs
		if refs == nil {
			refs = []string{}
		}
		v, err := scanVersion(tx.QueryRow(ctx, `
			INSERT INTO artifact_versions (tenant_id, model_id, artifact_id, version_number, checksum, content_ref, media_type, size_bytes,
			                               graph_version, graph_hash, generated_from, generation_run_id, agent_task_id, evidence_refs,
			                               created_by_type, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16) RETURNING `+versionCols,
			tenantID, modelID, art.ID, next, in.Checksum, in.ContentRef, in.MediaType, in.SizeBytes, cur.Number, cur.GraphHash, gen,
			nullable(a.RunID), nullable(a.AgentTaskID), refs, a.Type, a.ID))
		if err != nil {
			return fmt.Errorf("insert artifact version: %w", err)
		}
		batch := &pgx.Batch{}
		for _, d := range deps {
			batch.Queue(`INSERT INTO artifact_dependencies (tenant_id, model_id, artifact_version_id, dep_type, target_id, target_hash, via)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, tenantID, modelID, v.ID, d.typ, d.target, d.hash, d.via)
		}
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("record dependencies: %w", err)
		}
		art, err = scanArtifact(tx.QueryRow(ctx, `
			UPDATE artifacts SET current_version_id = $2, status = 'CURRENT', stale_reason = NULL, stale_since = NULL,
			       stale_graph_version = NULL, revalidation_run_id = NULL
			WHERE id = $1 RETURNING `+artifactCols, art.ID, v.ID))
		if err != nil {
			return err
		}
		if err := loadDeps(ctx, tx, &v); err != nil {
			return err
		}

		// Everything built on this artifact's previous version is now out of date.
		res.InvalidatedDownstream = []string{}
		if next > 1 {
			rows, err := tx.Query(ctx, `
				SELECT DISTINCT a.id::text FROM artifacts a JOIN artifact_dependencies d ON d.artifact_version_id = a.current_version_id
				WHERE a.model_id = $1 AND a.status = 'CURRENT' AND d.dep_type = 'ARTIFACT' AND d.target_id = $2 AND a.id <> $2`, modelID, art.ID)
			if err != nil {
				return err
			}
			dependents, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			seeds := map[string]string{}
			for _, id := range dependents {
				seeds[id] = fmt.Sprintf("upstream artifact %s has a new version (v%d)", art.Name, next)
			}
			res.InvalidatedDownstream, err = graph.MarkArtifactsStale(ctx, tx, modelID, cur.Number, seeds, func(id, why string) error {
				return emit(ctx, tx, tenantID, projectID, a.RunID, EventInvalidated, map[string]any{
					"model_id": modelID, "artifact_id": id, "reason": why, "graph_version": cur.Number, "cause": "artifact:" + art.ID,
				})
			})
			if err != nil {
				return err
			}
			if len(res.InvalidatedDownstream) > 0 {
				if err := tx.QueryRow(ctx, `INSERT INTO revalidation_requests (tenant_id, model_id, artifact_ids, graph_version, cause, reason)
					VALUES ($1, $2, $3::uuid[], $4, $5, $6) RETURNING id::text`, tenantID, modelID, res.InvalidatedDownstream, cur.Number,
					fmt.Sprintf("artifact:%s@v%d", art.ID, next), "upstream artifact changed").Scan(&res.RevalidationRequestID); err != nil {
					return err
				}
			}
		}
		if err := emit(ctx, tx, tenantID, projectID, a.RunID, EventVersionRecorded, map[string]any{
			"model_id": modelID, "artifact_id": art.ID, "version_id": v.ID, "version": v.Number, "checksum": v.Checksum,
			"graph_version": v.GraphVersion, "dependencies": len(v.Dependencies),
		}); err != nil {
			return err
		}
		art.Current = &v
		res.Artifact, res.Version = art, v
		return nil
	})
	return res, err
}

func evidenceRefFindings(ctx context.Context, tx pgx.Tx, modelID string, refs []string) []Finding {
	var fs []Finding
	for _, r := range refs {
		if strings.TrimSpace(r) == "" || len(r) > 500 {
			fs = append(fs, Finding{Field: "evidence_refs", Code: "invalid_evidence_ref", Message: "evidence_refs must be non-empty strings (max 500)"})
			continue
		}
		if !strings.HasPrefix(r, graph.EvidenceRefPrefix) {
			continue
		}
		id := strings.TrimPrefix(r, graph.EvidenceRefPrefix)
		var usable bool
		err := tx.QueryRow(ctx, `SELECT status = 'ACTIVE' AND (expires_at IS NULL OR expires_at > NOW()) FROM evidence_records
			WHERE id = $1 AND model_id = $2`, id, modelID).Scan(&usable)
		switch {
		case !ids.Valid(id) || errors.Is(err, pgx.ErrNoRows):
			fs = append(fs, Finding{Field: "evidence_refs", Code: "unknown_evidence", Message: fmt.Sprintf("%s does not exist in this model", r)})
		case err == nil && !usable:
			fs = append(fs, Finding{Field: "evidence_refs", Code: "evidence_not_usable", Message: fmt.Sprintf("%s is expired or revoked", r)})
		}
	}
	return fs
}

// resolveDependencies validates declared dependencies and computes their hashes.
func resolveDependencies(ctx context.Context, tx pgx.Tx, modelID, selfID string, version int, in Dependencies) ([]dep, []Finding, error) {
	var fs []Finding
	bad := func(field, code, format string, args ...any) {
		fs = append(fs, Finding{Field: field, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	if len(in.Nodes)+len(in.Artifacts)+len(in.Decisions)+len(in.Claims) == 0 {
		bad("dependencies", "no_dependencies", "an artifact must declare what it was generated from (nodes, artifacts, decisions or claims)")
		return nil, fs, nil
	}
	var deps []dep
	seen := map[string]bool{}
	add := func(d dep) {
		k := d.typ + ":" + d.target
		if !seen[k] {
			seen[k] = true
			deps = append(deps, d)
		}
	}

	// Nodes (and, for NODE_AND_DEPENDENCIES, their bounded dependency closure).
	via := map[string]string{}
	for i, nd := range in.Nodes {
		field := fmt.Sprintf("dependencies.nodes[%d]", i)
		if !ids.Valid(nd.NodeID) {
			bad(field, "invalid_node_id", "node_id must be a UUID")
			continue
		}
		if nd.Scope == "" {
			nd.Scope = ScopeNode
		}
		if nd.Scope != ScopeNode && nd.Scope != ScopeNodeDependencies {
			bad(field, "invalid_scope", "scope must be NODE or NODE_AND_DEPENDENCIES")
			continue
		}
		for _, r := range nd.RelationTypes {
			if !slices.Contains(graph.RelationTypes, r) {
				bad(field, "invalid_relation_type", "unknown relation type %q", r)
			}
		}
		via[nd.NodeID] = "direct"
		if nd.Scope == ScopeNodeDependencies {
			depth := nd.MaxDepth
			if depth <= 0 {
				depth = defaultClosureDepth
			}
			set, err := graph.TraverseTx(ctx, tx, modelID, version, []string{nd.NodeID}, graph.Outbound,
				graph.ImpactLimits{MaxDepth: depth, MaxNodes: maxClosureNodes, RelationTypes: nd.RelationTypes})
			if err != nil {
				return nil, nil, err
			}
			if set.Truncated {
				bad(field, "closure_too_large", "the dependency closure exceeds %d hops or %d nodes; bind narrower nodes", depth, maxClosureNodes)
				continue
			}
			for id := range set.Depth {
				if _, ok := via[id]; !ok {
					via[id] = "closure:" + nd.NodeID
				}
			}
		}
	}
	if len(via) > 0 {
		nodeIDs := make([]string, 0, len(via))
		for id := range via {
			nodeIDs = append(nodeIDs, id)
		}
		sort.Strings(nodeIDs)
		nodes, err := graph.NodesAtTx(ctx, tx, modelID, version, nodeIDs)
		if err != nil {
			return nil, nil, err
		}
		live := map[string]graph.Node{}
		for _, n := range nodes {
			live[n.ID] = n
		}
		for i, nd := range in.Nodes {
			if ids.Valid(nd.NodeID) {
				if _, ok := live[nd.NodeID]; !ok {
					bad(fmt.Sprintf("dependencies.nodes[%d]", i), "unknown_node", "node %s is not in the current graph version", nd.NodeID)
				}
			}
		}
		for _, id := range nodeIDs {
			if n, ok := live[id]; ok {
				add(dep{"NODE", id, graph.NodeHash(n), via[id]})
			}
		}
		edges, err := graph.EdgesFromTx(ctx, tx, modelID, version, nodeIDs)
		if err != nil {
			return nil, nil, err
		}
		for _, e := range edges {
			add(dep{"EDGE", e.ID, graph.EdgeHash(e), via[e.SourceNodeID]})
		}
	}

	// Other artifacts (must be CURRENT; no cycles).
	for i, id := range in.Artifacts {
		field := fmt.Sprintf("dependencies.artifacts[%d]", i)
		if !ids.Valid(id) {
			bad(field, "invalid_artifact_id", "artifact ids must be UUIDs")
			continue
		}
		if id == selfID {
			bad(field, "self_dependency", "an artifact cannot depend on itself")
			continue
		}
		var status, checksum string
		err := tx.QueryRow(ctx, `SELECT a.status, COALESCE(v.checksum, '') FROM artifacts a LEFT JOIN artifact_versions v ON v.id = a.current_version_id
			WHERE a.id = $1 AND a.model_id = $2`, id, modelID).Scan(&status, &checksum)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			bad(field, "unknown_artifact", "artifact %s does not exist in this model", id)
			continue
		case err != nil:
			return nil, nil, err
		case status != "CURRENT" || checksum == "":
			bad(field, "artifact_not_current", "artifact %s is %s; revalidate it first", id, status)
			continue
		}
		var cycle bool
		if err := tx.QueryRow(ctx, `
			WITH RECURSIVE up (id) AS (
				SELECT $1::uuid
				UNION
				SELECT d.target_id FROM up JOIN artifacts a ON a.id = up.id
				JOIN artifact_dependencies d ON d.artifact_version_id = a.current_version_id AND d.dep_type = 'ARTIFACT')
			SELECT EXISTS (SELECT 1 FROM up WHERE id = $2::uuid)`, id, selfID).Scan(&cycle); err != nil {
			return nil, nil, err
		}
		if cycle {
			bad(field, "dependency_cycle", "artifact %s already depends (transitively) on this artifact", id)
			continue
		}
		add(dep{"ARTIFACT", id, checksum, "direct"})
	}

	// Decisions (PROPOSED or ACCEPTED) and claims (ACTIVE).
	for i, id := range in.Decisions {
		field := fmt.Sprintf("dependencies.decisions[%d]", i)
		var status, hash string
		err := tx.QueryRow(ctx, `SELECT d.status, `+decisionHashSQL+` FROM decisions d WHERE d.id = $1 AND d.model_id = $2`, id, modelID).Scan(&status, &hash)
		switch {
		case !ids.Valid(id) || errors.Is(err, pgx.ErrNoRows):
			bad(field, "unknown_decision", "decision %s does not exist in this model", id)
		case err != nil:
			return nil, nil, err
		case status != "PROPOSED" && status != "ACCEPTED":
			bad(field, "decision_not_usable", "decision %s is %s", id, status)
		default:
			add(dep{"DECISION", id, hash, "direct"})
		}
	}
	for i, id := range in.Claims {
		field := fmt.Sprintf("dependencies.claims[%d]", i)
		var lifecycle, hash string
		err := tx.QueryRow(ctx, `SELECT c.lifecycle, `+claimHashSQL+` FROM claims c WHERE c.id = $1 AND c.model_id = $2`, id, modelID).Scan(&lifecycle, &hash)
		switch {
		case !ids.Valid(id) || errors.Is(err, pgx.ErrNoRows):
			bad(field, "unknown_claim", "claim %s does not exist in this model", id)
		case err != nil:
			return nil, nil, err
		case lifecycle != "ACTIVE":
			bad(field, "claim_not_usable", "claim %s is %s", id, lifecycle)
		default:
			add(dep{"CLAIM", id, hash, "direct"})
		}
	}
	return deps, fs, nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

func (s *Store) Get(ctx context.Context, tenantID, modelID, id string) (Artifact, error) {
	if !ids.Valid(id) || !ids.Valid(modelID) {
		return Artifact{}, ErrNotFound
	}
	var art Artifact
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var err error
		art, err = scanArtifact(tx.QueryRow(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE id = $1 AND model_id = $2`, id, modelID))
		if err != nil {
			return err
		}
		if art.CurrentVersionID != "" {
			v, err := scanVersion(tx.QueryRow(ctx, `SELECT `+versionCols+` FROM artifact_versions WHERE id = $1`, art.CurrentVersionID))
			if err != nil {
				return err
			}
			if err := loadDeps(ctx, tx, &v); err != nil {
				return err
			}
			art.Current = &v
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Artifact{}, ErrNotFound
	}
	return art, err
}

func (s *Store) List(ctx context.Context, tenantID, modelID, status string) ([]Artifact, error) {
	if !ids.Valid(modelID) {
		return nil, ErrModelNotFound
	}
	out := []Artifact{}
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM architecture_models WHERE id = $1)`, modelID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrModelNotFound
		}
		rows, err := tx.Query(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE model_id = $1 AND ($2 = '' OR status = $2) ORDER BY name`, modelID, status)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Artifact, error) { return scanArtifact(r) })
		out = append(out, list...)
		return err
	})
	return out, err
}

func (s *Store) Versions(ctx context.Context, tenantID, modelID, id string) ([]Version, error) {
	if !ids.Valid(id) || !ids.Valid(modelID) {
		return nil, ErrNotFound
	}
	var out []Version
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+versionCols+` FROM artifact_versions WHERE artifact_id = $1 AND model_id = $2 ORDER BY version_number DESC`, id, modelID)
		if err != nil {
			return err
		}
		if out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Version, error) { return scanVersion(r) }); err != nil {
			return err
		}
		for i := range out {
			if err := loadDeps(ctx, tx, &out[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil && len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, err
}

// Verify recomputes every dependency hash of the artifact's current version against the
// current graph and ledgers. It is read-only and deterministic.
func (s *Store) Verify(ctx context.Context, tenantID, modelID, id string) (VerifyReport, error) {
	art, err := s.Get(ctx, tenantID, modelID, id)
	if err != nil {
		return VerifyReport{}, err
	}
	if art.Current == nil {
		return VerifyReport{}, ErrNotFound
	}
	rep := VerifyReport{ArtifactID: art.ID, VersionID: art.Current.ID, BoundGraphVersion: art.Current.GraphVersion, Drift: []Drift{}}
	err = postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		cur, err := graph.CurrentVersionTx(ctx, tx, tenantID, modelID)
		if err != nil {
			return err
		}
		rep.CurrentGraphVersion = cur.Number
		var nodeIDs, edgeIDs []string
		for _, d := range art.Current.Dependencies {
			switch d.Type {
			case "NODE":
				nodeIDs = append(nodeIDs, d.TargetID)
			case "EDGE":
				edgeIDs = append(edgeIDs, d.TargetID)
			}
		}
		current := map[string]string{} // type:id -> hash
		nodes, err := graph.NodesAtTx(ctx, tx, modelID, cur.Number, nodeIDs)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			current["NODE:"+n.ID] = graph.NodeHash(n)
		}
		edges, err := graph.EdgesByIDTx(ctx, tx, modelID, cur.Number, edgeIDs)
		if err != nil {
			return err
		}
		for _, e := range edges {
			current["EDGE:"+e.ID] = graph.EdgeHash(e)
		}
		// Edges added from bound nodes since binding are drift too.
		if len(nodeIDs) > 0 {
			from, err := graph.EdgesFromTx(ctx, tx, modelID, cur.Number, nodeIDs)
			if err != nil {
				return err
			}
			recorded := map[string]bool{}
			for _, id := range edgeIDs {
				recorded[id] = true
			}
			for _, e := range from {
				if !recorded[e.ID] {
					rep.Drift = append(rep.Drift, Drift{Dependency: Dependency{Type: "EDGE", TargetID: e.ID, Via: "new"},
						CurrentHash: graph.EdgeHash(e), Problem: "new dependency edge from a bound node"})
				}
			}
		}
		for _, d := range art.Current.Dependencies {
			var h string
			problem := ""
			switch d.Type {
			case "NODE", "EDGE":
				h = current[d.Type+":"+d.TargetID]
			case "ARTIFACT":
				var status string
				if err := tx.QueryRow(ctx, `SELECT a.status, COALESCE(v.checksum, '') FROM artifacts a LEFT JOIN artifact_versions v ON v.id = a.current_version_id
					WHERE a.id = $1`, d.TargetID).Scan(&status, &h); err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				if status != "" && status != "CURRENT" {
					problem = "upstream artifact is " + status
				}
			case "DECISION":
				var status string
				if err := tx.QueryRow(ctx, `SELECT d.status, `+decisionHashSQL+` FROM decisions d WHERE d.id = $1`, d.TargetID).Scan(&status, &h); err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				if status != "" && status != "PROPOSED" && status != "ACCEPTED" {
					problem = "decision is " + status
				}
			case "CLAIM":
				var lifecycle string
				if err := tx.QueryRow(ctx, `SELECT c.lifecycle, `+claimHashSQL+` FROM claims c WHERE c.id = $1`, d.TargetID).Scan(&lifecycle, &h); err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				if lifecycle != "" && lifecycle != "ACTIVE" {
					problem = "claim is " + lifecycle
				}
			}
			rep.Checked++
			switch {
			case h == "":
				rep.Drift = append(rep.Drift, Drift{Dependency: d, Problem: "dependency no longer exists"})
			case problem != "":
				rep.Drift = append(rep.Drift, Drift{Dependency: d, CurrentHash: h, Problem: problem})
			case h != d.TargetHash:
				rep.Drift = append(rep.Drift, Drift{Dependency: d, CurrentHash: h, Problem: "content changed since binding"})
			}
		}
		return nil
	})
	rep.Fresh = err == nil && len(rep.Drift) == 0
	return rep, err
}

// ---------------------------------------------------------------------------
// Drift sweep (decisions / claims / artifacts that change outside graph patches)
// ---------------------------------------------------------------------------

// Sweep marks CURRENT artifacts STALE when a DECISION, CLAIM or ARTIFACT dependency
// changed or became unusable since binding, across all tenants (run by cmd/scheduler).
func (s *Store) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT ON (a.id) a.id::text, a.tenant_id::text, a.model_id::text, m.project_id::text, v.version_number,
			       CASE d.dep_type WHEN 'DECISION' THEN 'depends on decision ' || d.target_id || ' which changed or needs review'
			                       WHEN 'CLAIM' THEN 'depends on claim ' || d.target_id || ' which changed or is no longer ACTIVE'
			                       ELSE 'upstream artifact ' || d.target_id || ' changed or is stale' END
			FROM artifacts a
			JOIN architecture_models m ON m.id = a.model_id
			JOIN architecture_versions v ON v.id = m.current_version_id
			JOIN artifact_dependencies d ON d.artifact_version_id = a.current_version_id
			LEFT JOIN decisions dd ON d.dep_type = 'DECISION' AND dd.id = d.target_id
			LEFT JOIN claims cc ON d.dep_type = 'CLAIM' AND cc.id = d.target_id
			LEFT JOIN artifacts up ON d.dep_type = 'ARTIFACT' AND up.id = d.target_id
			LEFT JOIN artifact_versions upv ON upv.id = up.current_version_id
			WHERE a.status = 'CURRENT' AND (
			     (d.dep_type = 'DECISION' AND (dd.id IS NULL OR dd.status NOT IN ('PROPOSED', 'ACCEPTED')
			         OR encode(digest(concat_ws('|', dd.id::text, dd.status, dd.chosen_option, dd.rationale, dd.updated_at::text), 'sha256'), 'hex') <> d.target_hash))
			  OR (d.dep_type = 'CLAIM' AND (cc.id IS NULL OR cc.lifecycle <> 'ACTIVE'
			         OR encode(digest(concat_ws('|', cc.id::text, cc.lifecycle, cc.review_status, cc.statement, cc.last_validated_at::text), 'sha256'), 'hex') <> d.target_hash))
			  OR (d.dep_type = 'ARTIFACT' AND (up.id IS NULL OR up.status <> 'CURRENT' OR upv.checksum IS DISTINCT FROM d.target_hash)))
			ORDER BY a.id`)
		if err != nil {
			return err
		}
		type hit struct {
			id, tenant, model, project, why string
			version                         int
		}
		hits, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (hit, error) {
			var h hit
			return h, r.Scan(&h.id, &h.tenant, &h.model, &h.project, &h.version, &h.why)
		})
		if err != nil {
			return err
		}
		byModel := map[string][]hit{}
		for _, h := range hits {
			byModel[h.model] = append(byModel[h.model], h)
		}
		for model, hs := range byModel {
			seeds := map[string]string{}
			for _, h := range hs {
				seeds[h.id] = h.why
			}
			first := hs[0]
			stale, err := graph.MarkArtifactsStale(ctx, tx, model, first.version, seeds, func(id, why string) error {
				return emit(ctx, tx, first.tenant, first.project, "", EventInvalidated, map[string]any{
					"model_id": model, "artifact_id": id, "reason": why, "graph_version": first.version, "cause": "drift-sweep",
				})
			})
			if err != nil {
				return err
			}
			if len(stale) > 0 {
				if _, err := tx.Exec(ctx, `INSERT INTO revalidation_requests (tenant_id, model_id, artifact_ids, graph_version, cause, reason)
					VALUES ($1, $2, $3::uuid[], $4, 'drift-sweep', 'dependencies changed outside graph patches')`,
					first.tenant, model, stale, first.version); err != nil {
					return err
				}
			}
			res.ArtifactsInvalidated += len(stale)
		}
		return nil
	})
	return res, err
}

// ---------------------------------------------------------------------------
// 6.4 Revalidation as runtime tasks
// ---------------------------------------------------------------------------

// PlanSubmitter is the runtime surface used to schedule revalidation tasks.
type PlanSubmitter interface {
	SubmitPlan(ctx context.Context, tenantID, runID string, specs []runtime.TaskSpec) ([]runtime.Task, error)
}

// ScheduleRevalidations turns PENDING revalidation requests into runtime work: one design
// run per request (idempotent on the request id) with an artifact_revalidator task per
// artifact that is still stale. Safe to run in several scheduler replicas.
func (s *Store) ScheduleRevalidations(ctx context.Context, runRepo runs.Repository, planner PlanSubmitter, limit int) (ScheduleResult, error) {
	if limit <= 0 {
		limit = 10
	}
	type request struct {
		id, tenant, model, reason string
		artifacts                 []string
		version, attempts         int
	}
	var reqs []request
	err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE revalidation_requests SET attempts = attempts + 1
			WHERE id IN (SELECT id FROM revalidation_requests WHERE status = 'PENDING' ORDER BY created_at LIMIT $1 FOR UPDATE SKIP LOCKED)
			RETURNING id::text, tenant_id::text, model_id::text, reason, artifact_ids::text[], graph_version, attempts`, limit)
		if err != nil {
			return err
		}
		reqs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (request, error) {
			var q request
			return q, r.Scan(&q.id, &q.tenant, &q.model, &q.reason, &q.artifacts, &q.version, &q.attempts)
		})
		return err
	})
	if err != nil {
		return ScheduleResult{}, err
	}

	var res ScheduleResult
	for _, q := range reqs {
		runID, err := s.scheduleOne(ctx, runRepo, planner, q.id, q.tenant, q.model, q.reason, q.artifacts, q.version)
		if err != nil {
			res.Failed++
			status := "PENDING"
			if q.attempts >= 5 {
				status = "FAILED"
			}
			_ = postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
				_, err2 := tx.Exec(ctx, `UPDATE revalidation_requests SET status = $2, last_error = $3 WHERE id = $1`, q.id, status, err.Error())
				return err2
			})
			continue
		}
		res.Scheduled++
		_ = runID
	}
	return res, nil
}

func (s *Store) scheduleOne(ctx context.Context, runRepo runs.Repository, planner PlanSubmitter,
	reqID, tenantID, modelID, reason string, artifactIDs []string, graphVersion int) (string, error) {
	type art struct{ id, name, typ, version, staleReason string }
	var (
		projectID, creator string
		stale              []art
	)
	if err := postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT p.id::text, p.created_by::text FROM architecture_models m JOIN projects p ON p.id = m.project_id
			WHERE m.id = $1`, modelID).Scan(&projectID, &creator); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id::text, name, projection_type, COALESCE(current_version_id::text, ''), COALESCE(stale_reason, '')
			FROM artifacts WHERE id = ANY($1::uuid[]) AND status = 'STALE' ORDER BY name`, artifactIDs)
		if err != nil {
			return err
		}
		stale, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (art, error) {
			var a art
			return a, r.Scan(&a.id, &a.name, &a.typ, &a.version, &a.staleReason)
		})
		return err
	}); err != nil {
		return "", err
	}
	if len(stale) == 0 { // already revalidated by new versions
		return "", postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE revalidation_requests SET status = 'SCHEDULED', scheduled_at = NOW(), last_error = 'no artifact still stale' WHERE id = $1`, reqID)
			return err
		})
	}

	budget := min(20.0, max(1.0, 0.25*float64(len(stale))))
	run, _, err := runRepo.CreateIdempotent(ctx, runs.DesignRun{
		TenantID: tenantID, ProjectID: projectID, IdempotencyKey: "revalidate:" + reqID, CreatedBy: creator, MaxBudgetUSD: budget,
		Objective:   fmt.Sprintf("Revalidate %d artifact(s) invalidated at graph version %d: %s", len(stale), graphVersion, reason),
		Constraints: map[string]any{"revalidation_request_id": reqID, "model_id": modelID, "graph_version": graphVersion},
	})
	if err != nil {
		return "", fmt.Errorf("create revalidation run: %w", err)
	}
	specs := make([]runtime.TaskSpec, 0, len(stale))
	for _, a := range stale {
		specs = append(specs, runtime.TaskSpec{
			Key:       "artifact-" + a.id,
			AgentType: RevalidatorAgentType,
			Objective: fmt.Sprintf("Revalidate %s artifact %q against graph version %d", a.typ, a.name, graphVersion),
			Input: map[string]any{
				"artifact_id": a.id, "artifact_name": a.name, "projection_type": a.typ, "artifact_version_id": a.version,
				"stale_reason": a.staleReason, "graph_version": graphVersion, "model_id": modelID,
			},
			GrantedCapabilities: []string{"run.read"},
			BudgetUSD:           0.2,
			MaxToolCalls:        1,
		})
	}
	if _, err := planner.SubmitPlan(ctx, tenantID, run.ID, specs); err != nil && !errors.Is(err, scheduler.ErrRunNotPlannable) {
		return "", fmt.Errorf("plan revalidation tasks: %w", err)
	}
	ids := make([]string, len(stale))
	for i, a := range stale {
		ids[i] = a.id
	}
	return run.ID, postgres.WithSystemTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE artifacts SET revalidation_run_id = $2 WHERE id = ANY($1::uuid[]) AND status = 'STALE'`, ids, run.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE revalidation_requests SET status = 'SCHEDULED', run_id = $2, scheduled_at = NOW(), last_error = NULL WHERE id = $1`, reqID, run.ID); err != nil {
			return err
		}
		return emit(ctx, tx, tenantID, projectID, run.ID, EventRevalidationScheduled, map[string]any{
			"model_id": modelID, "request_id": reqID, "run_id": run.ID, "artifact_ids": ids, "graph_version": graphVersion,
		})
	})
}
