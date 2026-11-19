package graph

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrIdempotencyConflict: the idempotency key was used with a different patch body.
	ErrIdempotencyConflict = errors.New("idempotency_key was already used with a different patch")
	ErrPatchNotFound       = errors.New("graph patch not found")
	ErrInvalidPatch        = errors.New("invalid graph patch")
)

// Limits.
const (
	MaxOperationsPerPatch = 1000
	maxIdempotencyKeyLen  = 255
	maxImpacted           = 500
)

// Patch is contracts/graph/graph-patch.v1.json.
type Patch struct {
	PatchID            string      `json:"patch_id,omitempty"`
	RunID              string      `json:"run_id,omitempty"`
	AgentTaskID        string      `json:"agent_task_id,omitempty"`
	BaseGraphVersion   *int        `json:"base_graph_version"`
	IdempotencyKey     string      `json:"idempotency_key"`
	ExpectedInvariants []string    `json:"expected_invariants,omitempty"`
	Operations         []Operation `json:"operations"`
}

// Operation is one GraphPatch operation. Payload is decoded strictly per op.
type Operation struct {
	Op           string          `json:"op"`
	TargetID     string          `json:"target_id"`
	Payload      json.RawMessage `json:"payload"`
	Rationale    string          `json:"rationale"`
	EvidenceRefs []string        `json:"evidence_refs,omitempty"`
}

const (
	OpAddNode    = "add_node"
	OpUpdateNode = "update_node"
	OpRemoveNode = "remove_node"
	OpAddEdge    = "add_edge"
	OpRemoveEdge = "remove_edge"
)

// AddNodePayload is the payload of add_node.
type AddNodePayload struct {
	Key            string         `json:"key"`
	Type           string         `json:"type"`
	Name           string         `json:"name"`
	Description    string         `json:"description,omitempty"`
	Properties     map[string]any `json:"properties,omitempty"`
	OwnerDomain    string         `json:"owner_domain,omitempty"`
	LifecycleState string         `json:"lifecycle_state,omitempty"`
	EvidenceState  string         `json:"evidence_state,omitempty"`
}

// UpdateNodePayload is the payload of update_node: only present fields change. A node's
// type is part of its identity and cannot be updated.
type UpdateNodePayload struct {
	Key            *string         `json:"key,omitempty"`
	Name           *string         `json:"name,omitempty"`
	Description    *string         `json:"description,omitempty"`
	Properties     *map[string]any `json:"properties,omitempty"` // replaces the whole object
	OwnerDomain    *string         `json:"owner_domain,omitempty"`
	LifecycleState *string         `json:"lifecycle_state,omitempty"`
	EvidenceState  *string         `json:"evidence_state,omitempty"`
}

// RemoveNodePayload is the payload of remove_node.
type RemoveNodePayload struct {
	// CascadeEdges also removes every live edge touching the node. Without it, a node
	// that still has edges is rejected so removals are always deliberate.
	CascadeEdges bool `json:"cascade_edges,omitempty"`
}

// AddEdgePayload is the payload of add_edge.
type AddEdgePayload struct {
	SourceNodeID string         `json:"source_node_id"`
	TargetNodeID string         `json:"target_node_id"`
	RelationType string         `json:"relation_type"`
	Direction    string         `json:"direction,omitempty"`
	Protocol     string         `json:"protocol,omitempty"`
	SyncMode     string         `json:"sync_mode,omitempty"`
	Constraints  map[string]any `json:"constraints,omitempty"`
	Properties   map[string]any `json:"properties,omitempty"`
}

// decodeStrict decodes a payload rejecting unknown fields; numbers stay json.Number so
// values round-trip exactly into JSONB and the canonical hash.
func decodeStrict(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after payload object")
	}
	return nil
}

// Fingerprint is the SHA-256 of the patch's semantic content (everything except
// patch_id and idempotency_key), used to detect idempotency-key reuse with a new body.
func (p Patch) Fingerprint() (string, error) {
	type op struct {
		Op           string          `json:"op"`
		TargetID     string          `json:"target_id"`
		Payload      json.RawMessage `json:"payload"`
		Rationale    string          `json:"rationale"`
		EvidenceRefs []string        `json:"evidence_refs"`
	}
	ops := make([]op, len(p.Operations))
	for i, o := range p.Operations {
		compact, err := canonicalJSON(o.Payload)
		if err != nil {
			return "", fmt.Errorf("operation %d payload: %w", i, err)
		}
		refs := o.EvidenceRefs
		if refs == nil {
			refs = []string{}
		}
		ops[i] = op{o.Op, o.TargetID, compact, o.Rationale, refs}
	}
	inv := p.ExpectedInvariants
	if inv == nil {
		inv = []string{}
	}
	base := -1
	if p.BaseGraphVersion != nil {
		base = *p.BaseGraphVersion
	}
	b, err := json.Marshal(struct {
		Base        int      `json:"base"`
		RunID       string   `json:"run_id"`
		AgentTaskID string   `json:"agent_task_id"`
		Invariants  []string `json:"expected_invariants"`
		Ops         []op     `json:"operations"`
	}{base, p.RunID, p.AgentTaskID, inv, ops})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalJSON re-encodes JSON with sorted object keys and no insignificant whitespace.
func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("{}"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	out, err := json.Marshal(v)
	return out, err
}

// Patch outcomes.
const (
	PatchAccepted = "ACCEPTED"
	PatchRejected = "REJECTED"
	PatchConflict = "CONFLICT"
)

// Finding is a validation problem attached to an operation (OpIndex -1: whole patch).
type Finding struct {
	OpIndex   int    `json:"op_index"`
	TargetID  string `json:"target_id,omitempty"`
	Code      string `json:"code"`
	Severity  string `json:"severity"` // ERROR | WARNING
	Message   string `json:"message"`
	Invariant string `json:"invariant,omitempty"`
}

// Conflict explains why a patch on a stale base cannot be applied as-is.
type Conflict struct {
	OpIndex          int    `json:"op_index"`
	TargetID         string `json:"target_id"`
	Reason           string `json:"reason"` // modified_since_base | removed_since_base | key_taken_since_base
	ChangedInVersion int    `json:"changed_in_version,omitempty"`
}

// ChangeSummary lists what a patch (or a stale-base window) changed.
type ChangeSummary struct {
	NodesAdded   []string `json:"nodes_added"`
	NodesUpdated []string `json:"nodes_updated"`
	NodesRemoved []string `json:"nodes_removed"`
	EdgesAdded   []string `json:"edges_added"`
	EdgesRemoved []string `json:"edges_removed"`
}

func newChangeSummary() ChangeSummary {
	return ChangeSummary{NodesAdded: []string{}, NodesUpdated: []string{}, NodesRemoved: []string{}, EdgesAdded: []string{}, EdgesRemoved: []string{}}
}

// ImpactedComponent is a node affected by a change (the changed node itself or anything
// that depends on it, found by an inbound traversal).
type ImpactedComponent struct {
	NodeID string `json:"node_id"`
	Key    string `json:"key"`
	Type   string `json:"type"`
}

// Result is contracts/graph/graph-patch-result.v1.json.
type Result struct {
	SchemaVersion        string         `json:"schema_version"`
	PatchID              string         `json:"patch_id"`
	ModelID              string         `json:"model_id"`
	Status               string         `json:"status"`
	Accepted             bool           `json:"accepted"`
	BaseGraphVersion     int            `json:"base_graph_version"`
	CurrentGraphVersion  int            `json:"current_graph_version"`
	NewGraphVersion      *int           `json:"new_graph_version"`
	GraphHash            string         `json:"graph_hash,omitempty"`
	Changes              ChangeSummary  `json:"changes"`
	Conflicts            []Conflict     `json:"conflicts"`
	ChangesSinceBase     *ChangeSummary `json:"changes_since_base,omitempty"`
	ValidationFindings   []Finding      `json:"validation_findings"`
	InvalidatedArtifacts []string       `json:"invalidated_artifacts"`
	// Invalidation details everything the dependency engine marked stale (ACCEPTED only).
	Invalidation       *Invalidation       `json:"invalidation,omitempty"`
	ImpactedComponents []ImpactedComponent `json:"impacted_components"`
	DecidedAt          time.Time           `json:"decided_at"`
	// Replayed is set when the result was returned for a repeated idempotency key.
	Replayed bool `json:"-"`
}
