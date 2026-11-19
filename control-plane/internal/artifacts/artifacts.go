// Package artifacts is the dependency engine's artifact side (spec §9.3): versioned
// projections of the architecture model bound to one graph version, with typed
// dependency edges and content hashes, invalidation, verification and revalidation.
package artifacts

import (
	"errors"
	"time"
)

var (
	ErrNotFound          = errors.New("artifact not found")
	ErrModelNotFound     = errors.New("architecture model not found")
	ErrInvalidInput      = errors.New("invalid request")
	ErrStaleGraphVersion = errors.New("graph_version is not the model's current version")
	ErrExists            = errors.New("an artifact with this name already exists")
	ErrRetired           = errors.New("artifact is retired")
)

// ProjectionTypes mirrors the database CHECK constraint.
var ProjectionTypes = []string{
	"REQUIREMENTS", "HLD", "LLD", "COMPONENT_CATALOGUE", "ERD", "SQL_DDL", "OPENAPI", "EVENT_SCHEMA",
	"SEQUENCE_DIAGRAM", "DEPLOYMENT_DIAGRAM", "THREAT_MODEL", "CAPACITY_PLAN", "ADR_EXPORT", "TEST_SUITE",
	"SIMULATION_REPORT", "OTHER",
}

// Node dependency scopes.
const (
	ScopeNode             = "NODE"                  // the node (and its outgoing edges) only
	ScopeNodeDependencies = "NODE_AND_DEPENDENCIES" // plus everything it transitively depends on
)

// Events.
const (
	EventVersionRecorded       = "artifact.version_recorded"
	EventInvalidated           = "artifact.invalidated" // same as graph.EventArtifactInvalidated
	EventRevalidationScheduled = "artifact.revalidation_scheduled"
	RevalidatorAgentType       = "artifact_revalidator"
)

// Actor identifies who records a version and the optional generation run.
type Actor struct {
	Type        string
	ID          string
	RunID       string
	AgentTaskID string
}

type Artifact struct {
	ID                string     `json:"id"`
	ModelID           string     `json:"model_id"`
	Name              string     `json:"name"`
	ProjectionType    string     `json:"projection_type"`
	OwnerType         string     `json:"owner_type"`
	OwnerID           string     `json:"owner_id"`
	Status            string     `json:"status"`
	CurrentVersionID  string     `json:"current_version_id"`
	StaleReason       string     `json:"stale_reason,omitempty"`
	StaleSince        *time.Time `json:"stale_since,omitempty"`
	StaleGraphVersion *int       `json:"stale_graph_version,omitempty"`
	RevalidationRunID string     `json:"revalidation_run_id,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	Current           *Version   `json:"current_version,omitempty"`
}

// Version is an immutable artifact version (6.2).
type Version struct {
	ID              string         `json:"id"`
	ArtifactID      string         `json:"artifact_id"`
	Number          int            `json:"version"`
	Checksum        string         `json:"checksum"`
	ContentRef      string         `json:"content_ref"`
	MediaType       string         `json:"media_type"`
	SizeBytes       int64          `json:"size_bytes"`
	GraphVersion    int            `json:"graph_version"`
	GraphHash       string         `json:"graph_hash"`
	GeneratedFrom   map[string]any `json:"generated_from"`
	GenerationRunID string         `json:"generation_run_id,omitempty"`
	AgentTaskID     string         `json:"agent_task_id,omitempty"`
	EvidenceRefs    []string       `json:"evidence_refs"`
	CreatedByType   string         `json:"created_by_type"`
	CreatedBy       string         `json:"created_by"`
	CreatedAt       time.Time      `json:"created_at"`
	Dependencies    []Dependency   `json:"dependencies"`
}

// Dependency is a typed dependency edge with the dependency's hash at binding (6.1).
type Dependency struct {
	Type       string `json:"dep_type"` // NODE | EDGE | ARTIFACT | DECISION | CLAIM
	TargetID   string `json:"target_id"`
	TargetHash string `json:"target_hash"`
	Via        string `json:"via"`
}

// NodeDependency binds a version to a node.
type NodeDependency struct {
	NodeID        string   `json:"node_id"`
	Scope         string   `json:"scope,omitempty"`          // NODE (default) | NODE_AND_DEPENDENCIES
	RelationTypes []string `json:"relation_types,omitempty"` // closure filter
	MaxDepth      int      `json:"max_depth,omitempty"`      // closure depth (default 10)
}

// Dependencies declares what a version was generated from.
type Dependencies struct {
	Nodes     []NodeDependency `json:"nodes,omitempty"`
	Artifacts []string         `json:"artifacts,omitempty"`
	Decisions []string         `json:"decisions,omitempty"`
	Claims    []string         `json:"claims,omitempty"`
}

// RecordInput records a new artifact version. Name and ProjectionType identify a new
// artifact; for an existing artifact they must match (or be empty).
type RecordInput struct {
	Name           string         `json:"name,omitempty"`
	ProjectionType string         `json:"projection_type,omitempty"`
	Checksum       string         `json:"checksum,omitempty"`
	Content        string         `json:"content,omitempty"` // hashed when checksum is omitted
	ContentRef     string         `json:"content_ref,omitempty"`
	MediaType      string         `json:"media_type,omitempty"`
	SizeBytes      int64          `json:"size_bytes,omitempty"`
	GeneratedFrom  map[string]any `json:"generated_from,omitempty"`
	EvidenceRefs   []string       `json:"evidence_refs,omitempty"`
	GraphVersion   *int           `json:"graph_version,omitempty"` // must be current when set
	Dependencies   Dependencies   `json:"dependencies"`
}

// RecordResult is a recorded version and what it invalidated downstream.
type RecordResult struct {
	Artifact              Artifact `json:"artifact"`
	Version               Version  `json:"version"`
	InvalidatedDownstream []string `json:"invalidated_downstream"`
	RevalidationRequestID string   `json:"revalidation_request_id,omitempty"`
}

// Finding is a validation problem; any finding rejects the write (HTTP 422).
type Finding struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ValidationError struct{ Findings []Finding }

func (e *ValidationError) Error() string {
	if len(e.Findings) == 0 {
		return "validation failed"
	}
	return "validation failed: " + e.Findings[0].Message
}

// Drift is a dependency whose recorded hash no longer matches.
type Drift struct {
	Dependency
	CurrentHash string `json:"current_hash,omitempty"` // empty: dependency no longer exists / usable
	Problem     string `json:"problem"`
}

// VerifyReport recomputes every dependency hash of the current version (deterministic).
type VerifyReport struct {
	ArtifactID          string  `json:"artifact_id"`
	VersionID           string  `json:"version_id"`
	BoundGraphVersion   int     `json:"bound_graph_version"`
	CurrentGraphVersion int     `json:"current_graph_version"`
	Fresh               bool    `json:"fresh"`
	Checked             int     `json:"dependencies_checked"`
	Drift               []Drift `json:"drift"`
}

// SweepResult reports a drift sweep.
type SweepResult struct {
	ArtifactsInvalidated int `json:"artifacts_invalidated"`
}

// ScheduleResult reports a revalidation scheduling pass.
type ScheduleResult struct {
	Scheduled int `json:"scheduled"`
	Failed    int `json:"failed"`
}
