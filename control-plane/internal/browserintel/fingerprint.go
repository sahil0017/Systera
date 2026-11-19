package browserintel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/systera/control-plane/internal/evidence"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/ids"
)

// FingerprintDisclaimer is required on every ArchitectureFingerprint (§2.4, Checklist 5.1).
const FingerprintDisclaimer = "Structured hypothesis of observable capabilities and proposed architecture; explicitly not a factual map of hidden infrastructure."

// FingerprintLayer enumerates the 8 sub-graph layers of an Architecture Fingerprint (§2.4, Checklist 5.2).
type FingerprintLayer string

const (
	LayerCapability        FingerprintLayer = "capability"
	LayerDomainBoundary    FingerprintLayer = "domain_boundary"
	LayerUserJourney       FingerprintLayer = "user_journey"
	LayerServiceDependency FingerprintLayer = "service_dependency"
	LayerAPIInteraction    FingerprintLayer = "api_interaction"
	LayerDataEntity        FingerprintLayer = "data_entity"
	LayerEventFlow         FingerprintLayer = "event_flow"
	LayerInfrastructure    FingerprintLayer = "infrastructure"
)

// AllFingerprintLayers lists all 8 layers in inspection order.
var AllFingerprintLayers = []FingerprintLayer{
	LayerCapability,
	LayerDomainBoundary,
	LayerUserJourney,
	LayerServiceDependency,
	LayerAPIInteraction,
	LayerDataEntity,
	LayerEventFlow,
	LayerInfrastructure,
}

// LayerElement is a provenance-linked node within one of the 8 fingerprint sub-graph layers (5.4).
type LayerElement struct {
	NodeID        string           `json:"node_id"`
	Key           string           `json:"key"`
	NodeType      string           `json:"node_type"`
	Name          string           `json:"name"`
	Layer         FingerprintLayer `json:"layer"`
	EvidenceClass string           `json:"evidence_class"` // OBSERVED | INFERRED | PROPOSED
	Confidence    string           `json:"confidence"`     // HIGH | MEDIUM | LOW
	EvidenceRefs  []string         `json:"evidence_refs"`
	Provenance    string           `json:"provenance"`
}

// LayerEdge is a typed directed relation linking two elements inside or across layers.
type LayerEdge struct {
	EdgeID       string `json:"edge_id"`
	SourceNodeID string `json:"source_node_id"`
	TargetNodeID string `json:"target_node_id"`
	RelationType string `json:"relation_type"`
	Rationale    string `json:"rationale"`
}

// SubGraphLayer groups the nodes and edges belonging to a single fingerprint layer.
type SubGraphLayer struct {
	Layer    FingerprintLayer `json:"layer"`
	Elements []LayerElement   `json:"elements"`
	Edges    []LayerEdge      `json:"edges"`
}

// QualityAttributeItem models a latency, availability, or scale target as an explicit
// assumption or claim (Checklist 5.3).
type QualityAttributeItem struct {
	Attribute     string   `json:"attribute"` // latency | availability | scale
	Target        string   `json:"target"`
	Kind          string   `json:"kind"`           // ASSUMPTION | CLAIM
	EvidenceClass string   `json:"evidence_class"` // OBSERVED | INFERRED | PROPOSED
	Confidence    string   `json:"confidence"`
	Rationale     string   `json:"rationale"`
	EvidenceRefs  []string `json:"evidence_refs"`
	AssumptionID  string   `json:"assumption_id,omitempty"`
	ClaimID       string   `json:"claim_id,omitempty"`
}

// ArchitectureFingerprint is the complete §2.4 structured fingerprint.
type ArchitectureFingerprint struct {
	ID                string                             `json:"id"`
	TenantID          string                             `json:"tenant_id"`
	WebsiteID         string                             `json:"website_id"`
	SessionID         string                             `json:"session_id"`
	ModelID           string                             `json:"model_id,omitempty"`
	GraphVersion      int                                `json:"graph_version"`
	Domain            string                             `json:"domain"`
	Disclaimer        string                             `json:"disclaimer"`
	Layers            map[FingerprintLayer]SubGraphLayer `json:"layers"`
	QualityAttributes []QualityAttributeItem             `json:"quality_attributes"`
	Provenance        map[string]any                     `json:"provenance"`
	CreatedAt         time.Time                          `json:"created_at"`
	UpdatedAt         time.Time                          `json:"updated_at"`
}

// BuildFingerprint constructs an 8-layer ArchitectureFingerprint with explicit quality
// attributes and provenance links from a WebsiteProfile and its observations (§2.4).
func BuildFingerprint(w WebsiteProfile, obs []Observation) ArchitectureFingerprint {
	obsRefs := make([]string, 0, len(obs))
	for _, o := range obs {
		obsRefs = append(obsRefs, "observation:"+o.ID)
	}
	if len(obsRefs) == 0 {
		obsRefs = []string{"session:" + w.SessionID}
	}

	layers := make(map[FingerprintLayer]SubGraphLayer, len(AllFingerprintLayers))
	for _, l := range AllFingerprintLayers {
		layers[l] = SubGraphLayer{Layer: l, Elements: []LayerElement{}, Edges: []LayerEdge{}}
	}

	addElem := func(layer FingerprintLayer, nodeType, prefix, name, evClass, conf, prov string, refs []string) LayerElement {
		el := LayerElement{
			NodeID:        ids.New(),
			Key:           nodeKey(prefix, name),
			NodeType:      nodeType,
			Name:          name,
			Layer:         layer,
			EvidenceClass: evClass,
			Confidence:    conf,
			EvidenceRefs:  refs,
			Provenance:    prov,
		}
		sub := layers[layer]
		sub.Elements = append(sub.Elements, el)
		layers[layer] = sub
		return el
	}
	addEdge := func(layer FingerprintLayer, src, dst, rel, rationale string) {
		sub := layers[layer]
		sub.Edges = append(sub.Edges, LayerEdge{
			EdgeID:       ids.New(),
			SourceNodeID: src,
			TargetNodeID: dst,
			RelationType: rel,
			Rationale:    rationale,
		})
		layers[layer] = sub
	}

	// Root system + core domain + infrastructure region
	sysEl := addElem(LayerCapability, "system", "sys", w.Domain, "PROPOSED", "MEDIUM", "derived from website profile", obsRefs)
	domEl := addElem(LayerDomainBoundary, "domain", "dom", w.Domain+" Core", "INFERRED", "MEDIUM", "bounded context inferred from observed pages", obsRefs)
	addEdge(LayerDomainBoundary, sysEl.NodeID, domEl.NodeID, "owns", "system owns primary domain")

	svcEl := addElem(LayerServiceDependency, "service", "svc", w.Domain+" Application Service", "PROPOSED", "MEDIUM", "proposed core service", obsRefs)
	addEdge(LayerServiceDependency, domEl.NodeID, svcEl.NodeID, "owns", "domain owns application service")

	apiEl := addElem(LayerAPIInteraction, "api", "api", w.Domain+" Public API", "PROPOSED", "MEDIUM", "proposed HTTP interface for observed UI actions", obsRefs)
	addEdge(LayerAPIInteraction, svcEl.NodeID, apiEl.NodeID, "owns", "service exposes public API")

	dbEl := addElem(LayerDataEntity, "database", "db", w.Domain+" Primary Store", "PROPOSED", "MEDIUM", "proposed transactional store", obsRefs)
	addEdge(LayerDataEntity, svcEl.NodeID, dbEl.NodeID, "writes", "service persists state to primary store")

	topicEl := addElem(LayerEventFlow, "topic", "topic", w.Domain+" Domain Events", "PROPOSED", "LOW", "proposed async integration topic", obsRefs)
	addEdge(LayerEventFlow, svcEl.NodeID, topicEl.NodeID, "publishes", "service publishes domain events")

	regionEl := addElem(LayerInfrastructure, "region", "reg", "primary-region", "PROPOSED", "LOW", "proposed cloud deployment region", obsRefs)
	addEdge(LayerInfrastructure, svcEl.NodeID, regionEl.NodeID, "deploys_to", "service deploys to primary region")

	// Map observed/proposed capabilities & journeys into capability + user_journey layers
	seenCap := map[string]bool{}
	for _, c := range w.Capabilities {
		if seenCap[c.Name] {
			continue
		}
		seenCap[c.Name] = true
		refs := c.EvidenceRefs
		if len(refs) == 0 {
			refs = obsRefs
		}
		el := addElem(LayerCapability, "component", "cap", c.Name, c.EvidenceClass, c.Confidence, c.Rationale, refs)
		addEdge(LayerCapability, svcEl.NodeID, el.NodeID, "owns", "service implements capability")
	}
	for i, j := range w.Journeys {
		el := addElem(LayerUserJourney, "workload", fmt.Sprintf("jrn%d", i+1), j, "INFERRED", "MEDIUM", "inferred user journey from navigation", obsRefs)
		addEdge(LayerUserJourney, el.NodeID, apiEl.NodeID, "calls", "user journey invokes public API")
	}

	qa := []QualityAttributeItem{
		{
			Attribute:     "latency",
			Target:        "p95 < 250ms for primary interactive page actions",
			Kind:          "ASSUMPTION",
			EvidenceClass: "PROPOSED",
			Confidence:    "LOW",
			Rationale:     "Default interactive web SLO assumption; validate against real user telemetry.",
			EvidenceRefs:  obsRefs,
		},
		{
			Attribute:     "availability",
			Target:        "99.9% monthly service availability",
			Kind:          "ASSUMPTION",
			EvidenceClass: "PROPOSED",
			Confidence:    "LOW",
			Rationale:     "Standard multi-AZ web application availability assumption.",
			EvidenceRefs:  obsRefs,
		},
		{
			Attribute:     "scale",
			Target:        "Read-heavy traffic profile with bursty peak load",
			Kind:          "ASSUMPTION",
			EvidenceClass: "INFERRED",
			Confidence:    "MEDIUM",
			Rationale:     "Public browsing/search pages typically exhibit high read-to-write ratios.",
			EvidenceRefs:  obsRefs,
		},
	}

	return ArchitectureFingerprint{
		TenantID:          w.TenantID,
		WebsiteID:         w.ID,
		SessionID:         w.SessionID,
		ModelID:           w.ModelID,
		Domain:            w.Domain,
		Disclaimer:        FingerprintDisclaimer,
		Layers:            layers,
		QualityAttributes: qa,
		Provenance: map[string]any{
			"session_id":        w.SessionID,
			"website_id":        w.ID,
			"observation_refs":  obsRefs,
			"observation_count": len(obs),
			"generated_at":      time.Now().UTC().Format(time.RFC3339),
		},
	}
}

// BuildFingerprintPatch compiles all 8 fingerprint layers into a single canonical graph.Patch
// so diff, impact analysis, and versioning come for free (Checklist 5.2). It assumes none of
// the fingerprint is in the graph yet; ReconcileFingerprint handles rebuilds.
func BuildFingerprintPatch(baseVersion int, fp ArchitectureFingerprint, idempotencyKey string) graph.Patch {
	if idempotencyKey == "" {
		idempotencyKey = "fingerprint-" + fp.WebsiteID
	}
	ops := ReconcileFingerprint(&fp, graph.Graph{})
	return graph.Patch{BaseGraphVersion: &baseVersion, IdempotencyKey: idempotencyKey, Operations: ops}
}

// ReconcileFingerprint turns a freshly built fingerprint into the graph operations that make
// the model match it. Fingerprint nodes already in the graph (same website, same key) keep
// their node ids, edges keep their edge ids; changed nodes are updated, new ones added and
// ones no longer derived removed. It rewrites fp's node and edge ids to the graph's, so the
// persisted fingerprint always points at live nodes. A rebuild with nothing new returns no ops.
func ReconcileFingerprint(fp *ArchitectureFingerprint, cur graph.Graph) []graph.Operation {
	mine := func(props map[string]any) bool {
		_, layered := props["fingerprint_layer"]
		return layered && props["website_id"] == fp.WebsiteID
	}
	existing := map[string]graph.Node{} // key -> node
	for _, n := range cur.Nodes {
		if mine(n.Properties) {
			existing[n.Key] = n
		}
	}
	keyOf := map[string]string{} // final node id -> key
	remap := map[string]string{} // generated id -> final id
	var adds, updates, removes, edgeAdds []graph.Operation
	keep := map[string]bool{}

	for _, layer := range AllFingerprintLayers {
		sub := fp.Layers[layer]
		for i := range sub.Elements {
			el := &sub.Elements[i]
			props := map[string]any{"fingerprint_layer": string(el.Layer), "confidence": el.Confidence, "website_id": fp.WebsiteID}
			rationale := nonEmpty(el.Provenance, "derived for architecture fingerprint")
			if old, ok := existing[el.Key]; ok && old.Type == el.NodeType {
				remap[el.NodeID], el.NodeID = old.ID, old.ID
				keep[old.ID] = true
				if old.Name != el.Name || old.EvidenceState != el.EvidenceClass || old.Description != el.Provenance ||
					fmt.Sprint(old.Properties["confidence"]) != el.Confidence {
					name, desc, state := el.Name, el.Provenance, el.EvidenceClass
					pl, _ := json.Marshal(graph.UpdateNodePayload{Name: &name, Description: &desc, EvidenceState: &state, Properties: &props})
					updates = append(updates, graph.Operation{Op: graph.OpUpdateNode, TargetID: old.ID, Payload: pl, Rationale: rationale, EvidenceRefs: el.EvidenceRefs})
				}
			} else {
				pl, _ := json.Marshal(graph.AddNodePayload{Key: el.Key, Type: el.NodeType, Name: el.Name, Description: el.Provenance,
					EvidenceState: el.EvidenceClass, Properties: props})
				adds = append(adds, graph.Operation{Op: graph.OpAddNode, TargetID: el.NodeID, Payload: pl, Rationale: rationale, EvidenceRefs: el.EvidenceRefs})
				remap[el.NodeID] = el.NodeID
			}
			keyOf[el.NodeID] = el.Key
		}
		fp.Layers[layer] = sub
	}

	type ekey struct{ src, dst, rel string }
	existingEdges := map[ekey]graph.Edge{}
	nodeKey := map[string]string{}
	for _, n := range cur.Nodes {
		nodeKey[n.ID] = n.Key
	}
	for _, e := range cur.Edges {
		if mine(e.Properties) {
			existingEdges[ekey{nodeKey[e.SourceNodeID], nodeKey[e.TargetNodeID], e.RelationType}] = e
		}
	}
	keepEdge := map[string]bool{}
	for _, layer := range AllFingerprintLayers {
		sub := fp.Layers[layer]
		for i := range sub.Edges {
			ed := &sub.Edges[i]
			if id, ok := remap[ed.SourceNodeID]; ok {
				ed.SourceNodeID = id
			}
			if id, ok := remap[ed.TargetNodeID]; ok {
				ed.TargetNodeID = id
			}
			k := ekey{keyOf[ed.SourceNodeID], keyOf[ed.TargetNodeID], ed.RelationType}
			if old, ok := existingEdges[k]; ok {
				ed.EdgeID = old.ID
				keepEdge[old.ID] = true
				continue
			}
			pl, _ := json.Marshal(graph.AddEdgePayload{SourceNodeID: ed.SourceNodeID, TargetNodeID: ed.TargetNodeID, RelationType: ed.RelationType,
				Properties: map[string]any{"fingerprint_layer": string(layer), "website_id": fp.WebsiteID}})
			edgeAdds = append(edgeAdds, graph.Operation{Op: graph.OpAddEdge, TargetID: ed.EdgeID, Payload: pl,
				Rationale: nonEmpty(ed.Rationale, "architecture fingerprint relation")})
		}
		fp.Layers[layer] = sub
	}
	// Removals: edges first, then nodes (with any remaining edges) no longer derived.
	for _, e := range existingEdges {
		if !keepEdge[e.ID] && keep[e.SourceNodeID] && keep[e.TargetNodeID] {
			removes = append(removes, graph.Operation{Op: graph.OpRemoveEdge, TargetID: e.ID, Payload: json.RawMessage(`{}`),
				Rationale: "no longer derived by the rebuilt architecture fingerprint"})
		}
	}
	for _, n := range existing {
		if !keep[n.ID] {
			removes = append(removes, graph.Operation{Op: graph.OpRemoveNode, TargetID: n.ID, Payload: json.RawMessage(`{"cascade_edges":true}`),
				Rationale: "no longer derived by the rebuilt architecture fingerprint"})
		}
	}
	ops := append(append(append(removes, updates...), adds...), edgeAdds...)
	return ops
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// BuildAndPersistFingerprint builds the 8-layer fingerprint, optionally syncs its layers
// into the linked canonical architecture graph and records quality-attribute assumptions
// in the evidence ledger, and persists it in architecture_fingerprints.
func (s *Store) BuildAndPersistFingerprint(ctx context.Context, tenantID string, a Actor, websiteID string) (ArchitectureFingerprint, error) {
	w, err := s.GetWebsite(ctx, tenantID, a, websiteID)
	if err != nil {
		return ArchitectureFingerprint{}, err
	}
	obs, err := s.ListObservations(ctx, tenantID, a, w.SessionID)
	if err != nil {
		return ArchitectureFingerprint{}, err
	}

	fp := BuildFingerprint(w, obs)

	// 5.4: once the investigation has an architecture model, every element cites the ledger
	// evidence of its pages (verifiable, and revoked if the page is deleted) instead of a
	// raw observation reference. Observation ids stay in the provenance.
	if ids.Valid(w.ModelID) && s.ledger != nil {
		if err := s.syncLedger(ctx, tenantID, w.ID, evidence.Actor{Type: "USER", ID: a.UserID}); err != nil {
			return ArchitectureFingerprint{}, err
		}
		evByObs := map[string]string{}
		if err := s.tx(ctx, tenantID, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT observation_id::text, evidence_id::text FROM observation_ledger_evidence
				WHERE session_id = $1 AND model_id = $2 AND revoked_at IS NULL`, w.SessionID, w.ModelID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var o, e string
				if err := rows.Scan(&o, &e); err != nil {
					return err
				}
				evByObs[o] = e
			}
			return rows.Err()
		}); err != nil {
			return ArchitectureFingerprint{}, err
		}
		toLedger := func(refs []string) []string {
			out := make([]string, 0, len(refs))
			for _, r := range refs {
				if e, ok := evByObs[strings.TrimPrefix(r, "observation:")]; ok {
					out = append(out, "evidence:"+e)
				}
			}
			return dedupeStrings(out)
		}
		for layer, sub := range fp.Layers {
			for i := range sub.Elements {
				sub.Elements[i].EvidenceRefs = toLedger(sub.Elements[i].EvidenceRefs)
			}
			fp.Layers[layer] = sub
		}
		for i := range fp.QualityAttributes {
			fp.QualityAttributes[i].EvidenceRefs = toLedger(fp.QualityAttributes[i].EvidenceRefs)
		}
	}

	prev, hadPrev, err := s.loadFingerprint(ctx, tenantID, w.ID)
	if err != nil {
		return ArchitectureFingerprint{}, err
	}

	// Integrate with the canonical architecture graph when the website has a model (5.2).
	if ids.Valid(w.ModelID) && s.graphSvc != nil {
		cur, err := s.graphSvc.GraphAt(ctx, tenantID, w.ModelID, -1)
		if err != nil {
			return ArchitectureFingerprint{}, fmt.Errorf("load graph: %w", err)
		}
		fp.GraphVersion = cur.Version.Number
		ops := ReconcileFingerprint(&fp, cur)
		if len(ops) > 0 {
			base := cur.Version.Number
			res, err := s.graphSvc.ApplyPatch(ctx, tenantID, w.ModelID, graph.Patch{BaseGraphVersion: &base,
				IdempotencyKey: fmt.Sprintf("fp-%s-v%d", w.ID, base), Operations: ops}, graph.Author{Type: "USER", ID: a.UserID}, ids.New())
			if err != nil {
				return ArchitectureFingerprint{}, fmt.Errorf("apply fingerprint to the architecture graph: %w", err)
			}
			if !res.Accepted || res.NewGraphVersion == nil {
				msgs := make([]string, 0, len(res.ValidationFindings))
				for _, f := range res.ValidationFindings {
					msgs = append(msgs, f.Code+": "+f.Message)
				}
				return ArchitectureFingerprint{}, fmt.Errorf("fingerprint patch not accepted (status %s): %s", res.Status, strings.Join(msgs, "; "))
			}
			fp.GraphVersion = *res.NewGraphVersion
		}
	}

	// Record quality-attribute assumptions in the evidence ledger (5.3). An unchanged
	// assumption from the previous build is reused; a changed one supersedes (retires) it.
	if ids.Valid(w.ModelID) && s.ledger != nil {
		evActor := evidence.Actor{Type: "USER", ID: a.UserID}
		live := map[string]evidence.Assumption{}
		if hadPrev && prev.ModelID == w.ModelID {
			as, err := s.ledger.ListAssumptions(ctx, tenantID, w.ModelID, "")
			if err != nil {
				return ArchitectureFingerprint{}, err
			}
			for _, x := range as {
				if x.Status == "ACTIVE" || x.Status == "VALIDATED" {
					live[x.ID] = x
				}
			}
		}
		prevByAttr := map[string]string{}
		for _, q := range prev.QualityAttributes {
			prevByAttr[q.Attribute] = q.AssumptionID
		}
		for i, qa := range fp.QualityAttributes {
			if qa.Kind != "ASSUMPTION" {
				continue
			}
			statement := fmt.Sprintf("[%s] %s", qa.Attribute, qa.Target)
			if old, ok := live[prevByAttr[qa.Attribute]]; ok && old.Statement == statement {
				fp.QualityAttributes[i].AssumptionID = old.ID
				continue
			}
			rec, err := s.ledger.RecordAssumption(ctx, tenantID, w.ModelID, evidence.AssumptionInput{
				Statement: statement, Rationale: qa.Rationale, RiskIfWrong: "MEDIUM",
				ValidationMethod: "Benchmark or production telemetry verification",
			}, evActor)
			if err != nil {
				return ArchitectureFingerprint{}, fmt.Errorf("record %s assumption: %w", qa.Attribute, err)
			}
			fp.QualityAttributes[i].AssumptionID = rec.ID
			if old, ok := live[prevByAttr[qa.Attribute]]; ok {
				if _, _, err := s.ledger.SetAssumptionStatus(ctx, tenantID, w.ModelID, old.ID, evidence.AssumptionStatusInput{
					Status: "RETIRED", Note: "superseded by a rebuilt architecture fingerprint"}, evActor); err != nil {
					return ArchitectureFingerprint{}, err
				}
			}
		}
	}

	layersJSON, _ := json.Marshal(fp.Layers)
	qaJSON, _ := json.Marshal(fp.QualityAttributes)
	provJSON, _ := json.Marshal(fp.Provenance)

	err = s.tx(ctx, tenantID, func(tx pgx.Tx) error {
		var modelPtr *string
		if ids.Valid(fp.ModelID) {
			modelPtr = &fp.ModelID
		}
		return tx.QueryRow(ctx, `
			INSERT INTO architecture_fingerprints (
				tenant_id, website_id, session_id, model_id, graph_version,
				domain, disclaimer, layers_json, quality_attributes_json, provenance_json
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (website_id) DO UPDATE SET
				model_id = EXCLUDED.model_id,
				graph_version = EXCLUDED.graph_version,
				layers_json = EXCLUDED.layers_json,
				quality_attributes_json = EXCLUDED.quality_attributes_json,
				provenance_json = EXCLUDED.provenance_json,
				updated_at = NOW()
			RETURNING id::text, created_at, updated_at`,
			tenantID, w.ID, w.SessionID, modelPtr, fp.GraphVersion,
			fp.Domain, fp.Disclaimer, layersJSON, qaJSON, provJSON,
		).Scan(&fp.ID, &fp.CreatedAt, &fp.UpdatedAt)
	})
	return fp, err
}

// loadFingerprint returns the persisted fingerprint of a website, if any.
func (s *Store) loadFingerprint(ctx context.Context, tenantID, websiteID string) (ArchitectureFingerprint, bool, error) {
	var fp ArchitectureFingerprint
	var found bool
	err := s.tx(ctx, tenantID, func(tx pgx.Tx) error {
		var layersRaw, qaRaw, provRaw []byte
		err := tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, website_id::text, session_id::text,
			       COALESCE(model_id::text, ''), graph_version, domain, disclaimer,
			       layers_json, quality_attributes_json, provenance_json, created_at, updated_at
			FROM architecture_fingerprints WHERE website_id = $1`, websiteID).Scan(
			&fp.ID, &fp.TenantID, &fp.WebsiteID, &fp.SessionID, &fp.ModelID, &fp.GraphVersion, &fp.Domain, &fp.Disclaimer,
			&layersRaw, &qaRaw, &provRaw, &fp.CreatedAt, &fp.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if err := json.Unmarshal(layersRaw, &fp.Layers); err != nil {
			return fmt.Errorf("decode fingerprint layers: %w", err)
		}
		if err := json.Unmarshal(qaRaw, &fp.QualityAttributes); err != nil {
			return fmt.Errorf("decode quality attributes: %w", err)
		}
		return json.Unmarshal(provRaw, &fp.Provenance)
	})
	return fp, found, err
}

// GetFingerprint retrieves the persisted ArchitectureFingerprint for a website, or builds
// one on the fly from the WebsiteProfile if not yet materialized (Checklist 5.5).
func (s *Store) GetFingerprint(ctx context.Context, tenantID string, a Actor, websiteID string) (ArchitectureFingerprint, error) {
	if !ids.Valid(websiteID) {
		return ArchitectureFingerprint{}, ErrNotFound
	}
	w, err := s.GetWebsite(ctx, tenantID, a, websiteID) // authorizes the caller
	if err != nil {
		return ArchitectureFingerprint{}, err
	}
	fp, found, err := s.loadFingerprint(ctx, tenantID, websiteID)
	if err != nil || found {
		return fp, err
	}
	obs, err := s.ListObservations(ctx, tenantID, a, w.SessionID)
	if err != nil {
		return ArchitectureFingerprint{}, err
	}
	return BuildFingerprint(w, obs), nil
}
