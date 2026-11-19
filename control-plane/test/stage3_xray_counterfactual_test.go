package test

// Stage 3 Architecture X-Ray (6.1-6.3) and Architecture Counterfactual (7.1-7.5)
// integration tests against a live PostgreSQL container (SYSTERA_TEST_DATABASE_URL).

import (
	"log/slog"
	"net/http"
	"testing"

	"github.com/systera/control-plane/internal/auth"
	bi "github.com/systera/control-plane/internal/browserintel"
	"github.com/systera/control-plane/internal/counterfactual"
	"github.com/systera/control-plane/internal/decisions"
	"github.com/systera/control-plane/internal/evidence"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/httpserver"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/projects"
	"github.com/systera/control-plane/internal/runs"
	"github.com/systera/control-plane/internal/scheduler"
)

func TestXRayAndCounterfactualEndToEnd(t *testing.T) {
	l := newLensEnv(t)
	logger := slog.New(slog.NewTextHandler(debugOut(), nil))
	ledger := evidence.NewStore(l.pool)
	gsvc := graph.NewService(l.pool)
	decStore := decisions.NewStore(l.pool)
	cfStore := counterfactual.NewStore(l.pool, gsvc, decStore)
	projRepo := projects.NewPostgresRepository(l.pool)
	runRepo := runs.NewPostgresRepository(l.pool)
	browserStore := bi.NewStore(l.pool, projRepo, gsvc)
	browserStore.SetLedger(ledger)
	browserStore.SetRuntime(runRepo, scheduler.NewStore(l.pool, logger))

	issuer, _ := auth.NewTokenIssuer(testPrivateKey, "", testIssuer, testAudience)
	srv := httpserver.New(httpserver.Options{
		Logger:         logger,
		Projects:       projRepo,
		Runs:           runRepo,
		Events:         outbox.NewPostgresStore(l.pool, logger),
		Graph:          gsvc,
		Impact:         gsvc,
		Evidence:       ledger,
		Decisions:      decStore,
		Browser:        browserStore,
		Counterfactual: cfStore,
		TokenIssuer:    issuer,
		Authenticator:  auth.NewAuthenticator(newVerifier(t), auth.NewPostgresMemberships(l.pool)),
		Readiness:      []httpserver.ReadinessCheck{{Name: "postgres", Check: postgres.PingCheck(l.pool)}},
	})
	l.c = client{t: t, handler: srv.Handler()}

	// 1. Create a project & canonical architecture model.
	var proj struct {
		ID string `json:"id"`
	}
	l.call(http.MethodPost, "/v1/projects", l.token, map[string]any{
		"name":        "Commerce X-Ray Project",
		"description": "Integration test for X-Ray & Counterfactual",
	}, http.StatusCreated, &proj)

	var createdModel struct {
		Model graph.Model `json:"model"`
	}
	l.call(http.MethodPost, "/v1/projects/"+proj.ID+"/architecture", l.token, nil, http.StatusCreated, &createdModel)
	modelID := createdModel.Model.ID

	// 2. Populate nodes across all 8 X-Ray layers via canonical GraphPatch.
	nActor, nDom, nSvc, nAPI := ids.New(), ids.New(), ids.New(), ids.New()
	nDB, nTopic, nDeploy, nSLO := ids.New(), ids.New(), ids.New(), ids.New()
	patchBody := map[string]any{
		"base_graph_version": 0,
		"idempotency_key":    "xray-seed-v1",
		"operations": []map[string]any{
			{"op": "add_node", "target_id": nActor, "payload": map[string]any{"key": "actor:shopper", "type": "actor", "name": "Shopper", "evidence_state": "PROPOSED"}, "rationale": "seed"},
			{"op": "add_node", "target_id": nDom, "payload": map[string]any{"key": "domain:orders", "type": "domain", "name": "Orders Domain", "evidence_state": "PROPOSED"}, "rationale": "seed"},
			{"op": "add_node", "target_id": nSvc, "payload": map[string]any{"key": "svc:checkout", "type": "service", "name": "Checkout Service", "description": "Handles checkout orchestration", "owner_domain": "Orders", "evidence_state": "PROPOSED"}, "rationale": "seed"},
			{"op": "add_node", "target_id": nAPI, "payload": map[string]any{"key": "api:checkout", "type": "api", "name": "POST /v1/checkout", "evidence_state": "PROPOSED"}, "rationale": "seed"},
			{"op": "add_node", "target_id": nDB, "payload": map[string]any{"key": "db:orders", "type": "database", "name": "Orders Postgres", "evidence_state": "PROPOSED"}, "rationale": "seed"},
			{"op": "add_node", "target_id": nTopic, "payload": map[string]any{"key": "topic:orders", "type": "topic", "name": "orders.placed", "evidence_state": "PROPOSED"}, "rationale": "seed"},
			{"op": "add_node", "target_id": nDeploy, "payload": map[string]any{"key": "dep:eks", "type": "deployment_unit", "name": "EKS Cluster", "evidence_state": "PROPOSED"}, "rationale": "seed"},
			{"op": "add_node", "target_id": nSLO, "payload": map[string]any{"key": "slo:checkout", "type": "slo", "name": "p99 < 250ms", "description": "Checkout latency SLO", "evidence_state": "PROPOSED"}, "rationale": "seed"},
			{"op": "add_edge", "target_id": ids.New(), "payload": map[string]any{"source_node_id": nActor, "target_node_id": nAPI, "relation_type": "calls", "protocol": "HTTPS", "sync_mode": "SYNC"}, "rationale": "seed"},
			{"op": "add_edge", "target_id": ids.New(), "payload": map[string]any{"source_node_id": nDom, "target_node_id": nSvc, "relation_type": "owns"}, "rationale": "seed"},
			{"op": "add_edge", "target_id": ids.New(), "payload": map[string]any{"source_node_id": nSvc, "target_node_id": nAPI, "relation_type": "owns", "protocol": "HTTPS"}, "rationale": "seed"},
			{"op": "add_edge", "target_id": ids.New(), "payload": map[string]any{"source_node_id": nSvc, "target_node_id": nDB, "relation_type": "writes", "protocol": "PostgreSQL"}, "rationale": "seed"},
			{"op": "add_edge", "target_id": ids.New(), "payload": map[string]any{"source_node_id": nSvc, "target_node_id": nTopic, "relation_type": "publishes", "sync_mode": "ASYNC"}, "rationale": "seed"},
			{"op": "add_edge", "target_id": ids.New(), "payload": map[string]any{"source_node_id": nSvc, "target_node_id": nDeploy, "relation_type": "deploys_to"}, "rationale": "seed"},
			{"op": "add_edge", "target_id": ids.New(), "payload": map[string]any{"source_node_id": nSvc, "target_node_id": nSLO, "relation_type": "owns"}, "rationale": "seed"},
		},
	}
	l.call(http.MethodPost, "/v1/architecture/"+modelID+"/patches", l.token, patchBody, http.StatusCreated, nil)

	// Attach an assumption and an ADR to Checkout Service so Component Inspection returns them in one query (6.2).
	l.call(http.MethodPost, "/v1/architecture/"+modelID+"/assumptions", l.token, map[string]any{
		"statement":         "Checkout stays below 1,000 req/s",
		"rationale":         "Peak traffic projection",
		"risk_if_wrong":     "HIGH",
		"validation_method": "k6 load test",
		"subject_node_ids":  []string{nSvc},
	}, http.StatusCreated, nil)

	l.call(http.MethodPost, "/v1/architecture/"+modelID+"/decisions", l.token, map[string]any{
		"model_version":   1,
		"title":           "Checkout Outbox Pattern",
		"context":         "Reliable event publishing on order creation",
		"chosen_option":   "PostgreSQL Transactional Outbox",
		"rationale":       "Avoids dual-write inconsistency",
		"confidence_band": "HIGH",
		"affected_nodes":  []string{nSvc},
		"alternatives": []map[string]any{
			{"option": "2PC / XA", "pros": "Atomic", "cons": "Slow locks", "rejection_reason": "Tail latency"},
		},
	}, http.StatusCreated, nil)

	// ---- 6.1 & 6.3: GET /v1/architecture/{model}/xray ----
	var xrayView graph.XRayView
	l.call(http.MethodGet, "/v1/architecture/"+modelID+"/xray", l.token, nil, http.StatusOK, &xrayView)
	if len(xrayView.Layers) != 8 {
		t.Fatalf("6.1 expected 8 X-Ray layers, got %d", len(xrayView.Layers))
	}
	for _, g := range xrayView.Layers {
		if g.NodeCount != 1 {
			t.Fatalf("6.1 expected 1 node in layer %s, got %d", g.ID, g.NodeCount)
		}
	}
	if len(xrayView.CrossLayerEdges) != 7 {
		t.Fatalf("6.1 expected 7 cross-layer edges, got %d", len(xrayView.CrossLayerEdges))
	}

	// ---- 6.3: Layer drill-down via neighbourhood query ----
	var dd graph.XRayDrillDown
	l.call(http.MethodGet, "/v1/architecture/"+modelID+"/xray/nodes/"+nSvc+"/drilldown?depth=1&direction=out", l.token, nil, http.StatusOK, &dd)
	if dd.CenterNodeID != nSvc || dd.CenterLayer != graph.LayerService {
		t.Fatalf("6.3 unexpected drill-down center: %+v", dd)
	}
	if len(dd.CrossLayerEdges) < 5 {
		t.Fatalf("6.3 expected >= 5 outbound cross-layer edges from Checkout Service, got %d", len(dd.CrossLayerEdges))
	}

	// ---- 6.2: Single-query component inspection ----
	var insp graph.ComponentInspection
	l.call(http.MethodGet, "/v1/architecture/"+modelID+"/xray/nodes/"+nSvc+"/inspect", l.token, nil, http.StatusOK, &insp)
	if insp.Node.ID != nSvc || insp.Node.Layer != graph.LayerService {
		t.Fatalf("6.2 unexpected inspected node: %+v", insp.Node)
	}
	if len(insp.Responsibilities) < 2 {
		t.Fatalf("6.2 expected responsibilities, got %v", insp.Responsibilities)
	}
	if len(insp.Interfaces) != 2 {
		t.Fatalf("6.2 expected 2 interfaces (API + topic), got %+v", insp.Interfaces)
	}
	if len(insp.DataOwnership.Writes) != 1 || insp.DataOwnership.Writes[0].ID != nDB {
		t.Fatalf("6.2 expected write ownership of DB, got %+v", insp.DataOwnership)
	}
	if len(insp.Assumptions) != 1 {
		t.Fatalf("6.2 expected 1 linked assumption, got %d", len(insp.Assumptions))
	}
	if len(insp.FailureModes) < 2 {
		t.Fatalf("6.2 expected >= 2 failure modes (SLO + structural SPOF), got %+v", insp.FailureModes)
	}
	if len(insp.Alternatives) != 1 || insp.Alternatives[0].Option != "2PC / XA" {
		t.Fatalf("6.2 expected 1 linked ADR alternative, got %+v", insp.Alternatives)
	}

	// ---- 7.1 - 7.5: Architecture Counterfactual per model ----
	var cfSet counterfactual.CounterfactualSet
	l.call(http.MethodPost, "/v1/architecture/"+modelID+"/alternatives", l.token, map[string]any{
		"subject": "Commerce Platform",
		"constraints": map[string]any{
			"weights": map[string]float64{
				"scale":                 5.0,
				"availability":          5.0,
				"compliance":            5.0,
				"cost":                  0.2,
				"latency":               0.2,
				"operational_maturity":  0.2,
				"migration_complexity":  0.2,
				"team_size":             3.0,
			},
		},
	}, http.StatusCreated, &cfSet)

	if len(cfSet.Alternatives) != 3 {
		t.Fatalf("7.1 expected 3 viable alternatives, got %d", len(cfSet.Alternatives))
	}
	if len(cfSet.DecisionMatrix.Rows) != 8 {
		t.Fatalf("7.2 expected 8 constraint rows in decision matrix, got %d", len(cfSet.DecisionMatrix.Rows))
	}
	if cfSet.DecisionMatrix.RecommendedStyle != counterfactual.StyleDomainMicroservices {
		t.Fatalf("7.2 expected domain_microservices recommended under scale/compliance weights, got %s", cfSet.DecisionMatrix.RecommendedStyle)
	}
	if cfSet.DecisionID == "" {
		t.Fatalf("7.3 expected ADR decision_id to be recorded")
	}
	// Verify each alternative materialized as its own distinct graph version (7.1).
	vers := map[int]bool{}
	for _, a := range cfSet.Alternatives {
		if a.GraphVersion <= 1 {
			t.Fatalf("7.1 expected alternative %s to materialize a new graph version > 1, got %d", a.Style, a.GraphVersion)
		}
		if vers[a.GraphVersion] {
			t.Fatalf("7.1 duplicate graph version %d across alternatives", a.GraphVersion)
		}
		vers[a.GraphVersion] = true
	}

	// Verify GET /v1/architecture/{model}/alternatives returns the persisted set (7.5).
	var fetched counterfactual.CounterfactualSet
	l.call(http.MethodGet, "/v1/architecture/"+modelID+"/alternatives", l.token, nil, http.StatusOK, &fetched)
	if fetched.SetID != cfSet.SetID || len(fetched.Alternatives) != 3 {
		t.Fatalf("7.5 GET alternatives mismatch: got set %s (%d alts)", fetched.SetID, len(fetched.Alternatives))
	}

	// ---- 7.4: Diff between two alternatives using existing version diff ----
	altA := cfSet.Alternatives[0].ID
	altB := cfSet.Alternatives[2].ID
	var altDiff counterfactual.AlternativeDiff
	l.call(http.MethodGet, "/v1/architecture/"+modelID+"/alternatives/diff?from="+altA+"&to="+altB, l.token, nil, http.StatusOK, &altDiff)
	if altDiff.GraphDiff.FromVersion == altDiff.GraphDiff.ToVersion {
		t.Fatalf("7.4 expected distinct graph versions in diff, got %d -> %d", altDiff.GraphDiff.FromVersion, altDiff.GraphDiff.ToVersion)
	}
	if altDiff.GraphDiff.Empty() {
		t.Fatalf("7.4 expected non-empty canonical graph diff between %s and %s", altDiff.FromAlternative.Style, altDiff.ToAlternative.Style)
	}
	if len(altDiff.ScoreDeltas) != 8 {
		t.Fatalf("7.4 expected 8 constraint score deltas, got %d", len(altDiff.ScoreDeltas))
	}

	// ---- 7.5: Website-scoped /v1/websites/{id}/alternatives ----
	var tk struct {
		AccessToken string `json:"access_token"`
	}
	l.call(http.MethodPost, "/v1/auth/lens-token", l.token, nil, http.StatusCreated, &tk)
	lens := tk.AccessToken

	var sess bi.Session
	l.call(http.MethodPost, "/v1/browser/sessions", lens, sessionBody("cf-shop.example.com", []string{"https://cf-shop.example.com/"}, 1), http.StatusCreated, &sess)
	l.call(http.MethodPost, "/v1/browser/observations", lens, obsBody(sess.ID, "https://cf-shop.example.com/", nil), http.StatusCreated, nil)
	var webProf bi.WebsiteProfile
	l.call(http.MethodPost, "/v1/browser/analyze", lens, map[string]any{"session_id": sess.ID}, http.StatusCreated, &webProf)

	var webCF counterfactual.CounterfactualSet
	l.call(http.MethodGet, "/v1/websites/"+webProf.ID+"/alternatives", lens, nil, http.StatusOK, &webCF)
	if len(webCF.Alternatives) != 3 || len(webCF.DecisionMatrix.Rows) != 8 {
		t.Fatalf("7.5 expected 3 website alternatives and 8 decision-matrix rows, got %d / %d", len(webCF.Alternatives), len(webCF.DecisionMatrix.Rows))
	}
}
