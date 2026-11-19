package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/observability"
	"github.com/systera/control-plane/internal/tenancy"
)

// GraphService is the Architecture Intelligence Core surface (implemented by *graph.Service).
type GraphService interface {
	CreateModel(ctx context.Context, tenantID, projectID string, author graph.Author, correlationID string) (graph.Model, graph.Version, error)
	ModelForProject(ctx context.Context, tenantID, projectID string) (graph.Model, error)
	Versions(ctx context.Context, tenantID, modelID string, limit int) ([]graph.Version, error)
	GraphAt(ctx context.Context, tenantID, modelID string, number int) (graph.Graph, error)
	Diff(ctx context.Context, tenantID, modelID string, from, to int) (graph.Diff, error)
	Neighbourhood(ctx context.Context, tenantID, modelID string, number int, center string, q graph.NeighbourhoodQuery) (graph.Neighbourhood, error)
	ApplyPatch(ctx context.Context, tenantID, modelID string, p graph.Patch, author graph.Author, correlationID string) (graph.Result, error)
	GetPatch(ctx context.Context, tenantID, modelID, patchID string) (graph.Result, error)
	XRay(ctx context.Context, tenantID, modelID string, number int, filterLayers []graph.XRayLayer) (graph.XRayView, error)
	XRayDrillDown(ctx context.Context, tenantID, modelID string, number int, centerNodeID string, q graph.XRayDrillDownQuery) (graph.XRayDrillDown, error)
	InspectComponent(ctx context.Context, tenantID, modelID string, number int, nodeID string) (graph.ComponentInspection, error)
}

func (s *Server) registerGraphRoutes() {
	s.mux.Handle("POST /v1/projects/{id}/architecture", s.authenticated(true, s.handleCreateModel))
	s.mux.Handle("GET /v1/projects/{id}/architecture", s.authenticated(false, s.handleGetModel))
	s.mux.Handle("GET /v1/architecture/{model}/versions", s.authenticated(false, s.handleListVersions))
	s.mux.Handle("GET /v1/architecture/{model}/versions/{version}", s.authenticated(false, s.handleGetVersion))
	s.mux.Handle("GET /v1/architecture/{model}/diff", s.authenticated(false, s.handleDiff))
	s.mux.Handle("GET /v1/architecture/{model}/nodes/{node}/neighbourhood", s.authenticated(false, s.handleNeighbourhood))
	s.mux.Handle("GET /v1/architecture/{model}/xray", s.authenticated(false, s.handleXRay))
	s.mux.Handle("GET /v1/architecture/{model}/xray/layers/{layer}", s.authenticated(false, s.handleXRayLayer))
	s.mux.Handle("GET /v1/architecture/{model}/xray/nodes/{node}/drilldown", s.authenticated(false, s.handleXRayDrillDown))
	s.mux.Handle("GET /v1/architecture/{model}/xray/nodes/{node}/inspect", s.authenticated(false, s.handleInspectComponent))
	s.mux.Handle("POST /v1/architecture/{model}/patches", s.authenticated(true, s.handleSubmitPatch))
	s.mux.Handle("GET /v1/architecture/{model}/patches/{patch}", s.authenticated(false, s.handleGetPatch))
}

func author(p tenancy.Principal) graph.Author { return graph.Author{Type: "USER", ID: p.UserID} }

// graphError maps graph errors to HTTP responses; it returns false for unexpected errors.
func (s *Server) graphError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, graph.ErrModelNotFound), errors.Is(err, graph.ErrProjectNotFound):
		writeError(w, http.StatusNotFound, "not_found", "architecture model not found")
	case errors.Is(err, graph.ErrVersionNotFound):
		writeError(w, http.StatusNotFound, "not_found", "graph version not found")
	case errors.Is(err, graph.ErrNodeNotFound):
		writeError(w, http.StatusNotFound, "not_found", "node not found at this version")
	case errors.Is(err, graph.ErrPatchNotFound):
		writeError(w, http.StatusNotFound, "not_found", "graph patch not found")
	case errors.Is(err, graph.ErrModelExists):
		writeError(w, http.StatusConflict, "model_exists", err.Error())
	case errors.Is(err, graph.ErrInvalidPatch), errors.Is(err, graph.ErrInvalidQuery):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, graph.ErrIdempotencyConflict):
		writeError(w, http.StatusUnprocessableEntity, "idempotency_key_conflict", err.Error())
	default:
		s.internalError(w, r, op, err)
	}
}

func (s *Server) handleCreateModel(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	m, v, err := s.opts.Graph.CreateModel(r.Context(), p.TenantID, r.PathValue("id"), author(p), observability.CorrelationIDFromContext(r.Context()))
	if err != nil {
		s.graphError(w, r, "create architecture model", err)
		return
	}
	w.Header().Set("Location", "/v1/projects/"+m.ProjectID+"/architecture")
	writeJSON(w, http.StatusCreated, map[string]any{"model": m, "version": v})
}

func (s *Server) handleGetModel(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	m, err := s.opts.Graph.ModelForProject(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		s.graphError(w, r, "get architecture model", err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	limit, err := intParam(r, "limit", 100)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	vs, err := s.opts.Graph.Versions(r.Context(), p.TenantID, r.PathValue("model"), limit)
	if err != nil {
		s.graphError(w, r, "list versions", err)
		return
	}
	if len(vs) == 0 {
		// An unknown (or foreign) model has no versions; every real model has version 0.
		writeError(w, http.StatusNotFound, "not_found", "architecture model not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": vs})
}

// versionParam parses "current" (-1) or a non-negative integer.
func versionParam(raw string) (int, error) {
	if raw == "" || raw == "current" {
		return -1, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, errors.New(`version must be "current" or a non-negative integer`)
	}
	return n, nil
}

func intParam(r *http.Request, name string, def int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New(name + " must be an integer")
	}
	return n, nil
}

func (s *Server) handleGetVersion(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	n, err := versionParam(r.PathValue("version"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	g, err := s.opts.Graph.GraphAt(r.Context(), p.TenantID, r.PathValue("model"), n)
	if err != nil {
		s.graphError(w, r, "get graph version", err)
		return
	}
	w.Header().Set("ETag", `"`+g.Version.GraphHash+`"`)
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	from, err := versionParam(r.URL.Query().Get("from"))
	if err == nil && r.URL.Query().Get("from") == "" {
		err = errors.New("from is required")
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "from: "+err.Error())
		return
	}
	to, err := versionParam(r.URL.Query().Get("to"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "to: "+err.Error())
		return
	}
	d, err := s.opts.Graph.Diff(r.Context(), p.TenantID, r.PathValue("model"), from, to)
	if err != nil {
		s.graphError(w, r, "diff versions", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleNeighbourhood(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	q := r.URL.Query()
	n, err := versionParam(q.Get("version"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	depth, err := intParam(r, "depth", 1)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	maxNodes, err := intParam(r, "max_nodes", 200)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var relations []string
	if rel := q.Get("relation"); rel != "" {
		relations = strings.Split(rel, ",")
	}
	res, err := s.opts.Graph.Neighbourhood(r.Context(), p.TenantID, r.PathValue("model"), n, r.PathValue("node"), graph.NeighbourhoodQuery{
		Direction: graph.Direction(q.Get("direction")), RelationTypes: relations, Depth: depth, MaxNodes: maxNodes,
	})
	if err != nil {
		s.graphError(w, r, "neighbourhood query", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleSubmitPatch(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "patch body exceeds 4 MiB")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var patch graph.Patch
	if err := dec.Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid GraphPatch: "+err.Error())
		return
	}
	if hk := r.Header.Get("Idempotency-Key"); hk != "" {
		if patch.IdempotencyKey != "" && patch.IdempotencyKey != hk {
			writeError(w, http.StatusBadRequest, "invalid_request", "Idempotency-Key header and idempotency_key differ")
			return
		}
		patch.IdempotencyKey = hk
	}

	res, err := s.opts.Graph.ApplyPatch(r.Context(), p.TenantID, r.PathValue("model"), patch, author(p), observability.CorrelationIDFromContext(r.Context()))
	if err != nil {
		s.graphError(w, r, "apply graph patch", err)
		return
	}
	if res.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	code := http.StatusCreated
	switch res.Status {
	case graph.PatchConflict:
		code = http.StatusConflict
	case graph.PatchRejected:
		code = http.StatusUnprocessableEntity
	default:
		w.Header().Set("Location", "/v1/architecture/"+res.ModelID+"/versions/"+strconv.Itoa(*res.NewGraphVersion))
	}
	writeJSON(w, code, res)
}

func (s *Server) handleGetPatch(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	res, err := s.opts.Graph.GetPatch(r.Context(), p.TenantID, r.PathValue("model"), r.PathValue("patch"))
	if err != nil {
		s.graphError(w, r, "get graph patch", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func parseXRayLayersParam(raw string) ([]graph.XRayLayer, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]graph.XRayLayer, 0, len(parts))
	for _, part := range parts {
		l, ok := graph.ParseXRayLayer(part)
		if !ok {
			return nil, errors.New("unknown X-Ray layer: " + strings.TrimSpace(part))
		}
		out = append(out, l)
	}
	return out, nil
}

func (s *Server) handleXRay(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	n, err := versionParam(r.URL.Query().Get("version"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	layers, err := parseXRayLayersParam(r.URL.Query().Get("layers"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	view, err := s.opts.Graph.XRay(r.Context(), p.TenantID, r.PathValue("model"), n, layers)
	if err != nil {
		s.graphError(w, r, "xray view", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleXRayLayer(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	n, err := versionParam(r.URL.Query().Get("version"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	layer, ok := graph.ParseXRayLayer(r.PathValue("layer"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "unknown X-Ray layer: "+r.PathValue("layer"))
		return
	}
	view, err := s.opts.Graph.XRay(r.Context(), p.TenantID, r.PathValue("model"), n, []graph.XRayLayer{layer})
	if err != nil {
		s.graphError(w, r, "xray layer view", err)
		return
	}
	if len(view.Layers) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "layer not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model_id":   view.ModelID,
		"version":    view.Version,
		"graph_hash": view.GraphHash,
		"layer":      view.Layers[0],
	})
}

func (s *Server) handleXRayDrillDown(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	q := r.URL.Query()
	n, err := versionParam(q.Get("version"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	depth, err := intParam(r, "depth", 2)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	maxNodes, err := intParam(r, "max_nodes", 200)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	targetLayers, err := parseXRayLayersParam(q.Get("target_layers"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var relations []string
	if rel := q.Get("relation"); rel != "" {
		relations = strings.Split(rel, ",")
	}
	res, err := s.opts.Graph.XRayDrillDown(r.Context(), p.TenantID, r.PathValue("model"), n, r.PathValue("node"), graph.XRayDrillDownQuery{
		Direction:     graph.Direction(q.Get("direction")),
		RelationTypes: relations,
		TargetLayers:  targetLayers,
		Depth:         depth,
		MaxNodes:      maxNodes,
	})
	if err != nil {
		s.graphError(w, r, "xray drill-down", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleInspectComponent(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	n, err := versionParam(r.URL.Query().Get("version"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	insp, err := s.opts.Graph.InspectComponent(r.Context(), p.TenantID, r.PathValue("model"), n, r.PathValue("node"))
	if err != nil {
		s.graphError(w, r, "inspect component", err)
		return
	}
	writeJSON(w, http.StatusOK, insp)
}
