// Package graph is the Architecture Intelligence Core's canonical graph store: one
// typed, versioned architecture model per project, stored as PostgreSQL adjacency
// tables with append-only temporal rows (see migrations/000004_architecture_graph).
//
// Versions are immutable and linear. A node or edge keeps one stable id for life; each
// revision is a row valid for [valid_from_version, valid_to_version), so the graph at
// any historical version is reconstructable and history is never rewritten.
package graph

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

var (
	ErrModelExists     = errors.New("project already has an architecture model")
	ErrModelNotFound   = errors.New("architecture model not found")
	ErrVersionNotFound = errors.New("graph version not found")
	ErrNodeNotFound    = errors.New("node not found at this version")
	ErrProjectNotFound = errors.New("project not found")
	ErrInvalidQuery    = errors.New("invalid graph query")
)

// EventVersionCreated is emitted (via the outbox) for every new graph version.
const EventVersionCreated = "architecture.graph.version.created"

// Node types and edge relation types (spec §23); the database CHECK constraints are
// authoritative, these lists are for validation and documentation.
var (
	NodeTypes = []string{
		"system", "domain", "service", "component", "api", "event", "topic", "database", "table",
		"cache", "queue", "actor", "deployment_unit", "region", "secret_boundary",
		"external_dependency", "slo", "threat", "workload", "test",
	}
	RelationTypes = []string{
		"owns", "calls", "publishes", "subscribes", "reads", "writes", "deploys_to",
		"depends_on", "authenticates", "replicates", "fails_over_to", "derived_from",
	}
)

// Author identifies who produced a version.
type Author struct {
	Type string `json:"type"` // USER | AGENT | SYSTEM
	ID   string `json:"id"`
}

type Model struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	ProjectID      string    `json:"project_id"`
	CurrentVersion int       `json:"current_version"`
	SchemaVersion  string    `json:"schema_version"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type Version struct {
	ID            string    `json:"id"`
	ModelID       string    `json:"model_id"`
	Number        int       `json:"version"`
	ParentID      string    `json:"parent_version_id,omitempty"`
	GraphHash     string    `json:"graph_hash"`
	NodeCount     int       `json:"node_count"`
	EdgeCount     int       `json:"edge_count"`
	ChangeSummary string    `json:"change_summary"`
	AuthorType    string    `json:"author_type"`
	AuthorID      string    `json:"author_id"`
	PatchID       string    `json:"patch_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

type Node struct {
	ID             string         `json:"id"`
	Key            string         `json:"key"`
	Type           string         `json:"type"`
	Name           string         `json:"name"`
	Description    string         `json:"description"`
	Properties     map[string]any `json:"properties"`
	OwnerDomain    string         `json:"owner_domain,omitempty"`
	LifecycleState string         `json:"lifecycle_state"`
	EvidenceState  string         `json:"evidence_state"`
	Revision       int            `json:"revision"`
	ValidFrom      int            `json:"valid_from_version"`
}

type Edge struct {
	ID           string         `json:"id"`
	SourceNodeID string         `json:"source_node_id"`
	TargetNodeID string         `json:"target_node_id"`
	RelationType string         `json:"relation_type"`
	Direction    string         `json:"direction"`
	Protocol     string         `json:"protocol,omitempty"`
	SyncMode     string         `json:"sync_mode,omitempty"`
	Constraints  map[string]any `json:"constraints"`
	Properties   map[string]any `json:"properties"`
	Revision     int            `json:"revision"`
	ValidFrom    int            `json:"valid_from_version"`
}

// Graph is the model's content at one version.
type Graph struct {
	Version Version `json:"version"`
	Nodes   []Node  `json:"nodes"`
	Edges   []Edge  `json:"edges"`
}

type Snapshot struct {
	ID            string    `json:"id"`
	ModelID       string    `json:"model_id"`
	VersionID     string    `json:"version_id"`
	VersionNumber int       `json:"version"`
	GraphHash     string    `json:"graph_hash"`
	Label         string    `json:"label"`
	PublishedBy   string    `json:"published_by"`
	PublishedAt   time.Time `json:"published_at"`
}

// Hash is the canonical content hash of a graph: SHA-256 over a versioned, sorted,
// whitespace-free encoding of every semantic field. Storage details (row ids,
// timestamps, revision numbers, validity ranges) are excluded, so identical graph
// content always hashes identically, whichever version or path produced it.
func Hash(nodes []Node, edges []Edge) string {
	type hn struct {
		ID, Key, Type, Name, Description string
		Properties                       map[string]any
		OwnerDomain, Lifecycle, Evidence string
	}
	type he struct {
		ID, Source, Target, Relation, Direction, Protocol, SyncMode string
		Constraints, Properties                                     map[string]any
	}
	ns := make([]hn, len(nodes))
	for i, n := range nodes {
		ns[i] = hn{n.ID, n.Key, n.Type, n.Name, n.Description, nonNil(n.Properties), n.OwnerDomain, n.LifecycleState, n.EvidenceState}
	}
	es := make([]he, len(edges))
	for i, e := range edges {
		es[i] = he{e.ID, e.SourceNodeID, e.TargetNodeID, e.RelationType, e.Direction, e.Protocol, e.SyncMode, nonNil(e.Constraints), nonNil(e.Properties)}
	}
	sort.Slice(ns, func(i, j int) bool { return ns[i].ID < ns[j].ID })
	sort.Slice(es, func(i, j int) bool { return es[i].ID < es[j].ID })

	var buf bytes.Buffer
	buf.WriteString("systera.graph.v1\n")
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(ns) // maps marshal with sorted keys
	_ = enc.Encode(es)
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

// ComputeHash recomputes the canonical hash of g's content.
func (g Graph) ComputeHash() string { return Hash(g.Nodes, g.Edges) }

func nonNil(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// Direction selects how Reachable follows edges.
type Direction string

const (
	Outbound Direction = "out"  // follow edges source -> target (what does X depend on)
	Inbound  Direction = "in"   // follow edges target -> source (what depends on X: impact)
	Both     Direction = "both" // undirected
)

// TraversalQuery bounds a reachability query.
type TraversalQuery struct {
	Direction     Direction
	RelationTypes []string // empty: all relation types
	MaxNodes      int      // result cap (default 500, max 10000)
}

// TraversalResult lists nodes reachable from the start node (excluding it).
type TraversalResult struct {
	Start     string `json:"start_node_id"`
	Version   int    `json:"version"`
	Nodes     []Node `json:"nodes"`
	Truncated bool   `json:"truncated"`
}
