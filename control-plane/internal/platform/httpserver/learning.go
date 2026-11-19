package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/systera/control-plane/internal/learning"
	"github.com/systera/control-plane/internal/tenancy"
)

// LearningService serves Engineering Learning Mode (implemented by *learning.Store).
type LearningService interface {
	Overview(ctx context.Context, tenantID, modelID string, version int) (learning.Overview, error)
	Lesson(ctx context.Context, tenantID, modelID string, version int, nodeID string) (learning.Lesson, error)
	Challenge(ctx context.Context, tenantID, modelID string, version int, nodeID, text, actorID string) (learning.Challenge, error)
	ListChallenges(ctx context.Context, tenantID, modelID, nodeID string) ([]learning.Challenge, error)
}

func (s *Server) registerLearningRoutes() {
	s.mux.Handle("GET /v1/architecture/{model}/learning", s.authenticated(false, s.handleLearningOverview))
	s.mux.Handle("GET /v1/architecture/{model}/learning/nodes/{node}", s.authenticated(false, s.handleLesson))
	s.mux.Handle("GET /v1/architecture/{model}/learning/nodes/{node}/challenges", s.authenticated(false, s.handleListChallenges))
	s.mux.Handle("POST /v1/architecture/{model}/learning/nodes/{node}/challenges", s.authenticated(true, s.handleChallenge))
}

func (s *Server) learningError(w http.ResponseWriter, r *http.Request, op string, err error) {
	if errors.Is(err, learning.ErrInvalidInput) {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	s.graphError(w, r, op, err)
}

func (s *Server) handleLearningOverview(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	n, err := versionParam(r.URL.Query().Get("version"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ov, err := s.opts.Learning.Overview(r.Context(), p.TenantID, r.PathValue("model"), n)
	if err != nil {
		s.learningError(w, r, "learning overview", err)
		return
	}
	writeJSON(w, http.StatusOK, ov)
}

func (s *Server) handleLesson(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	n, err := versionParam(r.URL.Query().Get("version"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	l, err := s.opts.Learning.Lesson(r.Context(), p.TenantID, r.PathValue("model"), n, r.PathValue("node"))
	if err != nil {
		s.learningError(w, r, "lesson", err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) handleListChallenges(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	cs, err := s.opts.Learning.ListChallenges(r.Context(), p.TenantID, r.PathValue("model"), r.PathValue("node"))
	if err != nil {
		s.learningError(w, r, "list challenges", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": cs})
}

func (s *Server) handleChallenge(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in struct {
		Challenge string `json:"challenge"`
		Version   string `json:"version,omitempty"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	n, err := versionParam(in.Version)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	c, err := s.opts.Learning.Challenge(r.Context(), p.TenantID, r.PathValue("model"), n, r.PathValue("node"), in.Challenge, p.UserID)
	if err != nil {
		s.learningError(w, r, "challenge", err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}
