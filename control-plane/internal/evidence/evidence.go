// Package evidence is the Architecture Intelligence Core's evidence ledger (spec §23.11):
// captured evidence, explicit assumptions and classified, confidence-banded claims about
// an architecture model, with independent review and time/dependency-based revalidation.
//
// The database is authoritative for the rules (migrations/000007_evidence_ledger); this
// package validates first so callers get every problem as a finding, not one SQL error.
package evidence

import (
	"errors"
	"time"
)

var (
	ErrNotFound        = errors.New("not found")
	ErrModelNotFound   = errors.New("architecture model not found")
	ErrInvalidInput    = errors.New("invalid request")
	ErrInvalidState    = errors.New("invalid state transition")
	ErrReviewerIsOwner = errors.New("a claim cannot be reviewed by its author")
)

// Enumerations (mirrors the database CHECK constraints).
var (
	SourceTypes     = []string{"SOURCE_DOCUMENT", "WEB_PAGE", "OBSERVATION", "TEST_RESULT", "MEASUREMENT", "USER_STATEMENT", "SIMULATION", "RETRIEVED_KNOWLEDGE", "AGENT_ANALYSIS"}
	IndirectSources = []string{"SIMULATION", "RETRIEVED_KNOWLEDGE", "AGENT_ANALYSIS"}
	TrustLevels     = []string{"VERIFIED", "HIGH", "MEDIUM", "LOW", "UNVERIFIED"}
	AccessPolicies  = []string{"PUBLIC", "TENANT", "RESTRICTED"}
	ClaimClasses    = []string{"OBSERVED", "INFERRED", "PROPOSED"}
	ConfidenceBands = []string{"HIGH", "MEDIUM", "LOW"}
	LinkRelations   = []string{"SUPPORTS", "CONTRADICTS", "CONTEXT"}
	RiskLevels      = []string{"LOW", "MEDIUM", "HIGH"}
	ReviewDecisions = []string{"ACCEPTED", "DISPUTED", "REJECTED"}
)

// Event types written to the outbox.
const (
	EventEvidenceCaptured   = "evidence.captured"
	EventEvidenceRevoked    = "evidence.revoked"
	EventEvidenceExpired    = "evidence.expired"
	EventAssumptionRecorded = "assumption.recorded"
	EventAssumptionStatus   = "assumption.status_changed"
	EventClaimCreated       = "claim.created"
	EventClaimReviewed      = "claim.reviewed"
	EventClaimStale         = "claim.stale"
	EventClaimRevalidated   = "claim.revalidated"
	EventClaimRetracted     = "claim.retracted"
)

// Actor identifies who performs a ledger write, and optional run provenance.
type Actor struct {
	Type        string // USER | AGENT | SYSTEM
	ID          string
	RunID       string
	AgentTaskID string
}

type Evidence struct {
	ID              string     `json:"id"`
	ModelID         string     `json:"model_id"`
	SourceRef       string     `json:"source_ref"`
	SourceType      string     `json:"source_type"`
	Directness      string     `json:"directness"`
	ContentHash     string     `json:"content_hash"`
	Excerpt         string     `json:"excerpt"`
	CapturedAt      time.Time  `json:"captured_at"`
	TrustLevel      string     `json:"trust_level"`
	AccessPolicy    string     `json:"access_policy"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	Status          string     `json:"status"`
	StatusReason    string     `json:"status_reason,omitempty"`
	StatusChangedAt *time.Time `json:"status_changed_at,omitempty"`
	CreatedByType   string     `json:"created_by_type"`
	CreatedBy       string     `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
	// Ref is the identifier GraphPatch evidence_refs use: "evidence:<id>".
	Ref string `json:"ref"`
}

// EvidenceInput captures new evidence. ContentHash is the SHA-256 (hex) of the source
// content; it is computed from Content when only Content is supplied.
type EvidenceInput struct {
	SourceRef    string     `json:"source_ref"`
	SourceType   string     `json:"source_type"`
	ContentHash  string     `json:"content_hash,omitempty"`
	Content      string     `json:"content,omitempty"`
	Excerpt      string     `json:"excerpt,omitempty"`
	CapturedAt   *time.Time `json:"captured_at,omitempty"`
	TrustLevel   string     `json:"trust_level"`
	AccessPolicy string     `json:"access_policy,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}

type Assumption struct {
	ID               string     `json:"id"`
	ModelID          string     `json:"model_id"`
	Statement        string     `json:"statement"`
	Rationale        string     `json:"rationale"`
	Owner            string     `json:"owner"`
	RiskIfWrong      string     `json:"risk_if_wrong"`
	ValidationMethod string     `json:"validation_method"`
	SubjectNodeIDs   []string   `json:"subject_node_ids"`
	Status           string     `json:"status"`
	StatusNote       string     `json:"status_note,omitempty"`
	StatusChangedBy  string     `json:"status_changed_by,omitempty"`
	StatusChangedAt  *time.Time `json:"status_changed_at,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	CreatedBy        string     `json:"created_by"`
	CreatedAt        time.Time  `json:"created_at"`
}

type AssumptionInput struct {
	Statement        string     `json:"statement"`
	Rationale        string     `json:"rationale"`
	Owner            string     `json:"owner,omitempty"` // defaults to the actor
	RiskIfWrong      string     `json:"risk_if_wrong"`
	ValidationMethod string     `json:"validation_method,omitempty"`
	SubjectNodeIDs   []string   `json:"subject_node_ids,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

// AssumptionStatusInput changes an assumption's status.
type AssumptionStatusInput struct {
	Status    string     `json:"status"` // VALIDATED | INVALIDATED | RETIRED | NEEDS_REVALIDATION
	Note      string     `json:"note"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"` // new expiry when VALIDATED
}

// EvidenceLink attaches evidence to a claim.
type EvidenceLink struct {
	EvidenceID string `json:"evidence_id"`
	Relation   string `json:"relation"` // SUPPORTS | CONTRADICTS | CONTEXT
	Note       string `json:"note,omitempty"`
}

type LinkedEvidence struct {
	Evidence
	Relation string    `json:"relation"`
	Note     string    `json:"note"`
	LinkedAt time.Time `json:"linked_at"`
}

type Claim struct {
	ID              string           `json:"id"`
	ModelID         string           `json:"model_id"`
	Statement       string           `json:"statement"`
	Class           string           `json:"claim_class"`
	Reasoning       string           `json:"reasoning"`
	ConfidenceBand  string           `json:"confidence_band"`
	ConfidenceScore *float64         `json:"confidence_score,omitempty"`
	SubjectNodeIDs  []string         `json:"subject_node_ids"`
	GraphVersion    int              `json:"graph_version"`
	ReviewStatus    string           `json:"review_status"`
	Reviewer        string           `json:"reviewer,omitempty"`
	ReviewNote      string           `json:"review_note,omitempty"`
	ReviewedAt      *time.Time       `json:"reviewed_at,omitempty"`
	Lifecycle       string           `json:"lifecycle"`
	StaleReason     string           `json:"stale_reason,omitempty"`
	StaleSince      *time.Time       `json:"stale_since,omitempty"`
	ExpiresAt       *time.Time       `json:"expires_at,omitempty"`
	RevalidateAfter string           `json:"revalidate_after,omitempty"`
	LastValidatedAt time.Time        `json:"last_validated_at"`
	SupersededBy    string           `json:"superseded_by,omitempty"`
	RetractedReason string           `json:"retracted_reason,omitempty"`
	CreatedByType   string           `json:"created_by_type"`
	CreatedBy       string           `json:"created_by"`
	CreatedAt       time.Time        `json:"created_at"`
	Evidence        []LinkedEvidence `json:"evidence"`
	Assumptions     []Assumption     `json:"assumptions"`
}

// ClaimInput creates a claim. Set RevalidateAfter (e.g. "720h") for a rolling expiry,
// or ExpiresAt for a fixed one. Supersedes replaces an existing claim (edits supersede).
type ClaimInput struct {
	Statement       string         `json:"statement"`
	Class           string         `json:"claim_class"`
	Reasoning       string         `json:"reasoning,omitempty"`
	ConfidenceBand  string         `json:"confidence_band"`
	ConfidenceScore *float64       `json:"confidence_score,omitempty"`
	SubjectNodeIDs  []string       `json:"subject_node_ids,omitempty"`
	Evidence        []EvidenceLink `json:"evidence,omitempty"`
	AssumptionIDs   []string       `json:"assumption_ids,omitempty"`
	ExpiresAt       *time.Time     `json:"expires_at,omitempty"`
	RevalidateAfter string         `json:"revalidate_after,omitempty"`
	Supersedes      string         `json:"supersedes,omitempty"`
}

// ReviewInput records an independent review.
type ReviewInput struct {
	Decision string `json:"decision"` // ACCEPTED | DISPUTED | REJECTED
	Note     string `json:"note"`
}

// RevalidateInput re-establishes a STALE (or soon-expiring) claim with fresh evidence.
type RevalidateInput struct {
	Evidence  []EvidenceLink `json:"evidence,omitempty"`
	ExpiresAt *time.Time     `json:"expires_at,omitempty"`
}

// Finding is a validation problem; any ERROR finding rejects the request (HTTP 422).
type Finding struct {
	Field     string `json:"field"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Invariant string `json:"invariant,omitempty"`
}

// ValidationError carries all findings for a rejected write.
type ValidationError struct{ Findings []Finding }

func (e *ValidationError) Error() string {
	if len(e.Findings) == 0 {
		return "validation failed"
	}
	return "validation failed: " + e.Findings[0].Message
}

// SweepResult reports what a revalidation sweep changed.
type SweepResult struct {
	EvidenceExpired          int `json:"evidence_expired"`
	AssumptionsNeedingReview int `json:"assumptions_needing_revalidation"`
	ClaimsMarkedStale        int `json:"claims_marked_stale"`
}
