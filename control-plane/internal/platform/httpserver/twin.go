package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/systera/control-plane/internal/tenancy"
	"github.com/systera/control-plane/internal/twin"
)

// TwinService builds and serves design-time Digital Twins (implemented by *twin.Store).
type TwinService interface {
	BuildTwin(ctx context.Context, tenantID, modelID string, version int, a twin.Assumptions, actorID string) (twin.BuildRecord, bool, error)
	Preview(ctx context.Context, tenantID, modelID string, version int, a twin.Assumptions) (twin.BuildRecord, error)
	List(ctx context.Context, tenantID, modelID string, limit int) ([]twin.BuildRecord, error)
	Get(ctx context.Context, tenantID, id string) (twin.BuildRecord, error)
}

func (s *Server) registerTwinRoutes() {
	s.mux.Handle("POST /v1/architecture/{model}/twins", s.authenticated(true, s.handleBuildTwin))
	s.mux.Handle("GET /v1/architecture/{model}/twins", s.authenticated(false, s.handleListTwins))
	s.mux.Handle("GET /v1/twins/{id}", s.authenticated(false, s.handleGetTwin))
}

func (s *Server) twinError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, twin.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, twin.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "twin build not found")
	default:
		s.graphError(w, r, op, err)
	}
}

// handleBuildTwin builds a twin for a graph version. Builds are deterministic and fast, so
// the result is returned inline: 201 for a new build, 200 when an identical build (same
// graph version, assumptions and builder version) already exists. dry_run validates
// without recording.
func (s *Server) handleBuildTwin(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in struct {
		Version     string           `json:"version,omitempty"`
		Assumptions twin.Assumptions `json:"assumptions"`
		DryRun      bool             `json:"dry_run,omitempty"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	n, err := versionParam(in.Version)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	model := r.PathValue("model")
	if in.DryRun {
		rec, err := s.opts.Twins.Preview(r.Context(), p.TenantID, model, n, in.Assumptions)
		if err != nil {
			s.twinError(w, r, "preview twin", err)
			return
		}
		writeJSON(w, http.StatusOK, rec)
		return
	}
	rec, created, err := s.opts.Twins.BuildTwin(r.Context(), p.TenantID, model, n, in.Assumptions, p.UserID)
	if err != nil {
		s.twinError(w, r, "build twin", err)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	w.Header().Set("Location", "/v1/twins/"+rec.ID)
	writeJSON(w, code, rec)
}

func (s *Server) handleListTwins(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	limit, err := intParam(r, "limit", 20)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	items, err := s.opts.Twins.List(r.Context(), p.TenantID, r.PathValue("model"), limit)
	if err != nil {
		s.twinError(w, r, "list twins", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleGetTwin(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	rec, err := s.opts.Twins.Get(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		s.twinError(w, r, "get twin", err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}
