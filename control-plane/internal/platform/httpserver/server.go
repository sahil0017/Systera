package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/systera/control-plane/internal/artifacts"
	"github.com/systera/control-plane/internal/auth"
	"github.com/systera/control-plane/internal/observability"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/projects"
	"github.com/systera/control-plane/internal/runs"
	"github.com/systera/control-plane/internal/runtime"
	"github.com/systera/control-plane/internal/scheduler"
	"github.com/systera/control-plane/internal/tenancy"
)

const (
	maxBodyBytes      = 1 << 20
	sseBatchSize      = 200
	readinessTimeout  = 2 * time.Second
	defaultSSEPoll    = 2 * time.Second
	defaultSSEHeartbt = 15 * time.Second
)

// ReadinessCheck is one dependency probed by /readyz.
type ReadinessCheck struct {
	Name  string
	Check func(context.Context) error
}

// TaskService is the plan/task surface of the scheduler (implemented by *scheduler.Store).
type TaskService interface {
	SubmitPlan(ctx context.Context, tenantID, runID string, specs []runtime.TaskSpec) ([]runtime.Task, error)
	ListTasks(ctx context.Context, tenantID, runID string) ([]runtime.Task, error)
}

// Options wires the server's dependencies.
type Options struct {
	Logger        *slog.Logger
	Projects      projects.Repository
	Runs          runs.Repository
	Events        outbox.Stream
	Authenticator *auth.Authenticator
	Readiness     []ReadinessCheck
	// Tasks enables the Stage 1 plan/task endpoints when set.
	Tasks TaskService
	// Graph enables the Stage 2 architecture graph endpoints when set.
	Graph GraphService
	// Evidence enables the evidence ledger (claims, evidence, assumptions) when set.
	Evidence EvidenceService
	// Artifacts enables the dependency engine's artifact endpoints; Impact the dry-run
	// impact analysis endpoints.
	Artifacts      ArtifactService
	Impact         ImpactService
	Projector      *artifacts.Projector
	AsyncProjector *artifacts.AsyncProjector
	// Decisions enables the ADR decision ledger when set.
	Decisions DecisionService
	// Approvals enables human approval gates when set.
	Approvals ApprovalService
	// Knowledge & Memory enable hybrid knowledge retrieval and distilled memory.
	Knowledge KnowledgeService
	Memory    MemoryService
	// Browser enables Engineering Lens sessions, observations, and website intelligence.
	Browser BrowserService
	// TokenIssuer enables POST /v1/auth/lens-token (scoped, short-lived extension tokens)
	// and background workspace/lens auto-bootstrap when BootstrapMembers is also set.
	TokenIssuer      *auth.TokenIssuer
	BootstrapMembers interface {
		DefaultOwner(ctx context.Context) (subject, tenantID string, err error)
	}
	// Pulse enables the Engineering Pulse read API (/v1/trends, /v1/knowledge/updates...);
	// Patterns enables design-time pattern retrieval with explanations.
	Pulse    PulseService
	Patterns PatternService
	// Counterfactual enables Architecture Counterfactual alternatives, decision matrices, and diffs.
	Counterfactual CounterfactualService
	// Learning enables Engineering Learning Mode (lessons, challenges) for architecture models.
	Learning LearningService
	// Twins enables the design-time Architecture Digital Twin (build + validation report).
	Twins TwinService
	// WebDir, when set, serves the built React/Vite web workspace at /.
	WebDir string

	// SSEPollInterval bounds how long a stream may lag if a notification is lost.
	SSEPollInterval time.Duration
	// SSEHeartbeatInterval is how often idle streams send a keep-alive comment.
	SSEHeartbeatInterval time.Duration
}

// Server wires the Control Plane HTTP REST + SSE API.
type Server struct {
	opts Options
	log  *slog.Logger
	mux  *http.ServeMux
}

// New constructs the Control Plane HTTP server and registers routes.
func New(opts Options) *Server {
	if opts.SSEPollInterval <= 0 {
		opts.SSEPollInterval = defaultSSEPoll
	}
	if opts.SSEHeartbeatInterval <= 0 {
		opts.SSEHeartbeatInterval = defaultSSEHeartbt
	}
	s := &Server{opts: opts, log: opts.Logger, mux: http.NewServeMux()}
	s.registerRoutes()
	return s
}

// Handler returns the root HTTP handler wrapped with correlation middleware.
func (s *Server) Handler() http.Handler {
	return s.withCorrelation(s.mux)
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)

	s.mux.Handle("POST /v1/projects", s.authenticated(true, s.handleCreateProject))
	s.mux.Handle("GET /v1/projects", s.authenticated(false, s.handleListProjects))
	s.mux.Handle("GET /v1/projects/{id}", s.authenticated(false, s.handleGetProject))

	s.mux.Handle("POST /v1/projects/{id}/runs", s.authenticated(true, s.handleCreateRun))
	s.mux.Handle("GET /v1/runs/{id}", s.authenticated(false, s.handleGetRun))
	s.mux.Handle("POST /v1/runs/{id}/cancel", s.authenticated(true, s.handleCancelRun))
	s.mux.Handle("GET /v1/runs/{id}/events", s.authenticated(false, s.handleRunEventsSSE))

	if s.opts.Graph != nil {
		s.registerGraphRoutes()
	}
	if s.opts.Evidence != nil {
		s.registerEvidenceRoutes()
	}
	if s.opts.Artifacts != nil {
		s.registerArtifactRoutes()
	}
	if s.opts.Decisions != nil {
		s.registerDecisionRoutes()
	}
	if s.opts.Approvals != nil {
		s.registerApprovalRoutes()
	}
	s.registerKnowledgeRoutes()
	if s.opts.Browser != nil {
		s.registerBrowserRoutes()
	}
	if s.opts.Counterfactual != nil {
		s.registerCounterfactualRoutes()
	}
	if s.opts.Learning != nil {
		s.registerLearningRoutes()
	}
	if s.opts.Twins != nil {
		s.registerTwinRoutes()
	}
	s.registerAuthRoutes()
	s.registerPulseRoutes()
	if s.opts.Tasks != nil {
		s.mux.Handle("POST /v1/runs/{id}/plan", s.authenticated(true, s.handleSubmitPlan))
		s.mux.Handle("GET /v1/runs/{id}/tasks", s.authenticated(false, s.handleListTasks))
	}
	if s.opts.WebDir != "" {
		s.mux.Handle("/", http.FileServer(http.Dir(s.opts.WebDir)))
	}
}

// ---------------------------------------------------------------------------
// Probes
// ---------------------------------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz probes every dependency concurrently and reports 503 if any fails, so
// load balancers stop routing to an instance that cannot serve requests.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	serveReadiness(w, r, s.log, s.opts.Readiness)
}

// serveReadiness runs every check concurrently and answers 503 if any fails (or none
// are wired), so load balancers stop routing to an instance that cannot do its job.
func serveReadiness(w http.ResponseWriter, r *http.Request, log *slog.Logger, checks []ReadinessCheck) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	results := make(map[string]string, len(checks))
	var mu sync.Mutex
	var wg sync.WaitGroup
	ready := len(checks) > 0
	for _, c := range checks {
		wg.Add(1)
		go func(c ReadinessCheck) {
			defer wg.Done()
			err := c.Check(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				ready = false
				results[c.Name] = "fail"
				log.Warn("readiness check failed", "check", c.Name, "error", err)
				return
			}
			results[c.Name] = "ok"
		}(c)
	}
	wg.Wait()

	status, code := "ready", http.StatusOK
	if !ready {
		status, code = "not_ready", http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"status": status, "checks": results})
}

// ProbeHandler serves only /healthz and /readyz, for background processes (scheduler,
// workers) that have no public API.
func ProbeHandler(log *slog.Logger, checks []ReadinessCheck) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) { serveReadiness(w, r, log, checks) })
	return mux
}

// ---------------------------------------------------------------------------
// Projects
// ---------------------------------------------------------------------------

type createProjectRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var req createProjectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	created, err := s.opts.Projects.Create(r.Context(), projects.Project{
		TenantID:      p.TenantID,
		Name:          req.Name,
		Description:   req.Description,
		CreatedBy:     p.UserID,
		CorrelationID: observability.CorrelationIDFromContext(r.Context()),
	})
	if err != nil {
		if errors.Is(err, projects.ErrInvalidName) {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		s.internalError(w, r, "create project", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	list, err := s.opts.Projects.ListByTenant(r.Context(), p.TenantID)
	if err != nil {
		s.internalError(w, r, "list projects", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	proj, err := s.opts.Projects.GetByID(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, projects.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "project not found")
			return
		}
		s.internalError(w, r, "get project", err)
		return
	}
	writeJSON(w, http.StatusOK, proj)
}

// ---------------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------------

type createRunRequest struct {
	Objective          string         `json:"objective"`
	Constraints        map[string]any `json:"constraints,omitempty"`
	AcceptanceCriteria []string       `json:"acceptance_criteria,omitempty"`
	MaxBudgetUSD       float64        `json:"max_budget_usd,omitempty"`
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	projectID := r.PathValue("id")
	if _, err := s.opts.Projects.GetByID(r.Context(), p.TenantID, projectID); err != nil {
		if errors.Is(err, projects.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "project not found")
			return
		}
		s.internalError(w, r, "verify project", err)
		return
	}

	var req createRunRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	run, created, err := s.opts.Runs.CreateIdempotent(r.Context(), runs.DesignRun{
		TenantID:           p.TenantID,
		ProjectID:          projectID,
		IdempotencyKey:     r.Header.Get("Idempotency-Key"),
		Objective:          req.Objective,
		Constraints:        req.Constraints,
		AcceptanceCriteria: req.AcceptanceCriteria,
		MaxBudgetUSD:       req.MaxBudgetUSD,
		CorrelationID:      observability.CorrelationIDFromContext(r.Context()),
		CreatedBy:          p.UserID,
	})
	if err != nil {
		switch {
		case errors.Is(err, runs.ErrInvalidObjective), errors.Is(err, runs.ErrInvalidBudget), errors.Is(err, runs.ErrInvalidIdemKey):
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		case errors.Is(err, runs.ErrIdempotencyConflict):
			writeError(w, http.StatusUnprocessableEntity, "idempotency_key_conflict", err.Error())
		default:
			// Includes outbox failures: the run was rolled back, so do not report success.
			s.internalError(w, r, "create design run", err)
		}
		return
	}

	if !created {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Location", "/v1/runs/"+run.ID)
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	run, err := s.opts.Runs.GetByID(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, runs.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "design run not found")
			return
		}
		s.internalError(w, r, "get design run", err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	run, err := s.opts.Runs.UpdateState(r.Context(), p.TenantID, r.PathValue("id"), runs.StateCancelled)
	if err != nil {
		switch {
		case errors.Is(err, runs.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", "design run not found")
		case errors.Is(err, runs.ErrInvalidTransition):
			writeError(w, http.StatusConflict, "invalid_transition", "run is already in a terminal state")
		default:
			s.internalError(w, r, "cancel design run", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// handleRunEventsSSE streams a run's events: first the backlog after the client's cursor
// (Last-Event-ID header or ?after=), then every new event as it commits. The stream ends
// once the run is terminal and fully drained, or when the client disconnects.
func (s *Server) handleRunEventsSSE(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	runID := r.PathValue("id")
	run, err := s.opts.Runs.GetByID(r.Context(), p.TenantID, runID)
	if err != nil {
		if errors.Is(err, runs.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "design run not found")
			return
		}
		s.internalError(w, r, "get design run", err)
		return
	}

	cursor, err := parseCursor(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Last-Event-ID / after must be a non-negative integer")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}

	// Subscribe before the first read so nothing committed in between is missed.
	wake, unsubscribe := s.opts.Events.Subscribe(runID)
	defer unsubscribe()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	ctx := r.Context()
	terminal := run.State.IsTerminal()
	poll := time.NewTicker(s.opts.SSEPollInterval)
	defer poll.Stop()
	heartbeat := time.NewTicker(s.opts.SSEHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		for {
			batch, err := s.opts.Events.ListByRunAfter(ctx, p.TenantID, runID, cursor, sseBatchSize)
			if err != nil {
				if ctx.Err() == nil {
					s.log.Error("sse: read events", "run_id", runID, "error", err)
				}
				return
			}
			for _, evt := range batch {
				data, err := json.Marshal(evt)
				if err != nil {
					s.log.Error("sse: marshal event", "event_id", evt.EventID, "error", err)
					continue
				}
				if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", evt.Seq, evt.EventType, data); err != nil {
					return
				}
				cursor = evt.Seq
				if st, _ := evt.Payload["state"].(string); evt.EventType == runs.EventRunStateChanged && runs.State(st).IsTerminal() {
					terminal = true
				}
			}
			if len(batch) > 0 {
				flusher.Flush()
			}
			if len(batch) < sseBatchSize {
				break
			}
		}

		if terminal {
			_, _ = fmt.Fprintf(w, "event: stream.end\ndata: {\"run_id\":%q,\"last_seq\":%d}\n\n", runID, cursor)
			flusher.Flush()
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-poll.C:
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

type submitPlanRequest struct {
	Tasks []runtime.TaskSpec `json:"tasks"`
}

func (s *Server) handleSubmitPlan(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var req submitPlanRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tasks, err := s.opts.Tasks.SubmitPlan(r.Context(), p.TenantID, r.PathValue("id"), req.Tasks)
	if err != nil {
		switch {
		case errors.Is(err, runs.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", "design run not found")
		case errors.Is(err, runtime.ErrInvalidPlan), errors.Is(err, scheduler.ErrUnknownAgent):
			writeError(w, http.StatusBadRequest, "invalid_plan", err.Error())
		case errors.Is(err, scheduler.ErrRunNotPlannable):
			writeError(w, http.StatusConflict, "run_not_plannable", err.Error())
		default:
			s.internalError(w, r, "submit plan", err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"items": tasks})
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	tasks, err := s.opts.Tasks.ListTasks(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, runs.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "design run not found")
			return
		}
		s.internalError(w, r, "list tasks", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": tasks})
}

func parseCursor(r *http.Request) (int64, error) {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("after")
	}
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("bad cursor")
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Middleware & helpers
// ---------------------------------------------------------------------------

type principalHandler func(http.ResponseWriter, *http.Request, tenancy.Principal)

// authenticated verifies the bearer token and tenant membership server-side and passes
// the resulting Principal to h. write=true additionally requires a role that may mutate.
func (s *Server) authenticated(write bool, h principalHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.opts.Authenticator.Authenticate(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Tenant-ID"))
		switch {
		case errors.Is(err, auth.ErrUnauthenticated):
			w.Header().Set("WWW-Authenticate", `Bearer realm="systera"`)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "a valid bearer token is required")
			return
		case errors.Is(err, auth.ErrNoMembership):
			writeError(w, http.StatusForbidden, "forbidden", "not a member of the requested tenant")
			return
		case errors.Is(err, auth.ErrTenantAmbiguous):
			writeError(w, http.StatusBadRequest, "tenant_required", err.Error())
			return
		case err != nil:
			s.internalError(w, r, "authenticate", err)
			return
		}
		if !auth.ScopeAllows(p.Scopes, r.URL.Path) {
			writeError(w, http.StatusForbidden, "insufficient_scope", "this token's scope does not allow "+r.URL.Path)
			return
		}
		if write && !p.Role.CanWrite() {
			writeError(w, http.StatusForbidden, "forbidden", "role "+string(p.Role)+" is read-only")
			return
		}
		ctx := tenancy.WithPrincipal(r.Context(), p)
		h(w, r.WithContext(ctx), p)
	})
}

// withCorrelation propagates X-Correlation-ID when it is a UUID (the column type used
// throughout the event and audit model) and mints one otherwise.
func (s *Server) withCorrelation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Tenant-ID, X-Correlation-ID")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		corrID := strings.ToLower(r.Header.Get("X-Correlation-ID"))
		if !ids.Valid(corrID) {
			corrID = ids.New()
		}
		ctx := observability.WithCorrelationID(r.Context(), corrID)
		w.Header().Set("X-Correlation-ID", corrID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, op string, err error) {
	s.log.Error(op+" failed", "error", err,
		"correlation_id", observability.CorrelationIDFromContext(r.Context()),
		"method", r.Method, "path", r.URL.Path)
	writeError(w, http.StatusInternalServerError, "internal", op+" failed")
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1 MiB")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON payload")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}
