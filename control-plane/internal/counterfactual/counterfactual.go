// Package counterfactual implements Architecture Counterfactual analysis (spec §2.6, §8.4,
// checklist 7.1-7.5): generating >= 2 viable architectural designs (modular monolith,
// domain microservices, event-driven hybrid), evaluating them against eight explicit
// constraints in a decision matrix, recording "when each option is appropriate" as ADRs
// with rejected alternatives, materializing each alternative as a separate graph version,
// and diffing alternatives via the canonical graph version diff.
package counterfactual

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/ids"
)

var (
	ErrNotFound     = errors.New("architecture alternative not found")
	ErrInvalidInput = errors.New("invalid counterfactual request")
	ErrNoModel      = errors.New("diff requires alternatives materialized against an architecture model")
)

// Constraint identifies one of the eight explicit counterfactual evaluation dimensions (7.2).
type Constraint string

const (
	ConstraintScale               Constraint = "scale"
	ConstraintAvailability        Constraint = "availability"
	ConstraintLatency             Constraint = "latency"
	ConstraintCost                Constraint = "cost"
	ConstraintTeamSize            Constraint = "team_size"
	ConstraintCompliance          Constraint = "compliance"
	ConstraintOperationalMaturity Constraint = "operational_maturity"
	ConstraintMigrationComplexity Constraint = "migration_complexity"
)

// AllConstraints lists all eight explicit decision-matrix constraints (7.2).
var AllConstraints = []Constraint{
	ConstraintScale,
	ConstraintAvailability,
	ConstraintLatency,
	ConstraintCost,
	ConstraintTeamSize,
	ConstraintCompliance,
	ConstraintOperationalMaturity,
	ConstraintMigrationComplexity,
}

// Style identifies a canonical architectural alternative pattern (7.1).
type Style string

const (
	StyleModularMonolith     Style = "modular_monolith"
	StyleDomainMicroservices Style = "domain_microservices"
	StyleEventDrivenHybrid   Style = "event_driven_hybrid"
)

// DefaultStyles are the three baseline counterfactual designs evaluated for substantial problems.
var DefaultStyles = []Style{
	StyleModularMonolith,
	StyleDomainMicroservices,
	StyleEventDrivenHybrid,
}

// ConstraintProfile specifies optional caller weights (0..5, default 1.0) and context notes
// across the eight constraints.
type ConstraintProfile struct {
	Weights map[Constraint]float64 `json:"weights,omitempty"`
	Notes   map[Constraint]string  `json:"notes,omitempty"`
}

// NormalizedWeights returns a complete weight map for all 8 constraints (defaulting to 1.0).
func (p ConstraintProfile) NormalizedWeights() map[Constraint]float64 {
	out := make(map[Constraint]float64, len(AllConstraints))
	for _, c := range AllConstraints {
		w := 1.0
		if p.Weights != nil {
			if v, ok := p.Weights[c]; ok && v >= 0 {
				w = v
			}
		}
		out[c] = w
	}
	return out
}

// ConstraintScore holds one cell of the 8-constraint decision matrix.
type ConstraintScore struct {
	Constraint Constraint `json:"constraint"`
	Score      float64    `json:"score"`  // 1.0 (weak) to 5.0 (strong)
	Weight     float64    `json:"weight"` // caller weight (default 1.0)
	Rating     string     `json:"rating"` // HIGH | MEDIUM | LOW
	Rationale  string     `json:"rationale"`
}

// Alternative represents one viable architecture design (7.1-7.3).
type Alternative struct {
	ID              string                         `json:"id"`
	TenantID        string                         `json:"tenant_id,omitempty"`
	ModelID         string                         `json:"model_id,omitempty"`
	WebsiteID       string                         `json:"website_id,omitempty"`
	SetID           string                         `json:"set_id"`
	Style           Style                          `json:"style"`
	Name            string                         `json:"name"`
	Summary         string                         `json:"summary"`
	WhenAppropriate string                         `json:"when_appropriate"`
	Pros            string                         `json:"pros"`
	Cons            string                         `json:"cons"`
	GraphVersion    int                            `json:"graph_version"`
	IsRecommended   bool                           `json:"is_recommended"`
	Scores          map[Constraint]ConstraintScore `json:"scores"`
	WeightedScore   float64                        `json:"weighted_score"`
	DecisionID      string                         `json:"decision_id,omitempty"`
	CreatedAt       time.Time                      `json:"created_at,omitempty"`
	UpdatedAt       time.Time                      `json:"updated_at,omitempty"`
}

// DecisionMatrixRow summarizes one constraint across all alternatives in a set.
type DecisionMatrixRow struct {
	Constraint Constraint                 `json:"constraint"`
	Weight     float64                    `json:"weight"`
	Scores     map[Style]ConstraintScore  `json:"scores"`
	BestStyle  Style                      `json:"best_style"`
}

// DecisionMatrix compares all alternatives in a set across the 8 explicit constraints (7.2).
type DecisionMatrix struct {
	Constraints       []Constraint        `json:"constraints"`
	Rows              []DecisionMatrixRow `json:"rows"`
	RecommendedStyle  Style               `json:"recommended_style"`
	RecommendedReason string              `json:"recommended_reason"`
}

// CounterfactualSet groups >= 2 alternatives, their decision matrix, and linked ADR.
type CounterfactualSet struct {
	SetID          string            `json:"set_id"`
	ModelID        string            `json:"model_id,omitempty"`
	WebsiteID      string            `json:"website_id,omitempty"`
	Subject        string            `json:"subject"`
	Constraints    ConstraintProfile `json:"constraints"`
	Alternatives   []Alternative     `json:"alternatives"`
	DecisionMatrix DecisionMatrix    `json:"decision_matrix"`
	DecisionID     string            `json:"decision_id,omitempty"`
}

// GenerateInput configures counterfactual generation for a model or website.
type GenerateInput struct {
	Subject     string            `json:"subject,omitempty"`
	Styles      []Style           `json:"styles,omitempty"` // defaults to all 3 styles (must be >= 2)
	Constraints ConstraintProfile `json:"constraints,omitempty"`
	RecordADR   *bool             `json:"record_adr,omitempty"` // default true when linked to a model
}

// AlternativeDiff compares two materialized alternatives using the canonical graph version diff (7.4).
type AlternativeDiff struct {
	ModelID         string                      `json:"model_id"`
	FromAlternative Alternative                 `json:"from_alternative"`
	ToAlternative   Alternative                 `json:"to_alternative"`
	ScoreDeltas     map[Constraint]float64      `json:"score_deltas"`
	WeightedDelta   float64                     `json:"weighted_delta"`
	GraphDiff       graph.Diff                  `json:"graph_diff"`
}

type styleTemplate struct {
	Style           Style
	Name            string
	Summary         string
	WhenAppropriate string
	Pros            string
	Cons            string
	BaseScores      map[Constraint]struct {
		Score     float64
		Rationale string
	}
}

var styleTemplates = map[Style]styleTemplate{
	StyleModularMonolith: {
		Style:           StyleModularMonolith,
		Name:            "Modular Monolith",
		Summary:         "Single deployable runtime with strict internal domain modules and a shared ACID relational database.",
		WhenAppropriate: "Best when team size is small-to-medium (<= 25 engineers), operational maturity is growing, low internal call latency and low infrastructure cost matter most, and domain boundaries are still evolving.",
		Pros:            "Lowest operational overhead, sub-millisecond in-process calls, single ACID transaction boundary, fastest initial delivery and simplest migration.",
		Cons:            "Independent per-module scaling is limited, shared deployment blast radius, requires strict module-boundary discipline to avoid coupling.",
		BaseScores: map[Constraint]struct {
			Score     float64
			Rationale string
		}{
			ConstraintScale:               {3.0, "Vertical + horizontal stateless scaling works to high traffic, but hot modules cannot scale independently."},
			ConstraintAvailability:        {3.5, "Multi-AZ stateless replicas + managed DB failover; single deployable shares release blast radius."},
			ConstraintLatency:             {5.0, "In-process module calls avoid network hops and serialization overhead."},
			ConstraintCost:                {5.0, "Minimal compute, networking, and observability footprint."},
			ConstraintTeamSize:            {4.5, "Ideal for small-to-medium teams; coordination friction grows beyond ~30 engineers."},
			ConstraintCompliance:          {4.0, "Unified audit trail and single security perimeter simplify compliance reviews."},
			ConstraintOperationalMaturity: {5.0, "Single CI/CD pipeline, straightforward debugging, and simple rollback."},
			ConstraintMigrationComplexity: {5.0, "Lowest migration risk; clean stepping stone toward selective extraction later."},
		},
	},
	StyleDomainMicroservices: {
		Style:           StyleDomainMicroservices,
		Name:            "Domain Microservices",
		Summary:         "Independently deployable services aligned to bounded contexts, each owning its private datastore and versioned API.",
		WhenAppropriate: "Best when multiple autonomous squads (> 25 engineers) need independent release cadences, strict compliance/data isolation per domain, and independent horizontal scaling of hot workloads.",
		Pros:            "Independent scaling and deployment per domain, strong fault and compliance isolation, clear team ownership.",
		Cons:            "Distributed latency and partial failure modes, higher cloud and observability cost, requires mature platform/SRE tooling.",
		BaseScores: map[Constraint]struct {
			Score     float64
			Rationale string
		}{
			ConstraintScale:               {5.0, "Each domain service and datastore scales independently to workload hotspots."},
			ConstraintAvailability:        {4.5, "Fault isolation per bounded context prevents single-domain failures from taking down the platform."},
			ConstraintLatency:             {3.0, "Cross-service synchronous RPC hops add network and serialization tail latency."},
			ConstraintCost:                {2.5, "Higher compute, service-mesh, observability, and per-service database overhead."},
			ConstraintTeamSize:            {4.5, "Scales organizational autonomy across many squads with clear API contracts."},
			ConstraintCompliance:          {5.0, "Strong data-residency and PCI/HIPAA blast-radius isolation per service."},
			ConstraintOperationalMaturity: {2.5, "Requires mature CI/CD, distributed tracing, contract testing, and SRE practices."},
			ConstraintMigrationComplexity: {2.5, "Decomposing shared schemas and distributed transactions is complex and incremental."},
		},
	},
	StyleEventDrivenHybrid: {
		Style:           StyleEventDrivenHybrid,
		Name:            "Event-Driven Hybrid",
		Summary:         "Core synchronous API/domain kernel paired with transactional outbox, message broker (NATS/Kafka), and asynchronous event consumers.",
		WhenAppropriate: "Best when traffic is spiky or write-heavy, high resilience and decoupled background workflows are critical, and teams want incremental decoupling without full microservice sprawl.",
		Pros:            "Smooths traffic spikes with backpressure, decouples side effects and analytics, balances strong core consistency with async scalability.",
		Cons:            "Eventual consistency across projections, requires idempotent consumers, dead-letter handling, and schema evolution governance.",
		BaseScores: map[Constraint]struct {
			Score     float64
			Rationale string
		}{
			ConstraintScale:               {4.5, "Async consumers and partitioned topics absorb traffic bursts and scale elastically."},
			ConstraintAvailability:        {5.0, "Broker buffering isolates downstream outages and enables replay-based recovery."},
			ConstraintLatency:             {4.0, "Fast synchronous write path via outbox; non-critical work offloaded off the request path."},
			ConstraintCost:                {3.5, "Moderate cost: core runtime plus managed broker and worker pools."},
			ConstraintTeamSize:            {4.0, "Works well for mid-to-large teams; async workers can be owned independently."},
			ConstraintCompliance:          {4.0, "Immutable event log aids auditing; requires PII encryption/retention discipline on topics."},
			ConstraintOperationalMaturity: {3.5, "Requires consumer lag monitoring, DLQ playbooks, and idempotency discipline."},
			ConstraintMigrationComplexity: {4.0, "Strangler-friendly via transactional outbox without big-bang database splits."},
		},
	},
}

func ratingFromScore(score float64) string {
	switch {
	case score >= 4.0:
		return "HIGH"
	case score >= 3.0:
		return "MEDIUM"
	default:
		return "LOW"
	}
}

// GenerateCounterfactualSet builds >= 2 viable alternatives and evaluates them across all
// 8 explicit constraints in a weighted DecisionMatrix (7.1-7.3).
func GenerateCounterfactualSet(subject string, styles []Style, profile ConstraintProfile) (CounterfactualSet, error) {
	if len(styles) == 0 {
		styles = DefaultStyles
	}
	// Deduplicate styles and enforce >= 2 viable options (7.1).
	seen := map[Style]bool{}
	unique := make([]Style, 0, len(styles))
	for _, st := range styles {
		if _, ok := styleTemplates[st]; !ok {
			return CounterfactualSet{}, fmt.Errorf("%w: unsupported style %q (use modular_monolith, domain_microservices, event_driven_hybrid)", ErrInvalidInput, st)
		}
		if !seen[st] {
			seen[st] = true
			unique = append(unique, st)
		}
	}
	if len(unique) < 2 {
		return CounterfactualSet{}, fmt.Errorf("%w: at least 2 viable alternatives are required", ErrInvalidInput)
	}

	weights := profile.NormalizedWeights()
	setID := ids.New()
	if strings.TrimSpace(subject) == "" {
		subject = "Architecture Model"
	}

	alts := make([]Alternative, 0, len(unique))
	for _, st := range unique {
		tpl := styleTemplates[st]
		scores := make(map[Constraint]ConstraintScore, len(AllConstraints))
		var weightedSum, totalWeight float64
		for _, c := range AllConstraints {
			base := tpl.BaseScores[c]
			w := weights[c]
			scores[c] = ConstraintScore{
				Constraint: c,
				Score:      base.Score,
				Weight:     w,
				Rating:     ratingFromScore(base.Score),
				Rationale:  base.Rationale,
			}
			weightedSum += base.Score * w
			totalWeight += w
		}
		weightedAvg := 0.0
		if totalWeight > 0 {
			weightedAvg = math.Round((weightedSum/totalWeight)*1000) / 1000
		}
		alts = append(alts, Alternative{
			ID:              ids.New(),
			SetID:           setID,
			Style:           tpl.Style,
			Name:            tpl.Name,
			Summary:         tpl.Summary,
			WhenAppropriate: tpl.WhenAppropriate,
			Pros:            tpl.Pros,
			Cons:            tpl.Cons,
			Scores:          scores,
			WeightedScore:   weightedAvg,
		})
	}

	// Sort descending by WeightedScore (deterministic tie-break by Style).
	sort.Slice(alts, func(i, j int) bool {
		if alts[i].WeightedScore != alts[j].WeightedScore {
			return alts[i].WeightedScore > alts[j].WeightedScore
		}
		return alts[i].Style < alts[j].Style
	})
	alts[0].IsRecommended = true

	matrix := BuildDecisionMatrix(alts, weights)
	return CounterfactualSet{
		SetID:          setID,
		Subject:        subject,
		Constraints:    profile,
		Alternatives:   alts,
		DecisionMatrix: matrix,
	}, nil
}

// BuildDecisionMatrix constructs the 8-constraint comparison table across alternatives (7.2).
func BuildDecisionMatrix(alts []Alternative, weights map[Constraint]float64) DecisionMatrix {
	rows := make([]DecisionMatrixRow, 0, len(AllConstraints))
	for _, c := range AllConstraints {
		w := 1.0
		if weights != nil {
			if v, ok := weights[c]; ok {
				w = v
			}
		}
		rowScores := make(map[Style]ConstraintScore, len(alts))
		var bestStyle Style
		bestScore := -1.0
		for _, alt := range alts {
			sc := alt.Scores[c]
			rowScores[alt.Style] = sc
			if sc.Score > bestScore {
				bestScore = sc.Score
				bestStyle = alt.Style
			}
		}
		rows = append(rows, DecisionMatrixRow{
			Constraint: c,
			Weight:     w,
			Scores:     rowScores,
			BestStyle:  bestStyle,
		})
	}

	var recStyle Style
	var recReason string
	for _, a := range alts {
		if a.IsRecommended {
			recStyle = a.Style
			recReason = fmt.Sprintf("%s scored highest (%.2f/5.00 weighted). %s", a.Name, a.WeightedScore, a.WhenAppropriate)
			break
		}
	}
	if recStyle == "" && len(alts) > 0 {
		recStyle = alts[0].Style
		recReason = alts[0].WhenAppropriate
	}

	return DecisionMatrix{
		Constraints:       AllConstraints,
		Rows:              rows,
		RecommendedStyle:  recStyle,
		RecommendedReason: recReason,
	}
}

var nonKeyChars = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	v := strings.Trim(nonKeyChars.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if v == "" {
		return "app"
	}
	if len(v) > 24 {
		v = v[:24]
	}
	return v
}

// BuildAlternativePatch builds a valid, relation-checked canonical GraphPatch representing
// the topology of one architectural alternative (so each alternative can be materialized as
// its own graph version and diffed with graph.Service.Diff — 7.1, 7.4).
func BuildAlternativePatch(baseVersion int, subject string, alt Alternative, idempotencyKey string) graph.Patch {
	prefix := fmt.Sprintf("cf:%s:%s", slug(subject), slug(string(alt.Style)))
	shortID := alt.SetID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}

	type nodeSpec struct {
		id, key, typ, name, desc, layer string
	}
	type edgeSpec struct {
		src, dst, rel, proto, sync string
	}

	var nodes []nodeSpec
	var edges []edgeSpec

	switch alt.Style {
	case StyleModularMonolith:
		sysID, svcID, apiID, dbID, depID := ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
		nodes = []nodeSpec{
			{sysID, fmt.Sprintf("%s.%s.sys", prefix, shortID), "system", subject + " (Modular Monolith)", alt.Summary, "product_capability"},
			{svcID, fmt.Sprintf("%s.%s.app", prefix, shortID), "service", "Modular Monolith Core", "Single deployable runtime with bounded internal modules", "service"},
			{apiID, fmt.Sprintf("%s.%s.api", prefix, shortID), "api", "Unified REST/GraphQL API", "Public API surface served directly by the modular monolith", "api"},
			{dbID, fmt.Sprintf("%s.%s.db", prefix, shortID), "database", "Shared Primary PostgreSQL", "ACID relational store partitioned by module schemas", "data"},
			{depID, fmt.Sprintf("%s.%s.deploy", prefix, shortID), "deployment_unit", "Stateless App Fleet", "Multi-AZ auto-scaled monolith deployment unit", "infrastructure"},
		}
		edges = []edgeSpec{
			{sysID, svcID, "owns", "", ""},
			{svcID, apiID, "owns", "HTTPS", "SYNC"},
			{svcID, dbID, "writes", "PostgreSQL", "SYNC"},
			{svcID, depID, "deploys_to", "", ""},
		}

	case StyleDomainMicroservices:
		sysID, domAID, domBID := ids.New(), ids.New(), ids.New()
		svcAID, svcBID, apiID := ids.New(), ids.New(), ids.New()
		dbAID, dbBID, depID := ids.New(), ids.New(), ids.New()
		nodes = []nodeSpec{
			{sysID, fmt.Sprintf("%s.%s.sys", prefix, shortID), "system", subject + " (Domain Microservices)", alt.Summary, "product_capability"},
			{domAID, fmt.Sprintf("%s.%s.dom-core", prefix, shortID), "domain", "Core Domain Context", "Primary transactional bounded context", "domain"},
			{domBID, fmt.Sprintf("%s.%s.dom-sup", prefix, shortID), "domain", "Supporting Domain Context", "Independently scalable supporting context", "domain"},
			{svcAID, fmt.Sprintf("%s.%s.svc-core", prefix, shortID), "service", "Core Domain Service", "Owns core workflows and private datastore", "service"},
			{svcBID, fmt.Sprintf("%s.%s.svc-sup", prefix, shortID), "service", "Supporting Domain Service", "Owns auxiliary capabilities via gRPC/REST", "service"},
			{apiID, fmt.Sprintf("%s.%s.gw", prefix, shortID), "api", "API Gateway", "Edge routing, auth, and rate limiting across services", "api"},
			{dbAID, fmt.Sprintf("%s.%s.db-core", prefix, shortID), "database", "Core Service DB", "Private database owned exclusively by Core Service", "data"},
			{dbBID, fmt.Sprintf("%s.%s.db-sup", prefix, shortID), "database", "Supporting Service DB", "Private database owned exclusively by Supporting Service", "data"},
			{depID, fmt.Sprintf("%s.%s.k8s", prefix, shortID), "deployment_unit", "Kubernetes Service Mesh", "Independent deployments with mTLS service mesh", "infrastructure"},
		}
		edges = []edgeSpec{
			{sysID, domAID, "owns", "", ""},
			{sysID, domBID, "owns", "", ""},
			{domAID, svcAID, "owns", "", ""},
			{domBID, svcBID, "owns", "", ""},
			{svcAID, apiID, "owns", "HTTPS", "SYNC"},
			{svcAID, svcBID, "calls", "gRPC", "SYNC"},
			{svcAID, dbAID, "writes", "PostgreSQL", "SYNC"},
			{svcBID, dbBID, "writes", "PostgreSQL", "SYNC"},
			{svcAID, depID, "deploys_to", "", ""},
			{svcBID, depID, "deploys_to", "", ""},
		}

	default: // StyleEventDrivenHybrid
		sysID, svcCoreID, svcWorkerID := ids.New(), ids.New(), ids.New()
		apiID, dbID, topicID, depID := ids.New(), ids.New(), ids.New(), ids.New()
		nodes = []nodeSpec{
			{sysID, fmt.Sprintf("%s.%s.sys", prefix, shortID), "system", subject + " (Event-Driven Hybrid)", alt.Summary, "product_capability"},
			{svcCoreID, fmt.Sprintf("%s.%s.core", prefix, shortID), "service", "Core API & Outbox Service", "Handles synchronous commands and commits transactional outbox", "service"},
			{svcWorkerID, fmt.Sprintf("%s.%s.worker", prefix, shortID), "service", "Async Event Consumer Pool", "Processes background workflows, projections, and integrations", "service"},
			{apiID, fmt.Sprintf("%s.%s.api", prefix, shortID), "api", "Command & Query API", "Low-latency synchronous entry point", "api"},
			{dbID, fmt.Sprintf("%s.%s.db", prefix, shortID), "database", "Core PostgreSQL + Outbox", "ACID store with transactional outbox table", "data"},
			{topicID, fmt.Sprintf("%s.%s.topic", prefix, shortID), "topic", "Domain Event Stream (NATS/Kafka)", "Durable partitioned event bus decoupling async consumers", "events"},
			{depID, fmt.Sprintf("%s.%s.deploy", prefix, shortID), "deployment_unit", "Hybrid Core + Worker Cluster", "Autoscaled API pods and queue-driven worker pools", "infrastructure"},
		}
		edges = []edgeSpec{
			{sysID, svcCoreID, "owns", "", ""},
			{svcCoreID, apiID, "owns", "HTTPS", "SYNC"},
			{svcCoreID, dbID, "writes", "PostgreSQL", "SYNC"},
			{svcCoreID, topicID, "publishes", "NATS", "ASYNC"},
			{svcWorkerID, topicID, "subscribes", "NATS", "ASYNC"},
			{svcCoreID, depID, "deploys_to", "", ""},
			{svcWorkerID, depID, "deploys_to", "", ""},
		}
	}

	ops := make([]graph.Operation, 0, len(nodes)+len(edges))
	for _, n := range nodes {
		payload, _ := json.Marshal(graph.AddNodePayload{
			Key:           n.key,
			Type:          n.typ,
			Name:          n.name,
			Description:   n.desc,
			EvidenceState: "PROPOSED",
			Properties: map[string]any{
				"layer":               n.layer,
				"counterfactual_set":  alt.SetID,
				"alternative_style":   string(alt.Style),
				"alternative_id":      alt.ID,
			},
		})
		ops = append(ops, graph.Operation{
			Op:        graph.OpAddNode,
			TargetID:  n.id,
			Payload:   payload,
			Rationale: fmt.Sprintf("Counterfactual alternative %s (%s)", alt.Name, alt.Style),
		})
	}
	for _, e := range edges {
		payload, _ := json.Marshal(graph.AddEdgePayload{
			SourceNodeID: e.src,
			TargetNodeID: e.dst,
			RelationType: e.rel,
			Protocol:     e.proto,
			SyncMode:     e.sync,
			Properties: map[string]any{
				"counterfactual_set": alt.SetID,
				"alternative_style":  string(alt.Style),
			},
		})
		ops = append(ops, graph.Operation{
			Op:        graph.OpAddEdge,
			TargetID:  ids.New(),
			Payload:   payload,
			Rationale: fmt.Sprintf("Counterfactual relation for %s", alt.Name),
		})
	}

	return graph.Patch{
		BaseGraphVersion: &baseVersion,
		IdempotencyKey:   idempotencyKey,
		Operations:       ops,
	}
}
