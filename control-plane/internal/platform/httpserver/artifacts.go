package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/systera/control-plane/internal/artifacts"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/tenancy"
)

// ArtifactService is the dependency engine's artifact side (implemented by *artifacts.Store).
type ArtifactService interface {
	Record(ctx context.Context, tenantID, modelID, artifactID string, in artifacts.RecordInput, a artifacts.Actor) (artifacts.RecordResult, error)
	Get(ctx context.Context, tenantID, modelID, id string) (artifacts.Artifact, error)
	List(ctx context.Context, tenantID, modelID, status string) ([]artifacts.Artifact, error)
	Versions(ctx context.Context, tenantID, modelID, id string) ([]artifacts.Version, error)
	Verify(ctx context.Context, tenantID, modelID, id string) (artifacts.VerifyReport, error)
}

// ImpactService runs dry-run impact analysis (implemented by *graph.Store).
type ImpactService interface {
	ImpactOf(ctx context.Context, tenantID, modelID string, number int, nodeIDs []string, lim graph.ImpactLimits) (graph.ImpactReport, error)
}

func (s *Server) registerArtifactRoutes() {
	base := "/v1/architecture/{model}"
	s.mux.Handle("POST "+base+"/artifacts", s.authenticated(true, s.handleCreateArtifact))
	s.mux.Handle("GET "+base+"/artifacts", s.authenticated(false, s.handleListArtifacts))
	s.mux.Handle("GET "+base+"/artifacts/{id}", s.authenticated(false, s.handleGetArtifact))
	s.mux.Handle("POST "+base+"/artifacts/{id}/versions", s.authenticated(true, s.handleRecordArtifactVersion))
	s.mux.Handle("GET "+base+"/artifacts/{id}/versions", s.authenticated(false, s.handleListArtifactVersions))
	s.mux.Handle("GET "+base+"/artifacts/{id}/verify", s.authenticated(false, s.handleVerifyArtifact))
	if s.opts.Impact != nil {
		s.mux.Handle("POST "+base+"/impact", s.authenticated(false, s.handleImpact))
		s.mux.Handle("GET "+base+"/nodes/{node}/impact", s.authenticated(false, s.handleNodeImpact))
	}
	if s.opts.Projector != nil && s.opts.Graph != nil {
		s.mux.Handle("POST "+base+"/project", s.authenticated(true, s.handleProjectArtifact))
		s.mux.Handle("POST "+base+"/project/reward", s.authenticated(true, s.handleProjectReward))
	}
}

func (s *Server) artifactError(w http.ResponseWriter, r *http.Request, op string, err error) {
	var ve *artifacts.ValidationError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": ve.Error(), "code": "validation_failed", "findings": ve.Findings})
	case errors.Is(err, artifacts.ErrModelNotFound), errors.Is(err, graph.ErrModelNotFound), errors.Is(err, graph.ErrVersionNotFound):
		writeError(w, http.StatusNotFound, "not_found", "architecture model not found")
	case errors.Is(err, artifacts.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "artifact not found")
	case errors.Is(err, graph.ErrNodeNotFound):
		writeError(w, http.StatusNotFound, "not_found", "node not found at this version")
	case errors.Is(err, artifacts.ErrStaleGraphVersion):
		writeError(w, http.StatusConflict, "stale_graph_version", err.Error())
	case errors.Is(err, artifacts.ErrExists):
		writeError(w, http.StatusConflict, "artifact_exists", err.Error())
	case errors.Is(err, artifacts.ErrRetired):
		writeError(w, http.StatusConflict, "artifact_retired", err.Error())
	case errors.Is(err, artifacts.ErrInvalidInput), errors.Is(err, graph.ErrInvalidQuery):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		s.internalError(w, r, op, err)
	}
}

func artifactActor(p tenancy.Principal) artifacts.Actor {
	return artifacts.Actor{Type: "USER", ID: p.UserID}
}

func (s *Server) handleCreateArtifact(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in artifacts.RecordInput
	if !decodeJSON(w, r, &in) {
		return
	}
	res, err := s.opts.Artifacts.Record(r.Context(), p.TenantID, r.PathValue("model"), "", in, artifactActor(p))
	if err != nil {
		s.artifactError(w, r, "create artifact", err)
		return
	}
	w.Header().Set("Location", "/v1/architecture/"+res.Artifact.ModelID+"/artifacts/"+res.Artifact.ID)
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) handleRecordArtifactVersion(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in artifacts.RecordInput
	if !decodeJSON(w, r, &in) {
		return
	}
	res, err := s.opts.Artifacts.Record(r.Context(), p.TenantID, r.PathValue("model"), r.PathValue("id"), in, artifactActor(p))
	if err != nil {
		s.artifactError(w, r, "record artifact version", err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	list, err := s.opts.Artifacts.List(r.Context(), p.TenantID, r.PathValue("model"), r.URL.Query().Get("status"))
	if err != nil {
		s.artifactError(w, r, "list artifacts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	a, err := s.opts.Artifacts.Get(r.Context(), p.TenantID, r.PathValue("model"), r.PathValue("id"))
	if err != nil {
		s.artifactError(w, r, "get artifact", err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleListArtifactVersions(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	vs, err := s.opts.Artifacts.Versions(r.Context(), p.TenantID, r.PathValue("model"), r.PathValue("id"))
	if err != nil {
		s.artifactError(w, r, "list artifact versions", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": vs})
}

func (s *Server) handleVerifyArtifact(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	rep, err := s.opts.Artifacts.Verify(r.Context(), p.TenantID, r.PathValue("model"), r.PathValue("id"))
	if err != nil {
		s.artifactError(w, r, "verify artifact", err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

type impactRequest struct {
	NodeIDs       []string `json:"node_ids"`
	Version       string   `json:"version,omitempty"`
	MaxDepth      int      `json:"max_depth,omitempty"`
	MaxNodes      int      `json:"max_nodes,omitempty"`
	RelationTypes []string `json:"relation_types,omitempty"`
}

func (s *Server) runImpact(w http.ResponseWriter, r *http.Request, p tenancy.Principal, in impactRequest) {
	n, err := versionParam(in.Version)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	rep, err := s.opts.Impact.ImpactOf(r.Context(), p.TenantID, r.PathValue("model"), n, in.NodeIDs,
		graph.ImpactLimits{MaxDepth: in.MaxDepth, MaxNodes: in.MaxNodes, RelationTypes: in.RelationTypes})
	if err != nil {
		s.artifactError(w, r, "impact analysis", err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleImpact(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in impactRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	s.runImpact(w, r, p, in)
}

func (s *Server) handleNodeImpact(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	q := r.URL.Query()
	depth, err := intParam(r, "max_depth", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	nodes, err := intParam(r, "max_nodes", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var rel []string
	if v := q.Get("relation"); v != "" {
		rel = strings.Split(v, ",")
	}
	s.runImpact(w, r, p, impactRequest{NodeIDs: []string{r.PathValue("node")}, Version: q.Get("version"), MaxDepth: depth, MaxNodes: nodes, RelationTypes: rel})
}

func (s *Server) handleProjectArtifact(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in struct {
		ProjectionType artifacts.ProjectionType `json:"projection_type"`
		Version        *int                     `json:"version,omitempty"`
		Async          bool                     `json:"async,omitempty"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	ver := -1
	if in.Version != nil {
		ver = *in.Version
	}
	modelID := r.PathValue("model")
	if in.Async && s.opts.AsyncProjector != nil {
		queued := s.opts.AsyncProjector.Enqueue(artifacts.ProjectionJob{
			TenantID: p.TenantID, ModelID: modelID, ProjectionType: in.ProjectionType, Version: ver,
			Actor: artifacts.Actor{Type: "USER", ID: p.UserID},
		})
		if !queued {
			writeError(w, http.StatusServiceUnavailable, "queue_full", "async projection queue is full")
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "QUEUED", "model_id": modelID, "projection_type": in.ProjectionType})
		return
	}
	if s.opts.AsyncProjector != nil {
		res, err := s.opts.AsyncProjector.ProjectAndPersist(r.Context(), p.TenantID, modelID, in.ProjectionType, ver, artifacts.Actor{Type: "USER", ID: p.UserID})
		if err != nil {
			s.artifactError(w, r, "project and persist artifact", err)
			return
		}
		writeJSON(w, http.StatusCreated, res)
		return
	}
	g, err := s.opts.Graph.GraphAt(r.Context(), p.TenantID, modelID, ver)
	if err != nil {
		s.artifactError(w, r, "load graph for projection", err)
		return
	}
	art, err := s.opts.Projector.Generate(g, in.ProjectionType)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, art)
}

func (s *Server) handleProjectReward(w http.ResponseWriter, r *http.Request, _ tenancy.Principal) {
	var sig artifacts.RewardSignal
	if !decodeJSON(w, r, &sig) {
		return
	}
	stats := s.opts.Projector.RecordReward(sig)
	writeJSON(w, http.StatusOK, stats)
}
