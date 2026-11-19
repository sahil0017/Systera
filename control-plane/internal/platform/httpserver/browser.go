package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/systera/control-plane/internal/auth"
	bi "github.com/systera/control-plane/internal/browserintel"
	"github.com/systera/control-plane/internal/counterfactual"
	"github.com/systera/control-plane/internal/runs"
	"github.com/systera/control-plane/internal/runtime"
	"github.com/systera/control-plane/internal/tenancy"
)

// BrowserService manages Engineering Lens sessions, observations, analysis and promotion
// (implemented by *browserintel.Store).
type BrowserService interface {
	CreateSession(ctx context.Context, tenantID string, a bi.Actor, in bi.CreateSessionInput) (bi.Session, error)
	GetSession(ctx context.Context, tenantID string, a bi.Actor, sessionID string) (bi.Session, error)
	ListSessions(ctx context.Context, tenantID string, a bi.Actor) ([]bi.Session, error)
	DeleteSession(ctx context.Context, tenantID string, a bi.Actor, sessionID string) (bi.DeletionReport, error)
	RecordObservation(ctx context.Context, tenantID string, a bi.Actor, in bi.ObservationInput) (bi.Observation, error)
	ListObservations(ctx context.Context, tenantID string, a bi.Actor, sessionID string) ([]bi.Observation, error)
	DeleteObservation(ctx context.Context, tenantID string, a bi.Actor, observationID string) (bi.DeletionReport, error)
	AnalyzeSession(ctx context.Context, tenantID string, a bi.Actor, in bi.AnalyzeInput) (bi.WebsiteProfile, error)
	GetWebsite(ctx context.Context, tenantID string, a bi.Actor, websiteID string) (bi.WebsiteProfile, error)
	ListWebsites(ctx context.Context, tenantID string, a bi.Actor) ([]bi.WebsiteProfile, error)
	PromoteToWorkspace(ctx context.Context, tenantID string, a bi.Actor, websiteID string) (bi.WebsiteProfile, error)
	StartAnalysis(ctx context.Context, tenantID string, a bi.Actor, websiteID, idempotencyKey string, budgetUSD float64) (bi.AnalysisStart, error)
	ListFindings(ctx context.Context, tenantID string, a bi.Actor, websiteID, table string) ([]bi.Finding, error)
	ListEvidenceLinks(ctx context.Context, tenantID string, a bi.Actor, websiteID string) ([]bi.EvidenceLinkRow, error)
	GetFingerprint(ctx context.Context, tenantID string, a bi.Actor, websiteID string) (bi.ArchitectureFingerprint, error)
	BuildAndPersistFingerprint(ctx context.Context, tenantID string, a bi.Actor, websiteID string) (bi.ArchitectureFingerprint, error)
}

func (s *Server) registerBrowserRoutes() {
	s.mux.Handle("GET /v1/browser/consent", s.authenticated(false, s.handleBrowserConsent))
	s.mux.Handle("POST /v1/browser/sessions", s.authenticated(true, s.handleCreateBrowserSession))
	s.mux.Handle("GET /v1/browser/sessions", s.authenticated(false, s.handleListBrowserSessions))
	s.mux.Handle("GET /v1/browser/sessions/{id}", s.authenticated(false, s.handleGetBrowserSession))
	s.mux.Handle("DELETE /v1/browser/sessions/{id}", s.authenticated(true, s.handleDeleteBrowserSession))
	s.mux.Handle("GET /v1/browser/sessions/{id}/observations", s.authenticated(false, s.handleListBrowserObservations))
	s.mux.Handle("POST /v1/browser/observations", s.authenticated(true, s.handleRecordBrowserObservation))
	s.mux.Handle("DELETE /v1/browser/observations/{id}", s.authenticated(true, s.handleDeleteBrowserObservation))
	s.mux.Handle("POST /v1/browser/analyze", s.authenticated(true, s.handleAnalyzeBrowserSession))
	s.mux.Handle("GET /v1/websites", s.authenticated(false, s.handleListWebsites))
	s.mux.Handle("GET /v1/websites/{id}", s.authenticated(false, s.handleGetWebsite))
	s.mux.Handle("GET /v1/websites/{id}/fingerprint", s.authenticated(false, s.handleGetWebsiteFingerprint))
	s.mux.Handle("POST /v1/websites/{id}/fingerprint", s.authenticated(true, s.handleBuildWebsiteFingerprint))
	s.mux.Handle("GET /v1/websites/{id}/alternatives", s.authenticated(false, s.handleGetWebsiteAlternatives))
	s.mux.Handle("POST /v1/websites/{id}/alternatives", s.authenticated(true, s.handleGenerateWebsiteAlternatives))
	s.mux.Handle("GET /v1/websites/{id}/alternatives/diff", s.authenticated(false, s.handleDiffWebsiteAlternatives))
	s.mux.Handle("POST /v1/websites/{id}/promote", s.authenticated(true, s.handlePromoteWebsite))
	s.mux.Handle("POST /v1/websites/{id}/analysis", s.authenticated(true, s.handleStartWebsiteAnalysis))
	s.mux.Handle("GET /v1/websites/{id}/capabilities", s.authenticated(false, s.handleListWebsiteFindings("website_capabilities")))
	s.mux.Handle("GET /v1/websites/{id}/hypotheses", s.authenticated(false, s.handleListWebsiteFindings("architecture_hypotheses")))
	s.mux.Handle("GET /v1/websites/{id}/evidence", s.authenticated(false, s.handleListWebsiteEvidence))
}

func (s *Server) handleStartWebsiteAnalysis(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in struct {
		BudgetUSD float64 `json:"max_budget_usd,omitempty"`
	}
	if r.ContentLength > 0 && !decodeJSON(w, r, &in) {
		return
	}
	res, err := s.opts.Browser.StartAnalysis(r.Context(), p.TenantID, bi.ActorFrom(p), r.PathValue("id"), r.Header.Get("Idempotency-Key"), in.BudgetUSD)
	if err != nil {
		var pe *runtime.PlanError
		switch {
		case errors.As(err, &pe):
			writeError(w, http.StatusUnprocessableEntity, "invalid_plan", err.Error())
		case errors.Is(err, bi.ErrNoRuntime):
			writeError(w, http.StatusServiceUnavailable, "analysis_unavailable", err.Error())
		case errors.Is(err, runs.ErrIdempotencyConflict):
			writeError(w, http.StatusConflict, "idempotency_conflict", err.Error())
		default:
			s.browserError(w, r, "start website analysis", err)
		}
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (s *Server) handleListWebsiteFindings(table string) principalHandler {
	return func(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
		fs, err := s.opts.Browser.ListFindings(r.Context(), p.TenantID, bi.ActorFrom(p), r.PathValue("id"), table)
		if err != nil {
			s.browserError(w, r, "list website findings", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": fs})
	}
}

func (s *Server) handleListWebsiteEvidence(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	ev, err := s.opts.Browser.ListEvidenceLinks(r.Context(), p.TenantID, bi.ActorFrom(p), r.PathValue("id"))
	if err != nil {
		s.browserError(w, r, "list website evidence", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": ev})
}

func (s *Server) registerAuthRoutes() {
	if s.opts.TokenIssuer != nil {
		s.mux.Handle("POST /v1/auth/lens-token", s.authenticated(false, s.handleIssueLensToken))
	}
}

func (s *Server) browserError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, bi.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "browser resource not found")
	case errors.Is(err, bi.ErrConsentRequired):
		writeError(w, http.StatusForbidden, "consent_required", err.Error())
	case errors.Is(err, bi.ErrURLPolicyDenied):
		writeError(w, http.StatusBadRequest, "url_policy_denied", err.Error())
	case errors.Is(err, bi.ErrPayloadTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", err.Error())
	case errors.Is(err, bi.ErrPageLimitReached):
		writeError(w, http.StatusConflict, "page_limit_reached", err.Error())
	case errors.Is(err, bi.ErrDuplicatePage):
		writeError(w, http.StatusConflict, "page_already_captured", err.Error())
	case errors.Is(err, bi.ErrSessionClosed):
		writeError(w, http.StatusConflict, "session_closed", err.Error())
	case errors.Is(err, bi.ErrPromotionInProgress):
		writeError(w, http.StatusConflict, "promotion_in_progress", err.Error())
	case errors.Is(err, bi.ErrNoObservations):
		writeError(w, http.StatusConflict, "no_observations", err.Error())
	case errors.Is(err, bi.ErrInvalidFinding):
		writeError(w, http.StatusUnprocessableEntity, "invalid_finding", err.Error())
	case errors.Is(err, bi.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		s.internalError(w, r, op, err)
	}
}

// decodeObservation reads an observation strictly: bounded to the observation size limit
// and rejecting any field outside the collector contract (a modified extension cannot
// smuggle extra data such as cookies or storage dumps).
func decodeObservation(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, bi.MaxObservationBytes+4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "observation exceeds 256 KiB")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "observation does not match schema 1.0: "+err.Error())
		return false
	}
	return true
}

func (s *Server) handleBrowserConsent(w http.ResponseWriter, _ *http.Request, _ tenancy.Principal) {
	writeJSON(w, http.StatusOK, map[string]any{"consent_version": bi.ConsentVersion, "text": bi.ConsentText,
		"max_pages": bi.MaxPagesPerSession, "default_retention_days": bi.DefaultRetention, "max_retention_days": bi.MaxRetentionDays})
}

func (s *Server) handleCreateBrowserSession(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in bi.CreateSessionInput
	if !decodeJSON(w, r, &in) {
		return
	}
	sess, err := s.opts.Browser.CreateSession(r.Context(), p.TenantID, bi.ActorFrom(p), in)
	if err != nil {
		s.browserError(w, r, "create browser session", err)
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) handleListBrowserSessions(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	list, err := s.opts.Browser.ListSessions(r.Context(), p.TenantID, bi.ActorFrom(p))
	if err != nil {
		s.browserError(w, r, "list browser sessions", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) handleGetBrowserSession(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	sess, err := s.opts.Browser.GetSession(r.Context(), p.TenantID, bi.ActorFrom(p), r.PathValue("id"))
	if err != nil {
		s.browserError(w, r, "get browser session", err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleDeleteBrowserSession(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	rep, err := s.opts.Browser.DeleteSession(r.Context(), p.TenantID, bi.ActorFrom(p), r.PathValue("id"))
	if err != nil {
		s.browserError(w, r, "delete browser session", err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleListBrowserObservations(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	obs, err := s.opts.Browser.ListObservations(r.Context(), p.TenantID, bi.ActorFrom(p), r.PathValue("id"))
	if err != nil {
		s.browserError(w, r, "list browser observations", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": obs})
}

func (s *Server) handleRecordBrowserObservation(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in bi.ObservationInput
	if !decodeObservation(w, r, &in) {
		return
	}
	obs, err := s.opts.Browser.RecordObservation(r.Context(), p.TenantID, bi.ActorFrom(p), in)
	if err != nil {
		s.browserError(w, r, "record browser observation", err)
		return
	}
	writeJSON(w, http.StatusCreated, obs)
}

func (s *Server) handleDeleteBrowserObservation(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	rep, err := s.opts.Browser.DeleteObservation(r.Context(), p.TenantID, bi.ActorFrom(p), r.PathValue("id"))
	if err != nil {
		s.browserError(w, r, "delete browser observation", err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleAnalyzeBrowserSession(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in bi.AnalyzeInput
	if !decodeJSON(w, r, &in) {
		return
	}
	profile, err := s.opts.Browser.AnalyzeSession(r.Context(), p.TenantID, bi.ActorFrom(p), in)
	if err != nil {
		s.browserError(w, r, "analyze browser session", err)
		return
	}
	writeJSON(w, http.StatusCreated, profile)
}

func (s *Server) handleListWebsites(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	list, err := s.opts.Browser.ListWebsites(r.Context(), p.TenantID, bi.ActorFrom(p))
	if err != nil {
		s.browserError(w, r, "list websites", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) handleGetWebsite(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	web, err := s.opts.Browser.GetWebsite(r.Context(), p.TenantID, bi.ActorFrom(p), r.PathValue("id"))
	if err != nil {
		s.browserError(w, r, "get website", err)
		return
	}
	writeJSON(w, http.StatusOK, web)
}

func (s *Server) handleGetWebsiteFingerprint(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	fp, err := s.opts.Browser.GetFingerprint(r.Context(), p.TenantID, bi.ActorFrom(p), r.PathValue("id"))
	if err != nil {
		s.browserError(w, r, "get website fingerprint", err)
		return
	}
	if layerParam := r.URL.Query().Get("layer"); layerParam != "" {
		layerKey := bi.FingerprintLayer(layerParam)
		sub, ok := fp.Layers[layerKey]
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_layer", "unknown fingerprint layer: "+layerParam)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"website_id":    fp.WebsiteID,
			"domain":        fp.Domain,
			"disclaimer":    fp.Disclaimer,
			"model_id":      fp.ModelID,
			"graph_version": fp.GraphVersion,
			"layer":         sub,
		})
		return
	}
	writeJSON(w, http.StatusOK, fp)
}

func (s *Server) handleBuildWebsiteFingerprint(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	fp, err := s.opts.Browser.BuildAndPersistFingerprint(r.Context(), p.TenantID, bi.ActorFrom(p), r.PathValue("id"))
	if err != nil {
		s.browserError(w, r, "build website fingerprint", err)
		return
	}
	writeJSON(w, http.StatusCreated, fp)
}

func (s *Server) handleGetWebsiteAlternatives(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	web, err := s.opts.Browser.GetWebsite(r.Context(), p.TenantID, bi.ActorFrom(p), r.PathValue("id"))
	if err != nil {
		s.browserError(w, r, "get website alternatives", err)
		return
	}
	if s.opts.Counterfactual != nil {
		set, cErr := s.opts.Counterfactual.ListForWebsite(r.Context(), p.TenantID, web.ID, web.Domain)
		if cErr != nil {
			s.counterfactualError(w, r, "list website alternatives", cErr)
			return
		}
		writeJSON(w, http.StatusOK, set)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"alternatives": web.Alternatives})
}

func (s *Server) handleGenerateWebsiteAlternatives(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	if s.opts.Counterfactual == nil {
		writeError(w, http.StatusServiceUnavailable, "counterfactual_unavailable", "counterfactual service is not configured")
		return
	}
	web, err := s.opts.Browser.GetWebsite(r.Context(), p.TenantID, bi.ActorFrom(p), r.PathValue("id"))
	if err != nil {
		s.browserError(w, r, "get website for alternatives", err)
		return
	}
	var in counterfactual.GenerateInput
	if r.ContentLength > 0 && !decodeJSON(w, r, &in) {
		return
	}
	set, err := s.opts.Counterfactual.GenerateForWebsite(r.Context(), p.TenantID, web.ID, web.ModelID, web.Domain, in, p.UserID)
	if err != nil {
		s.counterfactualError(w, r, "generate website alternatives", err)
		return
	}
	writeJSON(w, http.StatusCreated, set)
}

func (s *Server) handleDiffWebsiteAlternatives(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	if s.opts.Counterfactual == nil {
		writeError(w, http.StatusServiceUnavailable, "counterfactual_unavailable", "counterfactual service is not configured")
		return
	}
	if _, err := s.opts.Browser.GetWebsite(r.Context(), p.TenantID, bi.ActorFrom(p), r.PathValue("id")); err != nil {
		s.browserError(w, r, "get website for alternative diff", err)
		return
	}
	s.handleDiffAlternatives(w, r, p)
}

func (s *Server) handlePromoteWebsite(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	web, err := s.opts.Browser.PromoteToWorkspace(r.Context(), p.TenantID, bi.ActorFrom(p), r.PathValue("id"))
	if err != nil {
		s.browserError(w, r, "promote website to workspace", err)
		return
	}
	writeJSON(w, http.StatusOK, web)
}

// handleIssueLensToken exchanges a workspace token for a short-lived, tenant-pinned token
// that may only call the browser-intelligence routes (3.5). Scoped tokens cannot mint more:
// /v1/auth is outside every scope.
func (s *Server) handleIssueLensToken(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in struct {
		TTLSeconds int `json:"ttl_seconds,omitempty"`
	}
	if r.ContentLength > 0 && !decodeJSON(w, r, &in) {
		return
	}
	ttl := time.Duration(in.TTLSeconds) * time.Second
	if ttl > auth.MaxScopedTokenLifetime {
		writeError(w, http.StatusBadRequest, "invalid_request", "ttl_seconds may not exceed 3600")
		return
	}
	tok, exp, err := s.opts.TokenIssuer.IssueScoped(p.Subject, p.TenantID, auth.ScopeLens, ttl)
	if err != nil {
		s.internalError(w, r, "issue lens token", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"access_token": tok, "token_type": "Bearer", "scope": auth.ScopeLens,
		"tenant_id": p.TenantID, "expires_at": exp, "expires_in": int(time.Until(exp).Seconds())})
}
