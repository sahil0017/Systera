package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/systera/control-plane/internal/evidence"
	"github.com/systera/control-plane/internal/tenancy"
)

// EvidenceService is the evidence ledger (implemented by *evidence.Store).
type EvidenceService interface {
	CaptureEvidence(ctx context.Context, tenantID, modelID string, in evidence.EvidenceInput, a evidence.Actor) (evidence.Evidence, bool, error)
	GetEvidence(ctx context.Context, tenantID, modelID, id string) (evidence.Evidence, error)
	ListEvidence(ctx context.Context, tenantID, modelID, status string, limit int) ([]evidence.Evidence, error)
	RevokeEvidence(ctx context.Context, tenantID, modelID, id, reason string, a evidence.Actor) (evidence.Evidence, int, error)
	RecordAssumption(ctx context.Context, tenantID, modelID string, in evidence.AssumptionInput, a evidence.Actor) (evidence.Assumption, error)
	SetAssumptionStatus(ctx context.Context, tenantID, modelID, id string, in evidence.AssumptionStatusInput, a evidence.Actor) (evidence.Assumption, int, error)
	ListAssumptions(ctx context.Context, tenantID, modelID, status string) ([]evidence.Assumption, error)
	MakeClaim(ctx context.Context, tenantID, modelID string, in evidence.ClaimInput, a evidence.Actor) (evidence.Claim, error)
	GetClaim(ctx context.Context, tenantID, modelID, id string) (evidence.Claim, error)
	ListClaims(ctx context.Context, tenantID, modelID string, f evidence.ClaimFilter) ([]evidence.Claim, error)
	ReviewClaim(ctx context.Context, tenantID, modelID, id string, in evidence.ReviewInput, a evidence.Actor) (evidence.Claim, error)
	RevalidateClaim(ctx context.Context, tenantID, modelID, id string, in evidence.RevalidateInput, a evidence.Actor) (evidence.Claim, error)
	RetractClaim(ctx context.Context, tenantID, modelID, id, reason string, a evidence.Actor) (evidence.Claim, error)
}

func (s *Server) registerEvidenceRoutes() {
	base := "/v1/architecture/{model}"
	s.mux.Handle("POST "+base+"/evidence", s.authenticated(true, s.handleCaptureEvidence))
	s.mux.Handle("GET "+base+"/evidence", s.authenticated(false, s.handleListEvidence))
	s.mux.Handle("GET "+base+"/evidence/{id}", s.authenticated(false, s.handleGetEvidence))
	s.mux.Handle("POST "+base+"/evidence/{id}/revoke", s.authenticated(true, s.handleRevokeEvidence))
	s.mux.Handle("POST "+base+"/assumptions", s.authenticated(true, s.handleRecordAssumption))
	s.mux.Handle("GET "+base+"/assumptions", s.authenticated(false, s.handleListAssumptions))
	s.mux.Handle("POST "+base+"/assumptions/{id}/status", s.authenticated(true, s.handleAssumptionStatus))
	s.mux.Handle("POST "+base+"/claims", s.authenticated(true, s.handleMakeClaim))
	s.mux.Handle("GET "+base+"/claims", s.authenticated(false, s.handleListClaims))
	s.mux.Handle("GET "+base+"/claims/{id}", s.authenticated(false, s.handleGetClaim))
	s.mux.Handle("POST "+base+"/claims/{id}/review", s.authenticated(true, s.handleReviewClaim))
	s.mux.Handle("POST "+base+"/claims/{id}/revalidate", s.authenticated(true, s.handleRevalidateClaim))
	s.mux.Handle("POST "+base+"/claims/{id}/retract", s.authenticated(true, s.handleRetractClaim))
}

func userActor(p tenancy.Principal) evidence.Actor { return evidence.Actor{Type: "USER", ID: p.UserID} }

func (s *Server) evidenceError(w http.ResponseWriter, r *http.Request, op string, err error) {
	var ve *evidence.ValidationError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": ve.Error(), "code": "validation_failed", "findings": ve.Findings})
	case errors.Is(err, evidence.ErrModelNotFound):
		writeError(w, http.StatusNotFound, "not_found", "architecture model not found")
	case errors.Is(err, evidence.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, evidence.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, evidence.ErrInvalidState):
		writeError(w, http.StatusConflict, "invalid_state", err.Error())
	case errors.Is(err, evidence.ErrReviewerIsOwner):
		writeError(w, http.StatusForbidden, "reviewer_is_author", err.Error())
	default:
		s.internalError(w, r, op, err)
	}
}

type reasonBody struct {
	Reason string `json:"reason"`
}

func (s *Server) handleCaptureEvidence(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in evidence.EvidenceInput
	if !decodeJSON(w, r, &in) {
		return
	}
	ev, created, err := s.opts.Evidence.CaptureEvidence(r.Context(), p.TenantID, r.PathValue("model"), in, userActor(p))
	if err != nil {
		s.evidenceError(w, r, "capture evidence", err)
		return
	}
	code := http.StatusCreated
	if !created {
		code = http.StatusOK // same source and content already captured
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, code, ev)
}

func (s *Server) handleListEvidence(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	limit, err := intParam(r, "limit", 200)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	list, err := s.opts.Evidence.ListEvidence(r.Context(), p.TenantID, r.PathValue("model"), r.URL.Query().Get("status"), limit)
	if err != nil {
		s.evidenceError(w, r, "list evidence", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) handleGetEvidence(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	ev, err := s.opts.Evidence.GetEvidence(r.Context(), p.TenantID, r.PathValue("model"), r.PathValue("id"))
	if err != nil {
		s.evidenceError(w, r, "get evidence", err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) handleRevokeEvidence(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in reasonBody
	if !decodeJSON(w, r, &in) {
		return
	}
	ev, stale, err := s.opts.Evidence.RevokeEvidence(r.Context(), p.TenantID, r.PathValue("model"), r.PathValue("id"), in.Reason, userActor(p))
	if err != nil {
		s.evidenceError(w, r, "revoke evidence", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"evidence": ev, "claims_marked_stale": stale})
}

func (s *Server) handleRecordAssumption(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in evidence.AssumptionInput
	if !decodeJSON(w, r, &in) {
		return
	}
	a, err := s.opts.Evidence.RecordAssumption(r.Context(), p.TenantID, r.PathValue("model"), in, userActor(p))
	if err != nil {
		s.evidenceError(w, r, "record assumption", err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) handleListAssumptions(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	list, err := s.opts.Evidence.ListAssumptions(r.Context(), p.TenantID, r.PathValue("model"), r.URL.Query().Get("status"))
	if err != nil {
		s.evidenceError(w, r, "list assumptions", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) handleAssumptionStatus(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in evidence.AssumptionStatusInput
	if !decodeJSON(w, r, &in) {
		return
	}
	a, stale, err := s.opts.Evidence.SetAssumptionStatus(r.Context(), p.TenantID, r.PathValue("model"), r.PathValue("id"), in, userActor(p))
	if err != nil {
		s.evidenceError(w, r, "set assumption status", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assumption": a, "claims_marked_stale": stale})
}

func (s *Server) handleMakeClaim(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in evidence.ClaimInput
	if !decodeJSON(w, r, &in) {
		return
	}
	c, err := s.opts.Evidence.MakeClaim(r.Context(), p.TenantID, r.PathValue("model"), in, userActor(p))
	if err != nil {
		s.evidenceError(w, r, "make claim", err)
		return
	}
	w.Header().Set("Location", "/v1/architecture/"+c.ModelID+"/claims/"+c.ID)
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) handleListClaims(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	q := r.URL.Query()
	limit, err := intParam(r, "limit", 200)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	list, err := s.opts.Evidence.ListClaims(r.Context(), p.TenantID, r.PathValue("model"), evidence.ClaimFilter{
		Lifecycle: q.Get("lifecycle"), Class: q.Get("claim_class"), NodeID: q.Get("node_id"), Limit: limit,
	})
	if err != nil {
		s.evidenceError(w, r, "list claims", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) handleGetClaim(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	c, err := s.opts.Evidence.GetClaim(r.Context(), p.TenantID, r.PathValue("model"), r.PathValue("id"))
	if err != nil {
		s.evidenceError(w, r, "get claim", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleReviewClaim(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in evidence.ReviewInput
	if !decodeJSON(w, r, &in) {
		return
	}
	c, err := s.opts.Evidence.ReviewClaim(r.Context(), p.TenantID, r.PathValue("model"), r.PathValue("id"), in, userActor(p))
	if err != nil {
		s.evidenceError(w, r, "review claim", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleRevalidateClaim(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in evidence.RevalidateInput
	if !decodeJSON(w, r, &in) {
		return
	}
	c, err := s.opts.Evidence.RevalidateClaim(r.Context(), p.TenantID, r.PathValue("model"), r.PathValue("id"), in, userActor(p))
	if err != nil {
		s.evidenceError(w, r, "revalidate claim", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleRetractClaim(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in reasonBody
	if !decodeJSON(w, r, &in) {
		return
	}
	c, err := s.opts.Evidence.RetractClaim(r.Context(), p.TenantID, r.PathValue("model"), r.PathValue("id"), in.Reason, userActor(p))
	if err != nil {
		s.evidenceError(w, r, "retract claim", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}
