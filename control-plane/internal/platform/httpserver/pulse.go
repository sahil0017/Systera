package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	ep "github.com/systera/control-plane/internal/engineeringpulse"
	"github.com/systera/control-plane/internal/tenancy"
)

// PulseService is Engineering Pulse's read side (implemented by *engineeringpulse.Store).
// Pulse data is platform-wide; writes happen in the research worker and the admin CLI.
type PulseService interface {
	ListTrends(ctx context.Context, tenantID, facet string, minScore float64, limit int) ([]ep.TrendSummary, error)
	LoadTopics(ctx context.Context, tenantID string) ([]ep.Topic, error)
	GetTopic(ctx context.Context, tenantID, key string) (ep.TopicDetail, error)
	ListItems(ctx context.Context, tenantID, topicKey string, includeDuplicates bool, limit int) ([]ep.Item, error)
	GetItem(ctx context.Context, tenantID, id string) (ep.Item, error)
	ListSources(ctx context.Context, tenantID string) ([]ep.Source, error)
	ListReports(ctx context.Context, tenantID, topicKey, status string, limit int) ([]ep.Report, error)
	GetReport(ctx context.Context, tenantID, id string) (ep.Report, error)
	ListKnowledgeUpdates(ctx context.Context, tenantID, topicKey, status string) ([]ep.KnowledgeUpdate, error)
	ListSubscriptions(ctx context.Context, tenantID, agentType string) ([]ep.Subscription, error)
	ListNotifications(ctx context.Context, tenantID, agentType string, since time.Time, limit int) ([]ep.Notification, error)
}

// PatternService retrieves Pulse knowledge for design work (implemented by *engineeringpulse.Retriever).
type PatternService interface {
	Patterns(ctx context.Context, tenantID string, q ep.PatternQuery) ([]ep.PatternReference, error)
	RunReferences(ctx context.Context, tenantID, runID string) ([]ep.RunReference, error)
}

func (s *Server) registerPulseRoutes() {
	if s.opts.Pulse != nil {
		s.mux.Handle("GET /v1/trends", s.authenticated(false, s.handleListTrends))
		s.mux.Handle("GET /v1/trends/topics", s.authenticated(false, s.handleListTopics))
		s.mux.Handle("GET /v1/trends/topics/{key}", s.authenticated(false, s.handleGetTopic))
		s.mux.Handle("GET /v1/trends/items", s.authenticated(false, s.handleListTrendItems))
		s.mux.Handle("GET /v1/trends/items/{id}", s.authenticated(false, s.handleGetTrendItem))
		s.mux.Handle("GET /v1/trends/sources", s.authenticated(false, s.handleListTrendSources))
		s.mux.Handle("GET /v1/trends/reports", s.authenticated(false, s.handleListReports))
		s.mux.Handle("GET /v1/trends/reports/{id}", s.authenticated(false, s.handleGetReport))
		s.mux.Handle("GET /v1/knowledge/updates", s.authenticated(false, s.handleListKnowledgeUpdates))
		s.mux.Handle("GET /v1/knowledge/subscriptions", s.authenticated(false, s.handleListSubscriptions))
		s.mux.Handle("GET /v1/knowledge/notifications", s.authenticated(false, s.handleListNotifications))
	}
	// The guardrail is pure computation and always available.
	s.mux.Handle("POST /v1/trends/compare", s.authenticated(false, s.handleCompareOptions))
	if s.opts.Patterns != nil {
		// Recording a retrieval on a run is a write; plain retrieval is not.
		s.mux.Handle("POST /v1/knowledge/patterns", s.authenticated(false, s.handleRetrievePatterns))
		s.mux.Handle("GET /v1/runs/{id}/knowledge", s.authenticated(false, s.handleRunKnowledge))
	}
}

func (s *Server) pulseError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, ep.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, ep.ErrRunNotFound):
		writeError(w, http.StatusNotFound, "not_found", "run not found")
	case errors.Is(err, ep.ErrIncompleteCriteria):
		writeError(w, http.StatusUnprocessableEntity, "incomplete_assessment", err.Error())
	case errors.Is(err, ep.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		s.internalError(w, r, op, err)
	}
}

func floatParam(r *http.Request, name string) (float64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, errors.New(name + " must be a number")
	}
	return f, nil
}

func (s *Server) handleListTrends(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	limit, err := intParam(r, "limit", 25)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	minScore, err := floatParam(r, "min_score")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	facet := r.URL.Query().Get("facet")
	switch facet {
	case "", "DOMAIN", "TECHNOLOGY", "PATTERN":
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "facet must be DOMAIN, TECHNOLOGY or PATTERN")
		return
	}
	items, err := s.opts.Pulse.ListTrends(r.Context(), p.TenantID, facet, minScore, limit)
	if err != nil {
		s.pulseError(w, r, "list trends", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"note":  "Trend scores measure attention (0.25P+0.25G+0.20E+0.20R+0.10N). They never select technology on their own; see the linked knowledge for evidence and POST /v1/trends/compare for selection.",
	})
}

func (s *Server) handleListTopics(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	ts, err := s.opts.Pulse.LoadTopics(r.Context(), p.TenantID)
	if err != nil {
		s.pulseError(w, r, "list topics", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": ts})
}

func (s *Server) handleGetTopic(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	d, err := s.opts.Pulse.GetTopic(r.Context(), p.TenantID, r.PathValue("key"))
	if err != nil {
		s.pulseError(w, r, "get topic", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleListTrendItems(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	limit, err := intParam(r, "limit", 50)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	q := r.URL.Query()
	items, err := s.opts.Pulse.ListItems(r.Context(), p.TenantID, q.Get("topic"), q.Get("include_duplicates") == "true", limit)
	if err != nil {
		s.pulseError(w, r, "list trend items", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleGetTrendItem(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	it, err := s.opts.Pulse.GetItem(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		s.pulseError(w, r, "get trend item", err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (s *Server) handleListTrendSources(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	srcs, err := s.opts.Pulse.ListSources(r.Context(), p.TenantID)
	if err != nil {
		s.pulseError(w, r, "list trend sources", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": srcs})
}

func (s *Server) handleListReports(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	limit, err := intParam(r, "limit", 50)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	q := r.URL.Query()
	rs, err := s.opts.Pulse.ListReports(r.Context(), p.TenantID, q.Get("topic"), q.Get("status"), limit)
	if err != nil {
		s.pulseError(w, r, "list research reports", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rs})
}

func (s *Server) handleGetReport(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	rep, err := s.opts.Pulse.GetReport(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		s.pulseError(w, r, "get research report", err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleListKnowledgeUpdates(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	q := r.URL.Query()
	ku, err := s.opts.Pulse.ListKnowledgeUpdates(r.Context(), p.TenantID, q.Get("topic"), q.Get("status"))
	if err != nil {
		s.pulseError(w, r, "list knowledge updates", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": ku})
}

func (s *Server) handleListSubscriptions(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	subs, err := s.opts.Pulse.ListSubscriptions(r.Context(), p.TenantID, r.URL.Query().Get("agent_type"))
	if err != nil {
		s.pulseError(w, r, "list knowledge subscriptions", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": subs})
}

func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	limit, err := intParam(r, "limit", 50)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		if since, err = time.Parse(time.RFC3339, v); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "since must be an RFC 3339 timestamp")
			return
		}
	}
	ns, err := s.opts.Pulse.ListNotifications(r.Context(), p.TenantID, r.URL.Query().Get("agent_type"), since, limit)
	if err != nil {
		s.pulseError(w, r, "list knowledge notifications", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": ns})
}

func (s *Server) handleCompareOptions(w http.ResponseWriter, r *http.Request, _ tenancy.Principal) {
	var in struct {
		Options []ep.OptionAssessment `json:"options"`
		Weights *ep.CriteriaWeights   `json:"weights,omitempty"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	cmp, err := ep.CompareOptions(in.Options, in.Weights)
	if err != nil {
		s.pulseError(w, r, "compare options", err)
		return
	}
	writeJSON(w, http.StatusOK, cmp)
}

func (s *Server) handleRetrievePatterns(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var q ep.PatternQuery
	if !decodeJSON(w, r, &q) {
		return
	}
	if q.RunID != "" && !p.Role.CanWrite() {
		writeError(w, http.StatusForbidden, "forbidden", "recording knowledge on a run requires a write role")
		return
	}
	refs, err := s.opts.Patterns.Patterns(r.Context(), p.TenantID, q)
	if err != nil {
		s.pulseError(w, r, "retrieve patterns", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": refs, "recorded_on_run": q.RunID != "" && len(refs) > 0})
}

func (s *Server) handleRunKnowledge(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	refs, err := s.opts.Patterns.RunReferences(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		s.pulseError(w, r, "list run knowledge", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": refs})
}
