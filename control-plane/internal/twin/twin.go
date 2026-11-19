// Package twin builds the design-time Architecture Digital Twin (spec §23.4, §26): a
// normalized, executable model of one architecture graph version plus explicit workload,
// environment and failure assumptions, validated across six dimensions (structure,
// interfaces, data, runtime, behaviour, quality attributes).
//
// The twin describes declared design intent. It never reads telemetry and never presents
// a value as observed: every runtime parameter carries its source (DECLARED on the graph,
// ASSUMED from the build's assumptions, or DEFAULT when neither stated it).
package twin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/systera/control-plane/internal/graph"
)

// BuilderVersion is recorded on every build; a new version can produce a different twin
// for the same inputs, so it is part of the build's identity.
const BuilderVersion = "twin-builder/1.0.0"

// Basis labels every twin and report: the content is declared design, not runtime fact.
const Basis = "DESIGN_INTENT"

// Disclaimer is shown with every twin and validation report.
const Disclaimer = "Design-time twin built from the declared architecture and the stated assumptions. " +
	"It describes intended design, not observed production behaviour: no telemetry was read and nothing here is a measurement."

// ErrInvalidInput reports malformed assumptions.
var ErrInvalidInput = errors.New("invalid twin input")

// Value sources.
const (
	SourceDeclared = "DECLARED" // stated on the graph node or edge
	SourceAssumed  = "ASSUMED"  // stated in this build's assumptions
	SourceDefault  = "DEFAULT"  // neither stated it; the builder's conservative default
	SourceMissing  = "MISSING"  // unknown; checks that need it report NOT_APPLICABLE
)

// FailureKinds are the failure assumptions a twin can carry for later engines.
var FailureKinds = []string{"NODE_LOSS", "ZONE_LOSS", "REGION_LOSS", "DEPENDENCY_OUTAGE", "NETWORK_PARTITION", "LATENCY_SPIKE"}

// Assumptions are the explicit inputs, beyond the graph, that a twin is built with.
type Assumptions struct {
	Workload    Workload            `json:"workload"`
	Environment Environment         `json:"environment"`
	Failures    []FailureAssumption `json:"failures"`
	Notes       string              `json:"notes,omitempty"`
}

// Workload describes the traffic the design must carry.
type Workload struct {
	PeakRPS           float64  `json:"peak_rps"`                       // 0 = not stated
	ReadRatio         *float64 `json:"read_ratio,omitempty"`           // fraction of requests that only read, 0..1
	AvgPayloadKB      float64  `json:"avg_payload_kb,omitempty"`       // mean request+response size
	GrowthPctPerMonth float64  `json:"growth_pct_per_month,omitempty"` // expected traffic growth
}

// Environment describes where the design runs.
type Environment struct {
	Cloud            string   `json:"cloud,omitempty"`
	Regions          []string `json:"regions"`
	DefaultReplicas  int      `json:"default_replicas,omitempty"`   // replicas for units that declare none (0 = 1, DEFAULT)
	DefaultTimeoutMS int      `json:"default_timeout_ms,omitempty"` // timeout for sync calls that declare none (0 = none)
}

// FailureAssumption is a fault later engines inject; the builder only validates it.
type FailureAssumption struct {
	Kind         string `json:"kind"`
	TargetNodeID string `json:"target_node_id,omitempty"`
	Note         string `json:"note,omitempty"`
}

// NodeRef identifies a node in the twin.
type NodeRef struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// Twin is the normalized, engine-ready model.
type Twin struct {
	ModelID         string      `json:"model_id"`
	GraphVersion    int         `json:"graph_version"`
	GraphHash       string      `json:"graph_hash"`
	Basis           string      `json:"basis"`
	Disclaimer      string      `json:"disclaimer"`
	BuilderVersion  string      `json:"builder_version"`
	AssumptionsHash string      `json:"assumptions_hash"`
	Assumptions     Assumptions `json:"assumptions"`

	Structure  Structure      `json:"structure"`
	Interfaces []Interface    `json:"interfaces"`
	Data       []DataStore    `json:"data"`
	Runtime    Runtime        `json:"runtime"`
	Behaviour  Behaviour      `json:"behaviour"`
	Quality    Quality        `json:"quality"`
	Defaults   []AppliedValue `json:"defaults_applied"`
}

// Structure: services, modules, ownership, dependencies and boundaries.
type Structure struct {
	Components []Component `json:"components"`
	Boundaries []NodeRef   `json:"boundaries"` // systems, domains, secret boundaries
	Edges      int         `json:"edge_count"`
}

type Component struct {
	NodeRef
	Owner          string   `json:"owner,omitempty"`
	OwnerSource    string   `json:"owner_source"` // OWNER_DOMAIN | OWNS_EDGE | MISSING
	Critical       bool     `json:"critical"`
	Lifecycle      string   `json:"lifecycle_state"`
	EvidenceState  string   `json:"evidence_state"`
	DependsOn      []string `json:"depends_on"` // node ids (calls, depends_on, reads, writes, publishes)
	DependentCount int      `json:"dependent_count"`
}

// Interface: APIs, events and messaging channels with their contracts.
type Interface struct {
	NodeRef
	Kind          string    `json:"kind"` // API | EVENT | CHANNEL
	Protocols     []string  `json:"protocols"`
	Version       string    `json:"version,omitempty"`
	SchemaRef     string    `json:"schema_ref,omitempty"`
	Providers     []NodeRef `json:"providers"` // owners (API) or publishers (EVENT/CHANNEL)
	Consumers     []NodeRef `json:"consumers"` // callers (API) or subscribers (EVENT/CHANNEL)
	ContractBasis string    `json:"contract_basis"`
}

// DataStore: ownership, access patterns, consistency, retention and redundancy.
type DataStore struct {
	NodeRef
	Writers     []NodeRef `json:"writers"`
	Readers     []NodeRef `json:"readers"`
	Owner       *NodeRef  `json:"owner,omitempty"` // the single writer, when there is exactly one
	Consistency string    `json:"consistency,omitempty"`
	Retention   string    `json:"retention,omitempty"`
	Replicas    []NodeRef `json:"replicates_to"`
	FailoverTo  []NodeRef `json:"fails_over_to"`
}

// Runtime: deployment units, replicas, regions and placement.
type Runtime struct {
	Units           []RuntimeUnit `json:"units"`
	DeploymentUnits []NodeRef     `json:"deployment_units"`
	Regions         []string      `json:"regions"`
	RegionsSource   string        `json:"regions_source"`
	TopologyModeled bool          `json:"topology_modeled"`
}

type RuntimeUnit struct {
	NodeRef
	Replicas       int       `json:"replicas"`
	ReplicasSource string    `json:"replicas_source"`
	CapacityRPS    float64   `json:"capacity_rps_per_replica,omitempty"`
	CapacitySource string    `json:"capacity_source"`
	PlacedOn       []NodeRef `json:"placed_on"`
	Regions        []string  `json:"regions"`
}

// Behaviour: interactions, request paths, retries and timeouts.
type Behaviour struct {
	Interactions []Interaction `json:"interactions"`
	EntryPoints  []NodeRef     `json:"entry_points"`
	Paths        []RequestPath `json:"request_paths"`
	PathsCapped  bool          `json:"paths_capped"`
}

type Interaction struct {
	EdgeID         string  `json:"edge_id"`
	From           NodeRef `json:"from"`
	To             NodeRef `json:"to"`
	Relation       string  `json:"relation"`
	Protocol       string  `json:"protocol,omitempty"`
	SyncMode       string  `json:"sync_mode"`
	SyncSource     string  `json:"sync_source"`
	TimeoutMS      int     `json:"timeout_ms,omitempty"`
	TimeoutSource  string  `json:"timeout_source"`
	Retries        int     `json:"retries"`
	RetriesSource  string  `json:"retries_source"`
	Backoff        bool    `json:"backoff"`
	CircuitBreaker bool    `json:"circuit_breaker"`
}

// RequestPath is one synchronous request path from an entry point, following calls
// (through an API to the service that provides it) until data access or an async boundary.
type RequestPath struct {
	Entry              NodeRef   `json:"entry"`
	Nodes              []NodeRef `json:"nodes"`
	EdgeIDs            []string  `json:"edge_ids"`
	SyncHops           int       `json:"sync_hops"`
	TimeoutBudgetMS    int       `json:"timeout_budget_ms,omitempty"` // worst case incl. retries; 0 when any hop lacks a timeout
	RetryAmplification int       `json:"retry_amplification"`         // product of (retries+1) over sync hops
	EndsAt             string    `json:"ends_at"`                     // DATA | ASYNC | LEAF | CYCLE | DEPTH_LIMIT
	CoveredBy          []string  `json:"covered_by_slo_ids"`
}

// Quality: SLOs, RTO/RPO and what they cover.
type Quality struct {
	SLOs          []SLO   `json:"slos"`
	PathsCovered  int     `json:"paths_covered"`
	PathsTotal    int     `json:"paths_total"`
	CoveragePct   float64 `json:"coverage_pct"`
	UnitsCovered  int     `json:"runtime_units_covered"`
	UnitsOnPaths  int     `json:"runtime_units_on_paths"`
	TargetAvail   float64 `json:"strictest_availability_pct,omitempty"`
	HasRecoveryTo bool    `json:"has_rto_or_rpo"`
}

type SLO struct {
	NodeRef
	AvailabilityPct float64   `json:"availability_pct,omitempty"`
	LatencyP95MS    float64   `json:"latency_p95_ms,omitempty"`
	LatencyP99MS    float64   `json:"latency_p99_ms,omitempty"`
	RTOMinutes      float64   `json:"rto_minutes,omitempty"`
	RPOMinutes      float64   `json:"rpo_minutes,omitempty"`
	Covers          []NodeRef `json:"covers"`
}

// AppliedValue records a value the builder filled in, so nothing assumed is hidden.
type AppliedValue struct {
	Subject NodeRef `json:"subject"`
	Field   string  `json:"field"`
	Value   any     `json:"value"`
	Source  string  `json:"source"`
	Reason  string  `json:"reason"`
}

const (
	maxPaths     = 200
	maxPathDepth = 12
)

// NormalizeAssumptions validates and canonicalizes assumptions against the graph, and
// returns the canonical form and its hash (the build's reproducibility key).
func NormalizeAssumptions(a Assumptions, g graph.Graph) (Assumptions, string, error) {
	var problems []string
	w := a.Workload
	if w.PeakRPS < 0 || math.IsNaN(w.PeakRPS) || math.IsInf(w.PeakRPS, 0) {
		problems = append(problems, "workload.peak_rps must be zero or positive")
	}
	if w.ReadRatio != nil && (*w.ReadRatio < 0 || *w.ReadRatio > 1) {
		problems = append(problems, "workload.read_ratio must be between 0 and 1")
	}
	if w.AvgPayloadKB < 0 {
		problems = append(problems, "workload.avg_payload_kb must be zero or positive")
	}
	if w.GrowthPctPerMonth < -100 || w.GrowthPctPerMonth > 1000 {
		problems = append(problems, "workload.growth_pct_per_month must be between -100 and 1000")
	}
	e := a.Environment
	e.Cloud = strings.TrimSpace(e.Cloud)
	if e.DefaultReplicas < 0 || e.DefaultReplicas > 1000 {
		problems = append(problems, "environment.default_replicas must be between 0 and 1000")
	}
	if e.DefaultTimeoutMS < 0 || e.DefaultTimeoutMS > 600000 {
		problems = append(problems, "environment.default_timeout_ms must be between 0 and 600000")
	}
	regions := []string{}
	for _, r := range e.Regions {
		r = strings.TrimSpace(r)
		if r != "" && !slices.Contains(regions, r) {
			regions = append(regions, r)
		}
	}
	sort.Strings(regions)
	if len(regions) > 20 {
		problems = append(problems, "environment.regions allows at most 20 regions")
	}
	e.Regions = regions

	nodes := map[string]bool{}
	for _, n := range g.Nodes {
		nodes[n.ID] = true
	}
	failures := []FailureAssumption{}
	if len(a.Failures) > 50 {
		problems = append(problems, "at most 50 failure assumptions")
	}
	for i, f := range a.Failures {
		f.Kind = strings.ToUpper(strings.TrimSpace(f.Kind))
		f.TargetNodeID = strings.TrimSpace(f.TargetNodeID)
		f.Note = strings.TrimSpace(f.Note)
		if !slices.Contains(FailureKinds, f.Kind) {
			problems = append(problems, fmt.Sprintf("failures[%d].kind must be one of %s", i, strings.Join(FailureKinds, ", ")))
		}
		if f.TargetNodeID != "" && !nodes[f.TargetNodeID] {
			problems = append(problems, fmt.Sprintf("failures[%d].target_node_id is not a node at graph version %d", i, g.Version.Number))
		}
		if (f.Kind == "NODE_LOSS" || f.Kind == "DEPENDENCY_OUTAGE") && f.TargetNodeID == "" {
			problems = append(problems, fmt.Sprintf("failures[%d] (%s) needs a target_node_id", i, f.Kind))
		}
		if len(f.Note) > 500 {
			problems = append(problems, fmt.Sprintf("failures[%d].note must be at most 500 characters", i))
		}
		failures = append(failures, f)
	}
	sort.SliceStable(failures, func(i, j int) bool {
		if failures[i].Kind != failures[j].Kind {
			return failures[i].Kind < failures[j].Kind
		}
		return failures[i].TargetNodeID < failures[j].TargetNodeID
	})
	notes := strings.TrimSpace(a.Notes)
	if len(notes) > 2000 {
		problems = append(problems, "notes must be at most 2000 characters")
	}
	if len(problems) > 0 {
		return Assumptions{}, "", fmt.Errorf("%w: %s", ErrInvalidInput, strings.Join(problems, "; "))
	}
	out := Assumptions{Workload: w, Environment: e, Failures: failures, Notes: notes}
	raw, _ := json.Marshal(out)
	sum := sha256.Sum256(raw)
	return out, hex.EncodeToString(sum[:]), nil
}

// Build turns a graph version plus canonical assumptions into a twin. It is deterministic:
// the same graph hash and assumptions always produce the same twin.
func Build(modelID string, g graph.Graph, a Assumptions, assumptionsHash string) Twin {
	ix := newIndex(g)
	t := Twin{
		ModelID: modelID, GraphVersion: g.Version.Number, GraphHash: g.Version.GraphHash,
		Basis: Basis, Disclaimer: Disclaimer, BuilderVersion: BuilderVersion,
		AssumptionsHash: assumptionsHash, Assumptions: a, Defaults: []AppliedValue{},
	}
	t.Structure = buildStructure(ix)
	t.Interfaces = buildInterfaces(ix)
	t.Data = buildData(ix)
	t.Runtime = buildRuntime(ix, a, &t.Defaults)
	t.Behaviour = buildBehaviour(ix, a, &t.Defaults)
	t.Quality = buildQuality(ix, &t.Behaviour)
	return t
}

// ---- graph index ----

type index struct {
	g     graph.Graph
	nodes map[string]graph.Node
	order []string // node ids sorted by key for deterministic output
	out   map[string][]graph.Edge
	in    map[string][]graph.Edge
}

func newIndex(g graph.Graph) *index {
	ix := &index{g: g, nodes: map[string]graph.Node{}, out: map[string][]graph.Edge{}, in: map[string][]graph.Edge{}}
	for _, n := range g.Nodes {
		ix.nodes[n.ID] = n
		ix.order = append(ix.order, n.ID)
	}
	sort.Slice(ix.order, func(i, j int) bool {
		a, b := ix.nodes[ix.order[i]], ix.nodes[ix.order[j]]
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		return a.ID < b.ID
	})
	edges := slices.Clone(g.Edges)
	sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })
	for _, e := range edges {
		if _, ok := ix.nodes[e.SourceNodeID]; !ok {
			continue
		}
		if _, ok := ix.nodes[e.TargetNodeID]; !ok {
			continue
		}
		ix.out[e.SourceNodeID] = append(ix.out[e.SourceNodeID], e)
		ix.in[e.TargetNodeID] = append(ix.in[e.TargetNodeID], e)
	}
	return ix
}

func (ix *index) ref(id string) NodeRef {
	n := ix.nodes[id]
	return NodeRef{ID: n.ID, Key: n.Key, Name: n.Name, Type: n.Type}
}

func (ix *index) ofType(types ...string) []graph.Node {
	var out []graph.Node
	for _, id := range ix.order {
		if slices.Contains(types, ix.nodes[id].Type) {
			out = append(out, ix.nodes[id])
		}
	}
	return out
}

// related returns distinct nodes linked to id by the given relations: outgoing when out is
// true (id is the source), incoming otherwise.
func (ix *index) related(id string, out bool, relations ...string) []NodeRef {
	edges := ix.in[id]
	if out {
		edges = ix.out[id]
	}
	seen := map[string]bool{}
	refs := []NodeRef{}
	for _, e := range edges {
		if len(relations) > 0 && !slices.Contains(relations, e.RelationType) {
			continue
		}
		other := e.SourceNodeID
		if out {
			other = e.TargetNodeID
		}
		if !seen[other] {
			seen[other] = true
			refs = append(refs, ix.ref(other))
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Key < refs[j].Key })
	return refs
}

func (ix *index) degree(id string) int { return len(ix.in[id]) + len(ix.out[id]) }

// providerOf returns the services/components that own an API (owns edge into it).
func (ix *index) providerOf(apiID string) []string {
	var out []string
	for _, e := range ix.in[apiID] {
		if e.RelationType == "owns" && slices.Contains([]string{"service", "component"}, ix.nodes[e.SourceNodeID].Type) {
			out = append(out, e.SourceNodeID)
		}
	}
	return out
}

// ---- property readers (values come back from JSONB as float64, from patches as json.Number) ----

func prop(m map[string]any, keys ...string) (any, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v, true
		}
	}
	return nil, false
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(x), "%"), "ms"))
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	}
	return 0, false
}

func num(m map[string]any, keys ...string) (float64, bool) {
	v, ok := prop(m, keys...)
	if !ok {
		return 0, false
	}
	return toFloat(v)
}

func str(m map[string]any, keys ...string) string {
	v, ok := prop(m, keys...)
	if !ok {
		return ""
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

func truthy(m map[string]any, keys ...string) bool {
	v, ok := prop(m, keys...)
	if !ok {
		return false
	}
	switch x := v.(type) {
	case bool:
		return x
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		return s != "" && s != "false" && s != "none" && s != "no" && s != "0"
	default:
		f, ok := toFloat(x)
		return ok && f != 0
	}
}

// edgeAttr reads an edge attribute from constraints first, then properties.
func edgeNum(e graph.Edge, keys ...string) (float64, bool) {
	if f, ok := num(e.Constraints, keys...); ok {
		return f, true
	}
	return num(e.Properties, keys...)
}

func edgeTruthy(e graph.Edge, keys ...string) bool {
	return truthy(e.Constraints, keys...) || truthy(e.Properties, keys...)
}

func isCritical(n graph.Node) bool {
	if truthy(n.Properties, "critical") {
		return true
	}
	switch strings.ToLower(str(n.Properties, "tier", "criticality")) {
	case "critical", "tier0", "tier-0", "tier 0", "0", "high", "p0":
		return true
	}
	return false
}

// ---- dimensions ----

var componentTypes = []string{"service", "component", "external_dependency", "actor", "workload", "system", "domain"}

func buildStructure(ix *index) Structure {
	s := Structure{Components: []Component{}, Boundaries: []NodeRef{}, Edges: len(ix.g.Edges)}
	for _, n := range ix.ofType("system", "domain", "secret_boundary") {
		s.Boundaries = append(s.Boundaries, ix.ref(n.ID))
	}
	for _, n := range ix.ofType(componentTypes...) {
		if n.Type == "system" || n.Type == "domain" {
			continue
		}
		c := Component{NodeRef: ix.ref(n.ID), Critical: isCritical(n), Lifecycle: n.LifecycleState, EvidenceState: n.EvidenceState,
			DependsOn: []string{}, OwnerSource: SourceMissing}
		if n.OwnerDomain != "" {
			c.Owner, c.OwnerSource = n.OwnerDomain, "OWNER_DOMAIN"
		} else if owners := ix.related(n.ID, false, "owns"); len(owners) > 0 {
			c.Owner, c.OwnerSource = owners[0].Name, "OWNS_EDGE"
		}
		for _, r := range ix.related(n.ID, true, "calls", "depends_on", "reads", "writes", "publishes", "subscribes") {
			c.DependsOn = append(c.DependsOn, r.ID)
		}
		c.DependentCount = len(ix.related(n.ID, false, "calls", "depends_on"))
		s.Components = append(s.Components, c)
	}
	return s
}

func buildInterfaces(ix *index) []Interface {
	out := []Interface{}
	for _, n := range ix.ofType("api", "event", "topic", "queue") {
		it := Interface{NodeRef: ix.ref(n.ID), Protocols: []string{}}
		it.Version = str(n.Properties, "version", "api_version", "schema_version")
		it.SchemaRef = str(n.Properties, "schema_ref", "schema", "openapi", "openapi_ref", "asyncapi", "contract", "contract_ref")
		protos := map[string]bool{}
		if p := str(n.Properties, "protocol"); p != "" {
			protos[p] = true
		}
		switch n.Type {
		case "api":
			it.Kind = "API"
			it.Providers = ix.related(n.ID, false, "owns")
			it.Consumers = ix.related(n.ID, false, "calls")
			for _, e := range ix.in[n.ID] {
				if e.Protocol != "" {
					protos[e.Protocol] = true
				}
			}
		default:
			it.Kind = "CHANNEL"
			if n.Type == "event" {
				it.Kind = "EVENT"
			}
			it.Providers = ix.related(n.ID, false, "publishes")
			it.Consumers = ix.related(n.ID, false, "subscribes")
		}
		for p := range protos {
			it.Protocols = append(it.Protocols, p)
		}
		sort.Strings(it.Protocols)
		switch {
		case it.Version != "" && it.SchemaRef != "":
			it.ContractBasis = "VERSIONED_SCHEMA"
		case it.SchemaRef != "":
			it.ContractBasis = "SCHEMA_ONLY"
		case it.Version != "":
			it.ContractBasis = "VERSION_ONLY"
		default:
			it.ContractBasis = "UNDECLARED"
		}
		out = append(out, it)
	}
	return out
}

func buildData(ix *index) []DataStore {
	out := []DataStore{}
	for _, n := range ix.ofType("database", "table", "cache") {
		d := DataStore{NodeRef: ix.ref(n.ID),
			Writers:     ix.related(n.ID, false, "writes"),
			Readers:     ix.related(n.ID, false, "reads"),
			Consistency: str(n.Properties, "consistency", "consistency_model", "isolation"),
			Retention:   str(n.Properties, "retention", "retention_days", "ttl"),
			Replicas:    ix.related(n.ID, true, "replicates"),
			FailoverTo:  ix.related(n.ID, true, "fails_over_to"),
		}
		if len(d.Writers) == 1 {
			w := d.Writers[0]
			d.Owner = &w
		}
		out = append(out, d)
	}
	return out
}

var runtimeUnitTypes = []string{"service", "component", "database", "cache", "queue", "topic"}

func buildRuntime(ix *index, a Assumptions, applied *[]AppliedValue) Runtime {
	rt := Runtime{Units: []RuntimeUnit{}, DeploymentUnits: []NodeRef{}, Regions: []string{}}
	regionSet := map[string]bool{}
	for _, n := range ix.ofType("deployment_unit") {
		rt.DeploymentUnits = append(rt.DeploymentUnits, ix.ref(n.ID))
	}
	for _, n := range ix.ofType("region") {
		regionSet[n.Name] = true
	}
	rt.TopologyModeled = len(rt.DeploymentUnits) > 0 || len(regionSet) > 0

	defaultReplicas, defaultSource := a.Environment.DefaultReplicas, SourceAssumed
	if defaultReplicas == 0 {
		defaultReplicas, defaultSource = 1, SourceDefault
	}
	for _, n := range ix.ofType(runtimeUnitTypes...) {
		u := RuntimeUnit{NodeRef: ix.ref(n.ID), PlacedOn: []NodeRef{}, Regions: []string{}, CapacitySource: SourceMissing}
		replicas, src := 0, ""
		if f, ok := num(n.Properties, "replicas", "replica_count", "instances", "min_replicas"); ok && f >= 1 {
			replicas, src = int(f), SourceDeclared
		}
		placed := map[string]bool{}
		regions := map[string]bool{}
		for _, e := range ix.out[n.ID] {
			if e.RelationType != "deploys_to" {
				continue
			}
			t := ix.nodes[e.TargetNodeID]
			if !placed[t.ID] {
				placed[t.ID] = true
				u.PlacedOn = append(u.PlacedOn, ix.ref(t.ID))
			}
			if t.Type == "region" {
				regions[t.Name] = true
			}
			if t.Type == "deployment_unit" {
				if replicas == 0 {
					if f, ok := num(t.Properties, "replicas", "replica_count", "instances", "min_replicas"); ok && f >= 1 {
						replicas, src = int(f), SourceDeclared
					}
				}
				for _, e2 := range ix.out[t.ID] {
					if e2.RelationType == "deploys_to" && ix.nodes[e2.TargetNodeID].Type == "region" {
						regions[ix.nodes[e2.TargetNodeID].Name] = true
					}
				}
				if r := str(t.Properties, "region"); r != "" {
					regions[r] = true
				}
			}
		}
		if r := str(n.Properties, "region"); r != "" {
			regions[r] = true
		}
		if replicas == 0 {
			replicas, src = defaultReplicas, defaultSource
			if n.Type == "service" || n.Type == "component" {
				*applied = append(*applied, AppliedValue{Subject: u.NodeRef, Field: "replicas", Value: replicas, Source: src,
					Reason: "no replica count declared on the node or its deployment unit"})
			}
		}
		u.Replicas, u.ReplicasSource = replicas, src
		if f, ok := num(n.Properties, "capacity_rps", "max_rps", "rps_per_replica", "throughput_rps"); ok && f > 0 {
			u.CapacityRPS, u.CapacitySource = f, SourceDeclared
		}
		for r := range regions {
			u.Regions = append(u.Regions, r)
			regionSet[r] = true
		}
		sort.Strings(u.Regions)
		rt.Units = append(rt.Units, u)
	}
	for r := range regionSet {
		rt.Regions = append(rt.Regions, r)
	}
	sort.Strings(rt.Regions)
	switch {
	case len(rt.Regions) > 0:
		rt.RegionsSource = SourceDeclared
	case len(a.Environment.Regions) > 0:
		rt.Regions, rt.RegionsSource = slices.Clone(a.Environment.Regions), SourceAssumed
	default:
		rt.RegionsSource = SourceMissing
	}
	return rt
}

func isSyncCall(it Interaction) bool { return it.Relation == "calls" && it.SyncMode == "SYNC" }

func buildBehaviour(ix *index, a Assumptions, applied *[]AppliedValue) Behaviour {
	b := Behaviour{Interactions: []Interaction{}, EntryPoints: []NodeRef{}, Paths: []RequestPath{}}
	byEdge := map[string]Interaction{}
	ids := make([]string, 0, len(ix.g.Edges))
	for _, e := range ix.g.Edges {
		ids = append(ids, e.ID)
	}
	sort.Strings(ids)
	edgeByID := map[string]graph.Edge{}
	for _, e := range ix.g.Edges {
		edgeByID[e.ID] = e
	}
	for _, id := range ids {
		e := edgeByID[id]
		if !slices.Contains([]string{"calls", "publishes", "subscribes", "reads", "writes"}, e.RelationType) {
			continue
		}
		if _, ok := ix.nodes[e.SourceNodeID]; !ok {
			continue
		}
		if _, ok := ix.nodes[e.TargetNodeID]; !ok {
			continue
		}
		it := Interaction{EdgeID: e.ID, From: ix.ref(e.SourceNodeID), To: ix.ref(e.TargetNodeID), Relation: e.RelationType,
			Protocol: e.Protocol, SyncMode: e.SyncMode, SyncSource: SourceDeclared, TimeoutSource: SourceMissing, RetriesSource: SourceDefault}
		if it.SyncMode == "" {
			it.SyncSource = SourceDefault
			switch e.RelationType {
			case "publishes", "subscribes":
				it.SyncMode = "ASYNC"
			default:
				it.SyncMode = "SYNC"
			}
		}
		if f, ok := edgeNum(e, "timeout_ms", "timeout"); ok && f > 0 {
			it.TimeoutMS, it.TimeoutSource = int(f), SourceDeclared
		} else if isSyncCall(it) && a.Environment.DefaultTimeoutMS > 0 {
			it.TimeoutMS, it.TimeoutSource = a.Environment.DefaultTimeoutMS, SourceAssumed
			*applied = append(*applied, AppliedValue{Subject: it.To, Field: "timeout_ms (" + it.From.Name + " → " + it.To.Name + ")",
				Value: it.TimeoutMS, Source: SourceAssumed, Reason: "sync call declares no timeout; environment.default_timeout_ms applied"})
		}
		if f, ok := edgeNum(e, "retries", "max_retries", "retry_attempts"); ok && f >= 0 {
			it.Retries, it.RetriesSource = int(f), SourceDeclared
		}
		it.Backoff = edgeTruthy(e, "backoff", "retry_backoff", "exponential_backoff", "jitter")
		it.CircuitBreaker = edgeTruthy(e, "circuit_breaker", "breaker")
		b.Interactions = append(b.Interactions, it)
		byEdge[e.ID] = it
	}
	b.EntryPoints, b.Paths, b.PathsCapped = requestPaths(ix, byEdge)
	return b
}

// entryPoints: actors and workloads that call something; otherwise APIs nobody inside the
// model calls; otherwise services nobody calls.
func entryPoints(ix *index) []string {
	var out []string
	for _, n := range ix.ofType("actor", "workload") {
		for _, e := range ix.out[n.ID] {
			if e.RelationType == "calls" {
				out = append(out, n.ID)
				break
			}
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, n := range ix.ofType("api") {
		if len(ix.related(n.ID, false, "calls")) == 0 && len(ix.providerOf(n.ID)) > 0 {
			out = append(out, n.ID)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, n := range ix.ofType("service", "component") {
		if len(ix.related(n.ID, false, "calls")) == 0 {
			hasOut := false
			for _, e := range ix.out[n.ID] {
				hasOut = hasOut || e.RelationType == "calls"
			}
			if hasOut {
				out = append(out, n.ID)
			}
		}
	}
	return out
}

func requestPaths(ix *index, byEdge map[string]Interaction) ([]NodeRef, []RequestPath, bool) {
	entries := entryPoints(ix)
	refs := []NodeRef{}
	paths := []RequestPath{}
	capped := false
	for _, id := range entries {
		refs = append(refs, ix.ref(id))
	}

	type hop struct {
		node string
		edge string // "" for the implicit api → provider step
	}
	var walk func(cur string, trail []hop, onPath map[string]bool)
	emit := func(trail []hop, ends string) {
		if len(paths) >= maxPaths {
			capped = true
			return
		}
		p := RequestPath{Entry: ix.ref(trail[0].node), Nodes: []NodeRef{}, EdgeIDs: []string{}, RetryAmplification: 1, EndsAt: ends, CoveredBy: []string{}}
		budgetOK := true
		budget := 0
		for _, h := range trail {
			p.Nodes = append(p.Nodes, ix.ref(h.node))
			if h.edge == "" {
				continue
			}
			p.EdgeIDs = append(p.EdgeIDs, h.edge)
			it := byEdge[h.edge]
			if isSyncCall(it) {
				p.SyncHops++
				p.RetryAmplification *= it.Retries + 1
				if it.TimeoutMS > 0 {
					budget += it.TimeoutMS * (it.Retries + 1)
				} else {
					budgetOK = false
				}
			}
		}
		if budgetOK && p.SyncHops > 0 {
			p.TimeoutBudgetMS = budget
		}
		paths = append(paths, p)
	}
	walk = func(cur string, trail []hop, onPath map[string]bool) {
		if len(paths) >= maxPaths {
			capped = true
			return
		}
		if len(trail) > maxPathDepth {
			emit(trail, "DEPTH_LIMIT")
			return
		}
		next := false
		terminal := ""
		// An API hands the request to the service that provides it.
		if ix.nodes[cur].Type == "api" {
			for _, pid := range ix.providerOf(cur) {
				next = true
				if onPath[pid] {
					emit(append(slices.Clone(trail), hop{node: pid}), "CYCLE")
					continue
				}
				onPath[pid] = true
				walk(pid, append(slices.Clone(trail), hop{node: pid}), onPath)
				delete(onPath, pid)
			}
		}
		for _, e := range ix.out[cur] {
			it, ok := byEdge[e.ID]
			if !ok {
				continue
			}
			switch {
			case it.Relation == "calls":
				next = true
				t := e.TargetNodeID
				if onPath[t] {
					emit(append(slices.Clone(trail), hop{node: t, edge: e.ID}), "CYCLE")
					continue
				}
				if it.SyncMode != "SYNC" {
					emit(append(slices.Clone(trail), hop{node: t, edge: e.ID}), "ASYNC")
					continue
				}
				onPath[t] = true
				walk(t, append(slices.Clone(trail), hop{node: t, edge: e.ID}), onPath)
				delete(onPath, t)
			case it.Relation == "reads" || it.Relation == "writes":
				terminal = "DATA"
			case it.Relation == "publishes":
				if terminal == "" {
					terminal = "ASYNC"
				}
			}
		}
		if !next {
			if terminal == "" {
				terminal = "LEAF"
			}
			emit(trail, terminal)
		}
	}
	for _, id := range entries {
		walk(id, []hop{{node: id}}, map[string]bool{id: true})
	}
	return refs, paths, capped
}

func buildQuality(ix *index, b *Behaviour) Quality {
	q := Quality{SLOs: []SLO{}}
	covered := map[string][]string{} // node id -> slo ids
	for _, n := range ix.ofType("slo") {
		s := SLO{NodeRef: ix.ref(n.ID), Covers: []NodeRef{}}
		s.AvailabilityPct, _ = num(n.Properties, "availability_pct", "availability", "target_availability")
		if s.AvailabilityPct > 0 && s.AvailabilityPct <= 1 { // 0.999 style
			s.AvailabilityPct *= 100
		}
		s.LatencyP95MS, _ = num(n.Properties, "latency_p95_ms", "p95_ms")
		s.LatencyP99MS, _ = num(n.Properties, "latency_p99_ms", "p99_ms", "latency_ms")
		s.RTOMinutes, _ = num(n.Properties, "rto_minutes", "rto_min", "rto")
		s.RPOMinutes, _ = num(n.Properties, "rpo_minutes", "rpo_min", "rpo")
		if s.RTOMinutes > 0 || s.RPOMinutes > 0 {
			q.HasRecoveryTo = true
		}
		if s.AvailabilityPct > q.TargetAvail {
			q.TargetAvail = s.AvailabilityPct
		}
		// Coverage: anything linked to the SLO, the providers of linked APIs, and what a
		// linked system/domain owns.
		seen := map[string]bool{}
		add := func(id string) {
			if !seen[id] && id != n.ID {
				seen[id] = true
				s.Covers = append(s.Covers, ix.ref(id))
				covered[id] = append(covered[id], n.ID)
			}
		}
		var linked []string
		for _, e := range ix.in[n.ID] {
			linked = append(linked, e.SourceNodeID)
		}
		for _, e := range ix.out[n.ID] {
			linked = append(linked, e.TargetNodeID)
		}
		for _, id := range linked {
			add(id)
			switch ix.nodes[id].Type {
			case "api":
				for _, p := range ix.providerOf(id) {
					add(p)
				}
			case "system", "domain":
				for _, e := range ix.out[id] {
					if e.RelationType == "owns" {
						add(e.TargetNodeID)
					}
				}
			}
		}
		sort.Slice(s.Covers, func(i, j int) bool { return s.Covers[i].Key < s.Covers[j].Key })
		q.SLOs = append(q.SLOs, s)
	}
	onPath := map[string]bool{}
	for i := range b.Paths {
		p := &b.Paths[i]
		set := map[string]bool{}
		for _, n := range p.Nodes {
			for _, sid := range covered[n.ID] {
				set[sid] = true
			}
			if t := ix.nodes[n.ID].Type; t == "service" || t == "component" {
				onPath[n.ID] = true
			}
		}
		for sid := range set {
			p.CoveredBy = append(p.CoveredBy, sid)
		}
		sort.Strings(p.CoveredBy)
		if len(p.CoveredBy) > 0 {
			q.PathsCovered++
		}
	}
	q.PathsTotal = len(b.Paths)
	if q.PathsTotal > 0 {
		q.CoveragePct = math.Round(1000*float64(q.PathsCovered)/float64(q.PathsTotal)) / 10
	}
	q.UnitsOnPaths = len(onPath)
	for id := range onPath {
		if len(covered[id]) > 0 {
			q.UnitsCovered++
		}
	}
	return q
}
