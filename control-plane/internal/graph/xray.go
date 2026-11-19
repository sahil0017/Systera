package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/postgres"
)

// XRayLayer identifies one of the eight Architecture X-Ray layers (spec §2.5, checklist 6.1).
type XRayLayer string

const (
	LayerProductCapability  XRayLayer = "product_capability"
	LayerDomain             XRayLayer = "domain"
	LayerService            XRayLayer = "service"
	LayerAPI                XRayLayer = "api"
	LayerData               XRayLayer = "data"
	LayerEvents             XRayLayer = "events"
	LayerInfrastructure     XRayLayer = "infrastructure"
	LayerSecurityReliability XRayLayer = "security_reliability"
)

// AllXRayLayers lists the eight Architecture X-Ray layers in top-down stack order.
var AllXRayLayers = []XRayLayer{
	LayerProductCapability,
	LayerDomain,
	LayerService,
	LayerAPI,
	LayerData,
	LayerEvents,
	LayerInfrastructure,
	LayerSecurityReliability,
}

// XRayLayerMeta describes one X-Ray layer and its default node-type mapping (6.1).
type XRayLayerMeta struct {
	ID               XRayLayer `json:"id"`
	Label            string    `json:"label"`
	Order            int       `json:"order"`
	Description      string    `json:"description"`
	DefaultNodeTypes []string  `json:"default_node_types"`
}

// XRayLayerDefinitions is the authoritative catalog of all eight X-Ray layers.
var XRayLayerDefinitions = []XRayLayerMeta{
	{
		ID:               LayerProductCapability,
		Label:            "Product / Capability",
		Order:            1,
		Description:      "User-facing product capabilities, actors, systems, and workloads.",
		DefaultNodeTypes: []string{"system", "actor", "workload"},
	},
	{
		ID:               LayerDomain,
		Label:            "Domain",
		Order:            2,
		Description:      "Bounded contexts, business domains, and ownership boundaries.",
		DefaultNodeTypes: []string{"domain"},
	},
	{
		ID:               LayerService,
		Label:            "Service",
		Order:            3,
		Description:      "Runtime services, internal components, and external dependencies.",
		DefaultNodeTypes: []string{"service", "component", "external_dependency"},
	},
	{
		ID:               LayerAPI,
		Label:            "API",
		Order:            4,
		Description:      "Synchronous and asynchronous API endpoints and contracts.",
		DefaultNodeTypes: []string{"api"},
	},
	{
		ID:               LayerData,
		Label:            "Data",
		Order:            5,
		Description:      "Databases, tables, schemas, and caches.",
		DefaultNodeTypes: []string{"database", "table", "cache"},
	},
	{
		ID:               LayerEvents,
		Label:            "Events",
		Order:            6,
		Description:      "Domain events, message topics, and work queues.",
		DefaultNodeTypes: []string{"event", "topic", "queue"},
	},
	{
		ID:               LayerInfrastructure,
		Label:            "Infrastructure",
		Order:            7,
		Description:      "Deployment units, compute clusters, and cloud regions.",
		DefaultNodeTypes: []string{"deployment_unit", "region"},
	},
	{
		ID:               LayerSecurityReliability,
		Label:            "Security / Reliability",
		Order:            8,
		Description:      "Secret boundaries, SLOs, threats, and verification tests.",
		DefaultNodeTypes: []string{"secret_boundary", "slo", "threat", "test"},
	},
}

var xrayLayerByNodeType = map[string]XRayLayer{
	"system":              LayerProductCapability,
	"actor":               LayerProductCapability,
	"workload":            LayerProductCapability,
	"domain":              LayerDomain,
	"service":             LayerService,
	"component":           LayerService,
	"external_dependency": LayerService,
	"api":                 LayerAPI,
	"database":            LayerData,
	"table":               LayerData,
	"cache":               LayerData,
	"event":               LayerEvents,
	"topic":               LayerEvents,
	"queue":               LayerEvents,
	"deployment_unit":     LayerInfrastructure,
	"region":              LayerInfrastructure,
	"secret_boundary":     LayerSecurityReliability,
	"slo":                 LayerSecurityReliability,
	"threat":              LayerSecurityReliability,
	"test":                LayerSecurityReliability,
}

var xrayLayerAliases = map[string]XRayLayer{
	"product_capability":   LayerProductCapability,
	"product/capability":   LayerProductCapability,
	"product":              LayerProductCapability,
	"capability":           LayerProductCapability,
	"user_journey":         LayerProductCapability,
	"domain":               LayerDomain,
	"domain_boundary":      LayerDomain,
	"service":              LayerService,
	"service_dependency":   LayerService,
	"api":                  LayerAPI,
	"api_interaction":      LayerAPI,
	"data":                 LayerData,
	"data_entity":          LayerData,
	"events":               LayerEvents,
	"event":                LayerEvents,
	"event_flow":           LayerEvents,
	"infrastructure":       LayerInfrastructure,
	"infra":                LayerInfrastructure,
	"security_reliability": LayerSecurityReliability,
	"security/reliability": LayerSecurityReliability,
	"security":             LayerSecurityReliability,
	"reliability":          LayerSecurityReliability,
}

// ParseXRayLayer normalizes a layer name or alias into one of the eight canonical X-Ray layers.
func ParseXRayLayer(raw string) (XRayLayer, bool) {
	key := strings.ToLower(strings.TrimSpace(raw))
	l, ok := xrayLayerAliases[key]
	return l, ok
}

// DefaultLayerForNodeType returns the canonical X-Ray layer for a graph node type (6.1).
func DefaultLayerForNodeType(nodeType string) XRayLayer {
	if l, ok := xrayLayerByNodeType[nodeType]; ok {
		return l
	}
	return LayerService
}

// ResolveXRayLayer resolves a node's X-Ray layer: explicit properties["layer"] takes
// precedence, then properties["fingerprint_layer"], then the canonical node-type mapping (6.1).
func ResolveXRayLayer(n Node) XRayLayer {
	if n.Properties != nil {
		if raw, ok := n.Properties["layer"].(string); ok && raw != "" {
			if l, valid := ParseXRayLayer(raw); valid {
				return l
			}
		}
		if raw, ok := n.Properties["fingerprint_layer"].(string); ok && raw != "" {
			if l, valid := ParseXRayLayer(raw); valid {
				return l
			}
		}
	}
	return DefaultLayerForNodeType(n.Type)
}

// XRayNode is a graph node enriched with its resolved X-Ray layer and optional hop depth.
type XRayNode struct {
	Node
	Layer XRayLayer `json:"layer"`
	Depth int       `json:"depth,omitempty"`
}

// XRayEdge is a graph edge enriched with source and target X-Ray layers.
type XRayEdge struct {
	Edge
	SourceLayer XRayLayer `json:"source_layer"`
	TargetLayer XRayLayer `json:"target_layer"`
	CrossLayer  bool      `json:"cross_layer"`
}

// XRayLayerGroup holds the nodes and intra-layer edges for one X-Ray layer.
type XRayLayerGroup struct {
	XRayLayerMeta
	NodeCount int        `json:"node_count"`
	EdgeCount int        `json:"edge_count"`
	Nodes     []XRayNode `json:"nodes"`
	Edges     []XRayEdge `json:"edges"`
}

// XRayView is the layered representation of an architecture model at one version (6.1, 6.3).
type XRayView struct {
	ModelID         string           `json:"model_id"`
	Version         int              `json:"version"`
	GraphHash       string           `json:"graph_hash"`
	TotalNodes      int              `json:"total_nodes"`
	TotalEdges      int              `json:"total_edges"`
	Layers          []XRayLayerGroup `json:"layers"`
	CrossLayerEdges []XRayEdge       `json:"cross_layer_edges"`
}

// XRayDrillDown is a neighbourhood-based layer drill-down around a center component (6.3).
type XRayDrillDown struct {
	ModelID         string           `json:"model_id"`
	Version         int              `json:"version"`
	CenterNodeID    string           `json:"center_node_id"`
	CenterLayer     XRayLayer        `json:"center_layer"`
	Depth           int              `json:"depth"`
	Direction       Direction        `json:"direction"`
	Truncated       bool             `json:"truncated"`
	Layers          []XRayLayerGroup `json:"layers"`
	CrossLayerEdges []XRayEdge       `json:"cross_layer_edges"`
}

// XRayDrillDownQuery configures a neighbourhood drill-down across X-Ray layers (6.3).
type XRayDrillDownQuery struct {
	Direction     Direction
	RelationTypes []string
	TargetLayers  []XRayLayer
	Depth         int
	MaxNodes      int
}

// BuildXRayView projects any Graph into the eight X-Ray layers (6.1).
// If filterLayers is non-empty, only those layers are returned (cross-layer edges are
// filtered to edges whose endpoints are both in the included layers).
func BuildXRayView(g Graph, filterLayers []XRayLayer) XRayView {
	include := map[XRayLayer]bool{}
	if len(filterLayers) == 0 {
		for _, l := range AllXRayLayers {
			include[l] = true
		}
	} else {
		for _, l := range filterLayers {
			include[l] = true
		}
	}

	nodeLayer := make(map[string]XRayLayer, len(g.Nodes))
	layerNodes := make(map[XRayLayer][]XRayNode, len(AllXRayLayers))
	for _, n := range g.Nodes {
		l := ResolveXRayLayer(n)
		nodeLayer[n.ID] = l
		if include[l] {
			layerNodes[l] = append(layerNodes[l], XRayNode{Node: n, Layer: l})
		}
	}

	layerEdges := make(map[XRayLayer][]XRayEdge, len(AllXRayLayers))
	crossEdges := []XRayEdge{}
	for _, e := range g.Edges {
		sl, okS := nodeLayer[e.SourceNodeID]
		tl, okT := nodeLayer[e.TargetNodeID]
		if !okS || !okT || !include[sl] || !include[tl] {
			continue
		}
		xe := XRayEdge{Edge: e, SourceLayer: sl, TargetLayer: tl, CrossLayer: sl != tl}
		if sl == tl {
			layerEdges[sl] = append(layerEdges[sl], xe)
		} else {
			crossEdges = append(crossEdges, xe)
		}
	}

	groups := make([]XRayLayerGroup, 0, len(AllXRayLayers))
	totalNodes, totalEdges := 0, len(crossEdges)
	for _, meta := range XRayLayerDefinitions {
		if !include[meta.ID] {
			continue
		}
		ns := layerNodes[meta.ID]
		if ns == nil {
			ns = []XRayNode{}
		}
		sort.Slice(ns, func(i, j int) bool { return ns[i].Key < ns[j].Key })
		es := layerEdges[meta.ID]
		if es == nil {
			es = []XRayEdge{}
		}
		totalNodes += len(ns)
		totalEdges += len(es)
		groups = append(groups, XRayLayerGroup{
			XRayLayerMeta: meta,
			NodeCount:     len(ns),
			EdgeCount:     len(es),
			Nodes:         ns,
			Edges:         es,
		})
	}

	return XRayView{
		ModelID:         g.Version.ModelID,
		Version:         g.Version.Number,
		GraphHash:       g.Version.GraphHash,
		TotalNodes:      totalNodes,
		TotalEdges:      totalEdges,
		Layers:          groups,
		CrossLayerEdges: crossEdges,
	}
}

// XRay returns the 8-layer Architecture X-Ray view of a model at a version (6.1, 6.3).
func (s *Store) XRay(ctx context.Context, tenantID, modelID string, number int, filterLayers []XRayLayer) (XRayView, error) {
	g, err := s.GraphAt(ctx, tenantID, modelID, number)
	if err != nil {
		return XRayView{}, err
	}
	return BuildXRayView(g, filterLayers), nil
}

// XRayDrillDown performs a neighbourhood walk around centerNodeID and projects the result
// onto the eight X-Ray layers for cross-layer navigation and drill-down (6.3).
func (s *Store) XRayDrillDown(ctx context.Context, tenantID, modelID string, number int, centerNodeID string, q XRayDrillDownQuery) (XRayDrillDown, error) {
	nb, err := s.Neighbourhood(ctx, tenantID, modelID, number, centerNodeID, NeighbourhoodQuery{
		Direction:     q.Direction,
		RelationTypes: q.RelationTypes,
		Depth:         q.Depth,
		MaxNodes:      q.MaxNodes,
	})
	if err != nil {
		return XRayDrillDown{}, err
	}

	include := map[XRayLayer]bool{}
	if len(q.TargetLayers) == 0 {
		for _, l := range AllXRayLayers {
			include[l] = true
		}
	} else {
		for _, l := range q.TargetLayers {
			include[l] = true
		}
	}

	nodeLayer := make(map[string]XRayLayer, len(nb.Nodes))
	kept := make(map[string]bool, len(nb.Nodes))
	layerNodes := make(map[XRayLayer][]XRayNode, len(AllXRayLayers))
	var centerLayer XRayLayer
	for _, nn := range nb.Nodes {
		l := ResolveXRayLayer(nn.Node)
		nodeLayer[nn.ID] = l
		if nn.ID == nb.CenterNodeID {
			centerLayer = l
		}
		if include[l] || nn.ID == nb.CenterNodeID {
			kept[nn.ID] = true
			layerNodes[l] = append(layerNodes[l], XRayNode{Node: nn.Node, Layer: l, Depth: nn.Depth})
		}
	}

	layerEdges := make(map[XRayLayer][]XRayEdge, len(AllXRayLayers))
	crossEdges := []XRayEdge{}
	for _, e := range nb.Edges {
		if !kept[e.SourceNodeID] || !kept[e.TargetNodeID] {
			continue
		}
		sl, tl := nodeLayer[e.SourceNodeID], nodeLayer[e.TargetNodeID]
		xe := XRayEdge{Edge: e, SourceLayer: sl, TargetLayer: tl, CrossLayer: sl != tl}
		if sl == tl {
			layerEdges[sl] = append(layerEdges[sl], xe)
		} else {
			crossEdges = append(crossEdges, xe)
		}
	}

	groups := make([]XRayLayerGroup, 0, len(AllXRayLayers))
	for _, meta := range XRayLayerDefinitions {
		ns := layerNodes[meta.ID]
		if len(ns) == 0 && !include[meta.ID] {
			continue
		}
		if ns == nil {
			ns = []XRayNode{}
		}
		sort.Slice(ns, func(i, j int) bool {
			if ns[i].Depth != ns[j].Depth {
				return ns[i].Depth < ns[j].Depth
			}
			return ns[i].Key < ns[j].Key
		})
		es := layerEdges[meta.ID]
		if es == nil {
			es = []XRayEdge{}
		}
		groups = append(groups, XRayLayerGroup{
			XRayLayerMeta: meta,
			NodeCount:     len(ns),
			EdgeCount:     len(es),
			Nodes:         ns,
			Edges:         es,
		})
	}

	dir := q.Direction
	if dir == "" {
		dir = Both
	}
	return XRayDrillDown{
		ModelID:         modelID,
		Version:         nb.Version,
		CenterNodeID:    nb.CenterNodeID,
		CenterLayer:     centerLayer,
		Depth:           nb.Depth,
		Direction:       dir,
		Truncated:       nb.Truncated,
		Layers:          groups,
		CrossLayerEdges: crossEdges,
	}, nil
}

// ---------------------------------------------------------------------------
// 6.2 Component Inspection (one query)
// ---------------------------------------------------------------------------

// NodeRef is a compact reference to a related component in an inspection response.
type NodeRef struct {
	ID            string    `json:"id"`
	Key           string    `json:"key"`
	Type          string    `json:"type"`
	Name          string    `json:"name"`
	Layer         XRayLayer `json:"layer"`
	EvidenceState string    `json:"evidence_state"`
}

// EdgeNodeRef pairs a related node with the connecting edge metadata.
type EdgeNodeRef struct {
	Node         NodeRef `json:"node"`
	EdgeID       string  `json:"edge_id"`
	RelationType string  `json:"relation_type"`
	Protocol     string  `json:"protocol,omitempty"`
	SyncMode     string  `json:"sync_mode,omitempty"`
}

// InterfaceSpec describes an API, event, topic, or queue exposed or consumed by a component.
type InterfaceSpec struct {
	Role         string  `json:"role"` // EXPOSES | CONSUMES | PUBLISHES | SUBSCRIBES
	RelationType string  `json:"relation_type"`
	Protocol     string  `json:"protocol,omitempty"`
	SyncMode     string  `json:"sync_mode,omitempty"`
	Target       NodeRef `json:"target"`
}

// DataOwnership summarizes the data stores a component owns, reads, or writes.
type DataOwnership struct {
	Owns   []NodeRef `json:"owns"`
	Reads  []NodeRef `json:"reads"`
	Writes []NodeRef `json:"writes"`
}

// InspectedClaim summarizes a claim and its supporting evidence linked to a component.
type InspectedClaim struct {
	ID             string              `json:"id"`
	Statement      string              `json:"statement"`
	ClaimClass     string              `json:"claim_class"`
	ConfidenceBand string              `json:"confidence_band"`
	Status         string              `json:"status"`
	ReviewState    string              `json:"review_state"`
	Evidence       []InspectedEvidence `json:"evidence"`
}

// InspectedEvidence is a compact evidence record attached to a claim.
type InspectedEvidence struct {
	ID         string    `json:"id"`
	SourceRef  string    `json:"source_ref"`
	SourceType string    `json:"source_type"`
	Directness string    `json:"directness"`
	TrustLevel string    `json:"trust_level"`
	Excerpt    string    `json:"excerpt,omitempty"`
	CapturedAt time.Time `json:"captured_at"`
}

// InspectedAssumption summarizes an assumption referencing a component.
type InspectedAssumption struct {
	ID               string `json:"id"`
	Statement        string `json:"statement"`
	Rationale        string `json:"rationale"`
	Owner            string `json:"owner"`
	RiskIfWrong      string `json:"risk_if_wrong"`
	ValidationMethod string `json:"validation_method,omitempty"`
	Status           string `json:"status"`
}

// FailureMode describes an explicit, graph-linked, or structurally inferred failure mode.
type FailureMode struct {
	Title       string `json:"title"`
	Severity    string `json:"severity"` // HIGH | MEDIUM | LOW
	Source      string `json:"source"`   // GRAPH_THREAT | GRAPH_SLO | PROPERTY | STRUCTURAL
	Description string `json:"description"`
	Mitigation  string `json:"mitigation,omitempty"`
	RelatedID   string `json:"related_node_id,omitempty"`
}

// InspectedAlternative captures a considered design alternative from ADRs or website analysis.
type InspectedAlternative struct {
	Source          string `json:"source"` // ADR | WEBSITE_ANALYSIS
	DecisionID      string `json:"decision_id,omitempty"`
	ADRNumber       int    `json:"adr_number,omitempty"`
	Title           string `json:"title"`
	ChosenOption    string `json:"chosen_option,omitempty"`
	Option          string `json:"option"`
	Pros            string `json:"pros,omitempty"`
	Cons            string `json:"cons,omitempty"`
	RejectionReason string `json:"rejection_reason,omitempty"`
	Status          string `json:"status,omitempty"`
}

// ComponentInspection aggregates every X-Ray inspection dimension for one node in a single query (6.2).
type ComponentInspection struct {
	ModelID          string                 `json:"model_id"`
	Version          int                    `json:"version"`
	Node             XRayNode               `json:"node"`
	Responsibilities []string               `json:"responsibilities"`
	OwnedComponents  []NodeRef              `json:"owned_components"`
	Dependencies     struct {
		Upstream   []EdgeNodeRef `json:"upstream"`
		Downstream []EdgeNodeRef `json:"downstream"`
	} `json:"dependencies"`
	Interfaces    []InterfaceSpec        `json:"interfaces"`
	DataOwnership DataOwnership          `json:"data_ownership"`
	Evidence      []InspectedClaim       `json:"evidence"`
	Assumptions   []InspectedAssumption  `json:"assumptions"`
	FailureModes  []FailureMode          `json:"failure_modes"`
	Alternatives  []InspectedAlternative `json:"alternatives"`
}

// InspectComponent returns responsibilities, dependencies, interfaces, data ownership,
// evidence, assumptions, failure modes, and alternatives for a single component in one transaction (6.2).
func (s *Store) InspectComponent(ctx context.Context, tenantID, modelID string, number int, nodeID string) (ComponentInspection, error) {
	if !ids.Valid(modelID) {
		return ComponentInspection{}, ErrModelNotFound
	}
	if !ids.Valid(nodeID) {
		return ComponentInspection{}, ErrNodeNotFound
	}

	var out ComponentInspection
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		g, err := graphAtTx(ctx, tx, tenantID, modelID, number)
		if err != nil {
			return err
		}
		nodeByID := make(map[string]Node, len(g.Nodes))
		for _, n := range g.Nodes {
			nodeByID[n.ID] = n
		}
		target, ok := nodeByID[nodeID]
		if !ok {
			return ErrNodeNotFound
		}

		out = BuildComponentInspectionFromGraph(g, target)

		// 1. Claims & supporting evidence referencing this node.
		claims, err := loadNodeClaimsTx(ctx, tx, modelID, nodeID)
		if err != nil {
			return fmt.Errorf("load node claims: %w", err)
		}
		out.Evidence = claims
		// 2. Assumptions referencing this node.
		assumptions, err := loadNodeAssumptionsTx(ctx, tx, modelID, nodeID)
		if err != nil {
			return fmt.Errorf("load node assumptions: %w", err)
		}
		out.Assumptions = assumptions
		// 3. Alternatives from ADRs affecting this node.
		alts, err := loadNodeAlternativesTx(ctx, tx, modelID, nodeID)
		if err != nil {
			return fmt.Errorf("load node alternatives: %w", err)
		}
		out.Alternatives = append(out.Alternatives, alts...)
		return nil
	})
	return out, err
}

// BuildComponentInspectionFromGraph computes the graph-derived inspection fields (responsibilities,
// dependencies, interfaces, data ownership, failure modes) purely from an in-memory Graph snapshot.
func BuildComponentInspectionFromGraph(g Graph, target Node) ComponentInspection {
	nodeByID := make(map[string]Node, len(g.Nodes))
	for _, n := range g.Nodes {
		nodeByID[n.ID] = n
	}
	toRef := func(n Node) NodeRef {
		return NodeRef{
			ID:            n.ID,
			Key:           n.Key,
			Type:          n.Type,
			Name:          n.Name,
			Layer:         ResolveXRayLayer(n),
			EvidenceState: n.EvidenceState,
		}
	}

	var out ComponentInspection
	out.ModelID = g.Version.ModelID
	out.Version = g.Version.Number
	out.Node = XRayNode{Node: target, Layer: ResolveXRayLayer(target)}
	out.Responsibilities = []string{}
	out.OwnedComponents = []NodeRef{}
	out.Dependencies.Upstream = []EdgeNodeRef{}
	out.Dependencies.Downstream = []EdgeNodeRef{}
	out.Interfaces = []InterfaceSpec{}
	out.DataOwnership = DataOwnership{Owns: []NodeRef{}, Reads: []NodeRef{}, Writes: []NodeRef{}}
	out.Evidence = []InspectedClaim{}
	out.Assumptions = []InspectedAssumption{}
	out.FailureModes = []FailureMode{}
	out.Alternatives = []InspectedAlternative{}

	if strings.TrimSpace(target.Description) != "" {
		out.Responsibilities = append(out.Responsibilities, target.Description)
	}
	if target.OwnerDomain != "" {
		out.Responsibilities = append(out.Responsibilities, fmt.Sprintf("Owned by domain: %s", target.OwnerDomain))
	}
	if target.Properties != nil {
		if rs, ok := target.Properties["responsibilities"].([]any); ok {
			for _, item := range rs {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					out.Responsibilities = append(out.Responsibilities, s)
				}
			}
		}
		if fms, ok := target.Properties["failure_modes"].([]any); ok {
			for _, item := range fms {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					out.FailureModes = append(out.FailureModes, FailureMode{
						Title: s, Severity: "MEDIUM", Source: "PROPERTY", Description: s,
					})
				}
			}
		}
	}

	hasFailoverOrReplica := false
	syncDownstreamCount := 0

	for _, e := range g.Edges {
		if e.SourceNodeID == target.ID {
			other, ok := nodeByID[e.TargetNodeID]
			if !ok {
				continue
			}
			ref := toRef(other)
			edgeRef := EdgeNodeRef{
				Node: ref, EdgeID: e.ID, RelationType: e.RelationType,
				Protocol: e.Protocol, SyncMode: e.SyncMode,
			}

			switch e.RelationType {
			case "owns":
				out.OwnedComponents = append(out.OwnedComponents, ref)
				if isDataNode(other.Type) {
					out.DataOwnership.Owns = append(out.DataOwnership.Owns, ref)
				}
				if isInterfaceNode(other.Type) {
					out.Interfaces = append(out.Interfaces, InterfaceSpec{
						Role: "EXPOSES", RelationType: e.RelationType, Protocol: e.Protocol, SyncMode: e.SyncMode, Target: ref,
					})
				}
			case "reads":
				out.Dependencies.Downstream = append(out.Dependencies.Downstream, edgeRef)
				if isDataNode(other.Type) {
					out.DataOwnership.Reads = append(out.DataOwnership.Reads, ref)
				}
			case "writes":
				out.Dependencies.Downstream = append(out.Dependencies.Downstream, edgeRef)
				if isDataNode(other.Type) {
					out.DataOwnership.Writes = append(out.DataOwnership.Writes, ref)
				}
			case "publishes":
				out.Dependencies.Downstream = append(out.Dependencies.Downstream, edgeRef)
				out.Interfaces = append(out.Interfaces, InterfaceSpec{
					Role: "PUBLISHES", RelationType: e.RelationType, Protocol: e.Protocol, SyncMode: e.SyncMode, Target: ref,
				})
			case "subscribes":
				out.Dependencies.Downstream = append(out.Dependencies.Downstream, edgeRef)
				out.Interfaces = append(out.Interfaces, InterfaceSpec{
					Role: "SUBSCRIBES", RelationType: e.RelationType, Protocol: e.Protocol, SyncMode: e.SyncMode, Target: ref,
				})
			case "calls":
				out.Dependencies.Downstream = append(out.Dependencies.Downstream, edgeRef)
				if e.SyncMode == "" || e.SyncMode == "SYNC" {
					syncDownstreamCount++
				}
				if isInterfaceNode(other.Type) {
					out.Interfaces = append(out.Interfaces, InterfaceSpec{
						Role: "CONSUMES", RelationType: e.RelationType, Protocol: e.Protocol, SyncMode: e.SyncMode, Target: ref,
					})
				}
			case "fails_over_to", "replicates":
				hasFailoverOrReplica = true
				out.Dependencies.Downstream = append(out.Dependencies.Downstream, edgeRef)
			default:
				out.Dependencies.Downstream = append(out.Dependencies.Downstream, edgeRef)
			}

			if other.Type == "threat" {
				out.FailureModes = append(out.FailureModes, FailureMode{
					Title: other.Name, Severity: "HIGH", Source: "GRAPH_THREAT",
					Description: other.Description, RelatedID: other.ID,
				})
			} else if other.Type == "slo" {
				out.FailureModes = append(out.FailureModes, FailureMode{
					Title: "SLO breach risk: " + other.Name, Severity: "MEDIUM", Source: "GRAPH_SLO",
					Description: other.Description, RelatedID: other.ID,
				})
			}
		} else if e.TargetNodeID == target.ID {
			other, ok := nodeByID[e.SourceNodeID]
			if !ok {
				continue
			}
			ref := toRef(other)
			out.Dependencies.Upstream = append(out.Dependencies.Upstream, EdgeNodeRef{
				Node: ref, EdgeID: e.ID, RelationType: e.RelationType,
				Protocol: e.Protocol, SyncMode: e.SyncMode,
			})
			if e.RelationType == "fails_over_to" || e.RelationType == "replicates" {
				hasFailoverOrReplica = true
			}
			if other.Type == "threat" {
				out.FailureModes = append(out.FailureModes, FailureMode{
					Title: other.Name, Severity: "HIGH", Source: "GRAPH_THREAT",
					Description: other.Description, RelatedID: other.ID,
				})
			}
		}
	}

	// Structural failure mode analysis when redundancy or fan-out risks exist.
	if slices.Contains([]string{"service", "database", "cache", "queue", "topic"}, target.Type) && !hasFailoverOrReplica {
		out.FailureModes = append(out.FailureModes, FailureMode{
			Title:       "Single point of failure (no failover/replication edge)",
			Severity:    "MEDIUM",
			Source:      "STRUCTURAL",
			Description: fmt.Sprintf("%s %q has no modeled fails_over_to or replicates relationship.", target.Type, target.Name),
			Mitigation:  "Model standby failover or replication topology if high availability is required.",
		})
	}
	if syncDownstreamCount >= 3 {
		out.FailureModes = append(out.FailureModes, FailureMode{
			Title:       "Synchronous fan-out cascading latency risk",
			Severity:    "MEDIUM",
			Source:      "STRUCTURAL",
			Description: fmt.Sprintf("%q makes %d synchronous downstream calls, amplifying tail latency and partial outage risk.", target.Name, syncDownstreamCount),
			Mitigation:  "Consider circuit breakers, bounded timeouts, or asynchronous event decoupling.",
		})
	}
	return out
}

func isDataNode(t string) bool {
	return t == "database" || t == "table" || t == "cache"
}

func isInterfaceNode(t string) bool {
	return t == "api" || t == "event" || t == "topic" || t == "queue"
}

func loadNodeClaimsTx(ctx context.Context, tx pgx.Tx, modelID, nodeID string) ([]InspectedClaim, error) {
	rows, err := tx.Query(ctx, `
		SELECT c.id::text, c.statement, c.claim_class, c.confidence_band, c.lifecycle, c.review_status
		FROM claims c
		WHERE c.model_id = $1 AND $2::uuid = ANY(c.subject_node_ids)
		ORDER BY c.created_at DESC
		LIMIT 50`, modelID, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	claims := []InspectedClaim{}
	claimIdx := map[string]int{}
	var claimIDs []string
	for rows.Next() {
		var c InspectedClaim
		if err := rows.Scan(&c.ID, &c.Statement, &c.ClaimClass, &c.ConfidenceBand, &c.Status, &c.ReviewState); err != nil {
			return nil, err
		}
		c.Evidence = []InspectedEvidence{}
		claimIdx[c.ID] = len(claims)
		claimIDs = append(claimIDs, c.ID)
		claims = append(claims, c)
	}
	if err := rows.Err(); err != nil || len(claimIDs) == 0 {
		return claims, err
	}

	evRows, err := tx.Query(ctx, `
		SELECT ce.claim_id::text, e.id::text, e.source_ref, e.source_type, e.directness, e.trust_level, e.excerpt, e.captured_at
		FROM claim_evidence ce
		JOIN evidence_records e ON e.id = ce.evidence_id
		WHERE ce.claim_id = ANY($1::uuid[])
		ORDER BY e.captured_at DESC`, claimIDs)
	if err != nil {
		return claims, err
	}
	defer evRows.Close()
	for evRows.Next() {
		var claimID string
		var ev InspectedEvidence
		if err := evRows.Scan(&claimID, &ev.ID, &ev.SourceRef, &ev.SourceType, &ev.Directness, &ev.TrustLevel, &ev.Excerpt, &ev.CapturedAt); err != nil {
			return claims, err
		}
		if idx, ok := claimIdx[claimID]; ok {
			claims[idx].Evidence = append(claims[idx].Evidence, ev)
		}
	}
	return claims, evRows.Err()
}

func loadNodeAssumptionsTx(ctx context.Context, tx pgx.Tx, modelID, nodeID string) ([]InspectedAssumption, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, statement, rationale, owner, risk_if_wrong, validation_method, status
		FROM assumptions
		WHERE model_id = $1 AND $2::uuid = ANY(subject_node_ids)
		ORDER BY created_at DESC
		LIMIT 50`, modelID, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InspectedAssumption{}
	for rows.Next() {
		var a InspectedAssumption
		if err := rows.Scan(&a.ID, &a.Statement, &a.Rationale, &a.Owner, &a.RiskIfWrong, &a.ValidationMethod, &a.Status); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func loadNodeAlternativesTx(ctx context.Context, tx pgx.Tx, modelID, nodeID string) ([]InspectedAlternative, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, adr_number, title, chosen_option, status, alternatives_json
		FROM decisions
		WHERE model_id = $1 AND $2::uuid = ANY(affected_nodes)
		ORDER BY adr_number DESC
		LIMIT 25`, modelID, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InspectedAlternative{}
	for rows.Next() {
		var id, title, chosen, status string
		var adrNum int
		var altsRaw []byte
		if err := rows.Scan(&id, &adrNum, &title, &chosen, &status, &altsRaw); err != nil {
			return nil, err
		}
		var parsed []struct {
			Option          string `json:"option"`
			Pros            string `json:"pros"`
			Cons            string `json:"cons"`
			RejectionReason string `json:"rejection_reason"`
		}
		_ = json.Unmarshal(altsRaw, &parsed)
		for _, a := range parsed {
			out = append(out, InspectedAlternative{
				Source:          "ADR",
				DecisionID:      id,
				ADRNumber:       adrNum,
				Title:           title,
				ChosenOption:    chosen,
				Option:          a.Option,
				Pros:            a.Pros,
				Cons:            a.Cons,
				RejectionReason: a.RejectionReason,
				Status:          status,
			})
		}
	}
	return out, rows.Err()
}
