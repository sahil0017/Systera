package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/observability"
	"github.com/systera/control-plane/internal/policy"
	"github.com/systera/control-plane/internal/tenancy"
)

// ApprovalService manages human approval gates (implemented by *policy.ApprovalStore).
type ApprovalService interface {
	List(ctx context.Context, tenantID, status string) ([]policy.ApprovalRequest, error)
	Decide(ctx context.Context, tenantID, approvalID, actorID string, approved bool, reason string) (policy.ApprovalRequest, error)
}

func (s *Server) registerApprovalRoutes() {
	s.mux.Handle("GET /v1/approvals", s.authenticated(false, s.handleListApprovals))
	s.mux.Handle("POST /v1/approvals/{id}/decision", s.authenticated(true, s.handleDecideApproval))
}

func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	list, err := s.opts.Approvals.List(r.Context(), p.TenantID, r.URL.Query().Get("status"))
	if err != nil {
		s.internalError(w, r, "list approvals", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) handleDecideApproval(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in struct {
		Approved bool   `json:"approved"`
		Reason   string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := s.opts.Approvals.Decide(r.Context(), p.TenantID, r.PathValue("id"), p.UserID, in.Approved, in.Reason)
	if err != nil {
		switch {
		case errors.Is(err, policy.ErrApprovalNotFound):
			writeError(w, http.StatusNotFound, "not_found", "approval request not found")
		case errors.Is(err, policy.ErrAlreadyDecided):
			writeError(w, http.StatusConflict, "already_decided", err.Error())
		default:
			s.internalError(w, r, "decide approval", err)
		}
		return
	}

	resp := map[string]any{"approval": out}
	// Seam 2: Automatically resume and apply the paused high-risk GraphPatch when approved.
	if in.Approved && out.ActionType == "HIGH_RISK_GRAPH_PATCH" && len(out.PayloadJSON) > 0 && s.opts.Graph != nil && out.ModelID != "" {
		var patch graph.Patch
		if uErr := json.Unmarshal(out.PayloadJSON, &patch); uErr == nil {
			patchRes, pErr := s.opts.Graph.ApplyPatch(r.Context(), p.TenantID, out.ModelID, patch, graph.Author{Type: "USER", ID: p.UserID}, observability.CorrelationIDFromContext(r.Context()))
			if pErr != nil {
				resp["patch_error"] = pErr.Error()
			} else {
				resp["patch_result"] = patchRes
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
