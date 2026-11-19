// Package decisions implements the Architecture Decision Record (ADR) ledger (spec §9.4).
// Every ADR follows an enforced state machine, captures full rationale/trade-offs/evidence,
// maintains an explicit supersession chain, and transitions to NEEDS_REVIEW when any of
// its affected architecture nodes change.
package decisions

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/systera/control-plane/internal/platform/ids"
)

var (
	ErrNotFound          = errors.New("decision not found")
	ErrModelNotFound     = errors.New("architecture model not found")
	ErrInvalidInput      = errors.New("invalid decision input")
	ErrInvalidTransition = errors.New("invalid ADR state transition")
)

// Status represents the lifecycle state of an ADR (§9.4).
type Status string

const (
	StatusProposed    Status = "PROPOSED"
	StatusAccepted    Status = "ACCEPTED"
	StatusRejected    Status = "REJECTED"
	StatusSuperseded  Status = "SUPERSEDED"
	StatusNeedsReview Status = "NEEDS_REVIEW"
)

// Event types emitted to the transactional outbox.
const (
	EventDecisionCreated       = "decision.created"
	EventDecisionStatusChanged = "decision.status_changed"
	EventDecisionSuperseded    = "decision.superseded"
	EventDecisionNeedsReview   = "decision.needs_review"
)

// ConfidenceBands lists allowed confidence bands.
var ConfidenceBands = map[string]bool{
	"HIGH":   true,
	"MEDIUM": true,
	"LOW":    true,
}

// Actor identifies who created or transitioned a decision.
type Actor struct {
	Type  string // USER | AGENT | SYSTEM
	ID    string
	RunID string
}

// Alternative records a considered but unchosen design option.
type Alternative struct {
	Option          string `json:"option"`
	Pros            string `json:"pros,omitempty"`
	Cons            string `json:"cons,omitempty"`
	RejectionReason string `json:"rejection_reason,omitempty"`
}

// Decision is an Architecture Decision Record (ADR) bound to an architecture model.
type Decision struct {
	ID             string        `json:"id"`
	TenantID       string        `json:"tenant_id"`
	ModelID        string        `json:"model_id"`
	ModelVersion   int           `json:"model_version"`
	ADRNumber      int           `json:"adr_number"`
	Title          string        `json:"title"`
	Context        string        `json:"context"`
	ChosenOption   string        `json:"chosen_option"`
	Alternatives   []Alternative `json:"alternatives"`
	Rationale      string        `json:"rationale"`
	TradeOffs      string        `json:"trade_offs"`
	Consequences   string        `json:"consequences"`
	EvidenceRefs   []string      `json:"evidence_refs"`
	ConfidenceBand string        `json:"confidence_band"`
	OwnerType      string        `json:"owner_type"`
	OwnerID        string        `json:"owner_id"`
	AffectedNodes  []string      `json:"affected_nodes"`
	Status         Status        `json:"status"`
	Supersedes     string        `json:"supersedes,omitempty"`
	SupersededBy   string        `json:"superseded_by,omitempty"`
	ReviewReason   string        `json:"review_reason,omitempty"`
	RunID          string        `json:"run_id,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

// CreateInput holds fields required to propose a new ADR.
type CreateInput struct {
	ModelVersion   int           `json:"model_version"`
	Title          string        `json:"title"`
	Context        string        `json:"context"`
	ChosenOption   string        `json:"chosen_option"`
	Alternatives   []Alternative `json:"alternatives"`
	Rationale      string        `json:"rationale"`
	TradeOffs      string        `json:"trade_offs"`
	Consequences   string        `json:"consequences"`
	EvidenceRefs   []string      `json:"evidence_refs"`
	ConfidenceBand string        `json:"confidence_band"`
	AffectedNodes  []string      `json:"affected_nodes"`
}

// TransitionInput requests a state change on an existing ADR.
type TransitionInput struct {
	Status Status `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// CanTransition enforces the ADR state machine (Checklist 5.1):
//
//	PROPOSED     -> ACCEPTED, REJECTED, SUPERSEDED, NEEDS_REVIEW
//	ACCEPTED     -> SUPERSEDED, NEEDS_REVIEW
//	NEEDS_REVIEW -> ACCEPTED, REJECTED, SUPERSEDED
//	REJECTED     -> (terminal)
//	SUPERSEDED   -> (terminal)
func CanTransition(from, to Status) bool {
	switch from {
	case StatusProposed:
		return to == StatusAccepted || to == StatusRejected || to == StatusSuperseded || to == StatusNeedsReview
	case StatusAccepted:
		return to == StatusSuperseded || to == StatusNeedsReview
	case StatusNeedsReview:
		return to == StatusAccepted || to == StatusRejected || to == StatusSuperseded
	case StatusRejected, StatusSuperseded:
		return false
	default:
		return false
	}
}

// ValidStatus reports whether s is a known ADR status.
func ValidStatus(s Status) bool {
	switch s {
	case StatusProposed, StatusAccepted, StatusRejected, StatusSuperseded, StatusNeedsReview:
		return true
	default:
		return false
	}
}

// ValidateCreate checks required ADR fields and normalizes defaults.
func ValidateCreate(in *CreateInput) error {
	if strings.TrimSpace(in.Title) == "" {
		return fmt.Errorf("%w: title is required", ErrInvalidInput)
	}
	if strings.TrimSpace(in.Context) == "" {
		return fmt.Errorf("%w: context is required", ErrInvalidInput)
	}
	if strings.TrimSpace(in.ChosenOption) == "" {
		return fmt.Errorf("%w: chosen_option is required", ErrInvalidInput)
	}
	if strings.TrimSpace(in.Rationale) == "" {
		return fmt.Errorf("%w: rationale is required", ErrInvalidInput)
	}
	if in.ConfidenceBand == "" {
		in.ConfidenceBand = "MEDIUM"
	}
	if !ConfidenceBands[in.ConfidenceBand] {
		return fmt.Errorf("%w: confidence_band must be HIGH, MEDIUM, or LOW", ErrInvalidInput)
	}
	if in.ModelVersion < 0 {
		return fmt.Errorf("%w: model_version must be >= 0", ErrInvalidInput)
	}
	for _, n := range in.AffectedNodes {
		if !ids.Valid(n) {
			return fmt.Errorf("%w: affected_nodes must contain valid UUIDs", ErrInvalidInput)
		}
	}
	if in.Alternatives == nil {
		in.Alternatives = []Alternative{}
	}
	if in.EvidenceRefs == nil {
		in.EvidenceRefs = []string{}
	}
	if in.AffectedNodes == nil {
		in.AffectedNodes = []string{}
	}
	return nil
}
