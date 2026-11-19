package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/systera/control-plane/internal/counterfactual"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/tenancy"
)

// CounterfactualService manages Architecture Counterfactual alternatives, decision matrices,
// ADR recording, and canonical graph-version diffs (implemented by *counterfactual.Store).
type CounterfactualService interface {
	GenerateForModel(ctx context.Context, tenantID, modelID string, in counterfactual.GenerateInput, actorID string) (counterfactual.CounterfactualSet, error)
	GenerateForWebsite(ctx context.Context, tenantID, websiteID, modelID, domain string, in counterfactual.GenerateInput, actorID string) (counterfactual.CounterfactualSet, error)
	ListForModel(ctx context.Context, tenantID, modelID string) (counterfactual.CounterfactualSet, error)
	ListForWebsite(ctx context.Context, tenantID, websiteID, domain string) (counterfactual.CounterfactualSet, error)
	DiffAlternatives(ctx context.Context, tenantID, altAID, altBID string) (counterfactual.AlternativeDiff, error)
}

func (s *Server) registerCounterfactualRoutes() {
	s.mux.Handle("GET /v1/architecture/{model}/alternatives", s.authenticated(false, s.handleListModelAlternatives))
	s.mux.Handle("POST /v1/architecture/{model}/alternatives", s.authenticated(true, s.handleGenerateModelAlternatives))
	s.mux.Handle("GET /v1/architecture/{model}/alternatives/diff", s.authenticated(false, s.handleDiffAlternatives))
}

func (s *Server) counterfactualError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, counterfactual.ErrNotFound), errors.Is(err, graph.ErrModelNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, counterfactual.ErrInvalidInput), errors.Is(err, counterfactual.ErrNoModel):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		s.internalError(w, r, op, err)
	}
}

func (s *Server) handleListModelAlternatives(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	set, err := s.opts.Counterfactual.ListForModel(r.Context(), p.TenantID, r.PathValue("model"))
	if err != nil {
		s.counterfactualError(w, r, "list model alternatives", err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

func (s *Server) handleGenerateModelAlternatives(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in counterfactual.GenerateInput
	if r.ContentLength > 0 && !decodeJSON(w, r, &in) {
		return
	}
	set, err := s.opts.Counterfactual.GenerateForModel(r.Context(), p.TenantID, r.PathValue("model"), in, p.UserID)
	if err != nil {
		s.counterfactualError(w, r, "generate model alternatives", err)
		return
	}
	writeJSON(w, http.StatusCreated, set)
}

func (s *Server) handleDiffAlternatives(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	fromID := r.URL.Query().Get("from")
	toID := r.URL.Query().Get("to")
	if fromID == "" || toID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "query parameters from and to (alternative IDs) are required")
		return
	}
	diff, err := s.opts.Counterfactual.DiffAlternatives(r.Context(), p.TenantID, fromID, toID)
	if err != nil {
		s.counterfactualError(w, r, "diff alternatives", err)
		return
	}
	writeJSON(w, http.StatusOK, diff)
}
