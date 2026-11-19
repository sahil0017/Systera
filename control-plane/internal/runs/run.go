package runs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
)

var (
	ErrNotFound            = errors.New("design run not found")
	ErrInvalidObjective    = errors.New("objective is required")
	ErrInvalidBudget       = errors.New("max_budget_usd must be between 0 and 100000")
	ErrInvalidIdemKey      = errors.New("Idempotency-Key must be at most 255 printable ASCII characters")
	ErrInvalidTransition   = errors.New("invalid run state transition")
	ErrIdempotencyConflict = errors.New("Idempotency-Key was already used with a different request body")
)

const (
	DefaultBudgetUSD = 5.0
	MaxBudgetUSD     = 100000.0
	maxIdemKeyLen    = 255

	EventRunCreated      = "run.created"
	EventRunStateChanged = "run.state_changed"
)

// State represents the durable V4 autonomous execution run state machine (Section 24).
type State string

const (
	StateCreated         State = "CREATED"
	StatePlanning        State = "PLANNING"
	StatePlanValidating  State = "PLAN_VALIDATING"
	StateReady           State = "READY"
	StateRunning         State = "RUNNING"
	StateWaitingExternal State = "WAITING_EXTERNAL"
	StateEvaluating      State = "EVALUATING"
	StateRevising        State = "REVISING"
	StatePublishReady    State = "PUBLISH_READY"
	StateCompleted       State = "COMPLETED"
	StateFailed          State = "FAILED"
	StateCancelled       State = "CANCELLED"
	StateExpired         State = "EXPIRED"
)

// IsTerminal reports whether no further transitions (or events) are possible.
func (s State) IsTerminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateCancelled || s == StateExpired
}

// DesignRun represents a durable, budgeted execution of the Autonomous System Design Engine.
type DesignRun struct {
	ID                 string         `json:"id"`
	TenantID           string         `json:"tenant_id"`
	ProjectID          string         `json:"project_id"`
	IdempotencyKey     string         `json:"idempotency_key,omitempty"`
	Objective          string         `json:"objective"`
	Constraints        map[string]any `json:"constraints"`
	AcceptanceCriteria []string       `json:"acceptance_criteria"`
	State              State          `json:"state"`
	MaxBudgetUSD       float64        `json:"max_budget_usd"`
	SpentBudgetUSD     float64        `json:"spent_budget_usd"`
	// ReservedBudgetUSD is held by in-flight task leases (Stage 1 runtime).
	ReservedBudgetUSD float64   `json:"reserved_budget_usd"`
	FailureReason     string    `json:"failure_reason,omitempty"`
	CorrelationID     string    `json:"correlation_id"`
	CreatedBy         string    `json:"created_by"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`

	// RequestFingerprint is the SHA-256 of the normalized create request.
	RequestFingerprint string `json:"-"`
}

// Repository defines the persistence port for design runs. Implementations must write
// the run change and its outbox event atomically: either both are durable or neither is.
type Repository interface {
	// CreateIdempotent creates a run and its run.created event. When IdempotencyKey matches
	// an existing run with the same fingerprint it returns that run with created=false;
	// with a different fingerprint it returns ErrIdempotencyConflict.
	CreateIdempotent(ctx context.Context, run DesignRun) (run2 DesignRun, created bool, err error)
	GetByID(ctx context.Context, tenantID, runID string) (DesignRun, error)
	// UpdateState applies a validated transition and records run.state_changed.
	UpdateState(ctx context.Context, tenantID, runID string, next State) (DesignRun, error)
}

// Prepare validates and normalizes a create request and computes its fingerprint.
// Semantically equivalent requests (e.g. omitted vs. default budget) fingerprint equally.
func Prepare(run DesignRun) (DesignRun, error) {
	run.Objective = strings.TrimSpace(run.Objective)
	if run.Objective == "" {
		return DesignRun{}, ErrInvalidObjective
	}
	if run.MaxBudgetUSD < 0 || run.MaxBudgetUSD > MaxBudgetUSD {
		return DesignRun{}, ErrInvalidBudget
	}
	if run.MaxBudgetUSD == 0 {
		run.MaxBudgetUSD = DefaultBudgetUSD
	}
	if len(run.IdempotencyKey) > maxIdemKeyLen {
		return DesignRun{}, ErrInvalidIdemKey
	}
	for i := 0; i < len(run.IdempotencyKey); i++ {
		if c := run.IdempotencyKey[i]; c < 0x21 || c > 0x7e {
			return DesignRun{}, ErrInvalidIdemKey
		}
	}
	if run.Constraints == nil {
		run.Constraints = map[string]any{}
	}
	if run.AcceptanceCriteria == nil {
		run.AcceptanceCriteria = []string{}
	}
	if !ids.Valid(run.CorrelationID) {
		run.CorrelationID = ids.New()
	}

	// json.Marshal sorts map keys, giving a canonical encoding of the request.
	canonical, err := json.Marshal(struct {
		ProjectID          string         `json:"project_id"`
		Objective          string         `json:"objective"`
		Constraints        map[string]any `json:"constraints"`
		AcceptanceCriteria []string       `json:"acceptance_criteria"`
		MaxBudgetUSD       string         `json:"max_budget_usd"`
	}{run.ProjectID, run.Objective, run.Constraints, run.AcceptanceCriteria, fmt.Sprintf("%.4f", run.MaxBudgetUSD)})
	if err != nil {
		return DesignRun{}, fmt.Errorf("fingerprint request: %w", err)
	}
	sum := sha256.Sum256(canonical)
	run.RequestFingerprint = hex.EncodeToString(sum[:])
	return run, nil
}

// checkReplay decides the outcome of a replayed Idempotency-Key.
func checkReplay(existing, incoming DesignRun) (DesignRun, bool, error) {
	// Rows created before fingerprints existed cannot be compared; accept them.
	if existing.RequestFingerprint != "" && existing.RequestFingerprint != incoming.RequestFingerprint {
		return DesignRun{}, false, ErrIdempotencyConflict
	}
	return existing, false, nil
}

func createdEvent(run DesignRun) outbox.EventEnvelope {
	return outbox.EventEnvelope{
		EventType:     EventRunCreated,
		SchemaVersion: "1.0",
		TenantID:      run.TenantID,
		ProjectID:     run.ProjectID,
		RunID:         run.ID,
		CorrelationID: run.CorrelationID,
		OccurredAt:    run.CreatedAt,
		Payload: map[string]any{
			"objective":      run.Objective,
			"state":          string(run.State),
			"max_budget_usd": run.MaxBudgetUSD,
			"created_by":     run.CreatedBy,
		},
	}
}

func stateChangedEvent(run DesignRun, from State) outbox.EventEnvelope {
	payload := map[string]any{
		"from":     string(from),
		"to":       string(run.State),
		"state":    string(run.State),
		"terminal": run.State.IsTerminal(),
	}
	if run.FailureReason != "" {
		payload["reason"] = run.FailureReason
	}
	return outbox.EventEnvelope{
		EventType:     EventRunStateChanged,
		SchemaVersion: "1.0",
		TenantID:      run.TenantID,
		ProjectID:     run.ProjectID,
		RunID:         run.ID,
		CorrelationID: run.CorrelationID,
		OccurredAt:    run.UpdatedAt,
		Payload:       payload,
	}
}

// CanTransition reports whether the run state machine allows from -> to.
func CanTransition(from, to State) bool { return isValidTransition(from, to) }

func isValidTransition(from, to State) bool {
	if from.IsTerminal() {
		return false
	}
	if to == StateCancelled || to == StateFailed || to == StateExpired {
		return true
	}
	switch from {
	case StateCreated:
		return to == StatePlanning
	case StatePlanning:
		return to == StatePlanValidating
	case StatePlanValidating:
		return to == StateReady || to == StatePlanning
	case StateReady:
		return to == StateRunning
	case StateRunning:
		return to == StateWaitingExternal || to == StateEvaluating
	case StateWaitingExternal:
		return to == StateRunning
	case StateEvaluating:
		return to == StateRevising || to == StatePublishReady
	case StateRevising:
		return to == StateReady || to == StatePlanning
	case StatePublishReady:
		return to == StateCompleted
	default:
		return false
	}
}
