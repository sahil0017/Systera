package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/systera/control-plane/internal/decisions"
	"github.com/systera/control-plane/internal/tenancy"
)

// DecisionService is the ADR ledger (implemented by *decisions.Store).
type DecisionService interface {
	Create(ctx context.Context, tenantID, modelID string, in decisions.CreateInput, actor decisions.Actor) (decisions.Decision, error)
	Get(ctx context.Context, tenantID, modelID, id string) (decisions.Decision, error)
	List(ctx context.Context, tenantID, modelID string, status decisions.Status) ([]decisions.Decision, error)
	Transition(ctx context.Context, tenantID, modelID, id string, in decisions.TransitionInput, actor decisions.Actor) (decisions.Decision, error)
	Supersede(ctx context.Context, tenantID, modelID, oldID string, in decisions.CreateInput, actor decisions.Actor) (decisions.Decision, decisions.Decision, error)
}

func (s *Server) registerDecisionRoutes() {
	base := "/v1/architecture/{model}/decisions"
	s.mux.Handle("POST "+base, s.authenticated(true, s.handleCreateDecision))
	s.mux.Handle("GET "+base, s.authenticated(false, s.handleListDecisions))
	s.mux.Handle("GET "+base+"/{id}", s.authenticated(false, s.handleGetDecision))
	s.mux.Handle("POST "+base+"/{id}/transition", s.authenticated(true, s.handleTransitionDecision))
	s.mux.Handle("POST "+base+"/{id}/supersede", s.authenticated(true, s.handleSupersedeDecision))
}

func decisionActor(p tenancy.Principal) decisions.Actor {
	return decisions.Actor{Type: "USER", ID: p.UserID}
}

func (s *Server) decisionError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, decisions.ErrModelNotFound):
		writeError(w, http.StatusNotFound, "not_found", "architecture model not found")
	case errors.Is(err, decisions.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "decision not found")
	case errors.Is(err, decisions.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, decisions.ErrInvalidTransition):
		writeError(w, http.StatusConflict, "invalid_transition", err.Error())
	default:
		s.internalError(w, r, op, err)
	}
}

func (s *Server) handleCreateDecision(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in decisions.CreateInput
	if !decodeJSON(w, r, &in) {
		return
	}
	d, err := s.opts.Decisions.Create(r.Context(), p.TenantID, r.PathValue("model"), in, decisionActor(p))
	if err != nil {
		s.decisionError(w, r, "create decision", err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) handleListDecisions(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	status := decisions.Status(r.URL.Query().Get("status"))
	if status != "" && !decisions.ValidStatus(status) {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid status filter")
		return
	}
	list, err := s.opts.Decisions.List(r.Context(), p.TenantID, r.PathValue("model"), status)
	if err != nil {
		s.decisionError(w, r, "list decisions", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) handleGetDecision(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	d, err := s.opts.Decisions.Get(r.Context(), p.TenantID, r.PathValue("model"), r.PathValue("id"))
	if err != nil {
		s.decisionError(w, r, "get decision", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleTransitionDecision(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in decisions.TransitionInput
	if !decodeJSON(w, r, &in) {
		return
	}
	d, err := s.opts.Decisions.Transition(r.Context(), p.TenantID, r.PathValue("model"), r.PathValue("id"), in, decisionActor(p))
	if err != nil {
		s.decisionError(w, r, "transition decision", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleSupersedeDecision(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in decisions.CreateInput
	if !decodeJSON(w, r, &in) {
		return
	}
	oldDec, newDec, err := s.opts.Decisions.Supersede(r.Context(), p.TenantID, r.PathValue("model"), r.PathValue("id"), in, decisionActor(p))
	if err != nil {
		s.decisionError(w, r, "supersede decision", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"superseded": oldDec,
		"replacement": newDec,
	})
}
