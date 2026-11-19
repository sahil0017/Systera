package artifacts

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/systera/control-plane/internal/evaluation"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/objectstore"
)

// ProjectionJob describes an asynchronous projection + validation + persistence job.
type ProjectionJob struct {
	TenantID       string
	ModelID        string
	ProjectionType ProjectionType
	Version        int
	Actor          Actor
}

// PersistedProjection bundles the generated artifact, validation result, and S3 content_ref.
type PersistedProjection struct {
	Projected  ProjectedArtifact           `json:"projected"`
	Validation evaluation.ValidationResult `json:"validation"`
	ContentRef string                      `json:"content_ref"`
	ArtifactID string                      `json:"artifact_id,omitempty"`
}

// AsyncProjector generates projections via RLPolicy, validates them deterministically,
// uploads immutable blobs to object storage, and records versioned bindings in artifacts.Store.
type AsyncProjector struct {
	projector *Projector
	objects   objectstore.Store
	store     *Store
	graphSvc  *graph.Service
	logger    *slog.Logger
	queue     chan ProjectionJob
}

func NewAsyncProjector(
	projector *Projector,
	objects objectstore.Store,
	store *Store,
	graphSvc *graph.Service,
	logger *slog.Logger,
) *AsyncProjector {
	if projector == nil {
		projector = NewProjector(nil)
	}
	if objects == nil {
		objects = objectstore.NewMemoryStore("systera-artifacts")
	}
	return &AsyncProjector{
		projector: projector,
		objects:   objects,
		store:     store,
		graphSvc:  graphSvc,
		logger:    logger,
		queue:     make(chan ProjectionJob, 128),
	}
}

// Run processes queued asynchronous projection jobs until ctx is cancelled.
func (a *AsyncProjector) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-a.queue:
			if _, err := a.ProjectAndPersist(ctx, job.TenantID, job.ModelID, job.ProjectionType, job.Version, job.Actor); err != nil && a.logger != nil {
				a.logger.Error("async projection failed", "model_id", job.ModelID, "type", job.ProjectionType, "error", err)
			}
		}
	}
}

// Enqueue schedules an asynchronous projection job.
func (a *AsyncProjector) Enqueue(job ProjectionJob) bool {
	select {
	case a.queue <- job:
		return true
	default:
		return false
	}
}

// ProjectAndPersist generates, validates, uploads to S3/MinIO, and records the projection.
func (a *AsyncProjector) ProjectAndPersist(
	ctx context.Context,
	tenantID, modelID string,
	pt ProjectionType,
	version int,
	actor Actor,
) (PersistedProjection, error) {
	if a.graphSvc == nil {
		return PersistedProjection{}, fmt.Errorf("graph service is not configured")
	}
	g, err := a.graphSvc.GraphAt(ctx, tenantID, modelID, version)
	if err != nil {
		return PersistedProjection{}, err
	}
	return a.PersistFromGraph(ctx, tenantID, g, pt, actor)
}

// PersistFromGraph runs generation, deterministic validation, object upload, and optional DB recording.
func (a *AsyncProjector) PersistFromGraph(
	ctx context.Context,
	tenantID string,
	g graph.Graph,
	pt ProjectionType,
	actor Actor,
) (PersistedProjection, error) {
	art, err := a.projector.Generate(g, pt)
	if err != nil {
		return PersistedProjection{}, err
	}
	val := evaluation.ValidateProjection(string(pt), art.Content)
	if !val.Valid {
		return PersistedProjection{Projected: art, Validation: val}, fmt.Errorf("projection validation failed: %s", strings.Join(val.Errors, "; "))
	}

	ext, mediaType := projectionFormat(pt)
	key := fmt.Sprintf("%s/%s/v%d/%s-%s.%s", tenantID, g.Version.ModelID, g.Version.Number, strings.ToLower(string(pt)), art.ContentHash[:12], ext)
	ref, err := a.objects.PutObject(ctx, key, mediaType, []byte(art.Content))
	if err != nil {
		return PersistedProjection{}, err
	}

	out := PersistedProjection{
		Projected:  art,
		Validation: val,
		ContentRef: ref,
	}

	if a.store != nil {
		nodeDeps := make([]NodeDependency, 0, len(g.Nodes))
		for _, n := range g.Nodes {
			nodeDeps = append(nodeDeps, NodeDependency{NodeID: n.ID})
		}
		gv := g.Version.Number
		rec, err := a.store.Record(ctx, tenantID, g.Version.ModelID, "", RecordInput{
			Name:           fmt.Sprintf("%s (v%d)", strings.ToLower(string(pt)), gv),
			ProjectionType: string(pt),
			Checksum:       art.ContentHash,
			ContentRef:     ref,
			MediaType:      mediaType,
			SizeBytes:      int64(len(art.Content)),
			GraphVersion:   &gv,
			Dependencies:   Dependencies{Nodes: nodeDeps},
		}, actor)
		if err != nil {
			return out, err
		}
		out.ArtifactID = rec.Artifact.ID
	}
	return out, nil
}

func projectionFormat(pt ProjectionType) (ext, mediaType string) {
	switch pt {
	case ProjectionSQL:
		return "sql", "application/sql"
	case ProjectionOpenAPI:
		return "yaml", "application/yaml"
	case ProjectionMermaid:
		return "mmd", "text/vnd.mermaid"
	default:
		return "md", "text/markdown"
	}
}
