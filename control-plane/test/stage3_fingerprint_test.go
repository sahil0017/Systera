package test

// Stage 3 Architecture Fingerprint (checklist 5.1-5.5) integration test. Requires
// SYSTERA_TEST_DATABASE_URL.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/systera/control-plane/internal/auth"
	bi "github.com/systera/control-plane/internal/browserintel"
	"github.com/systera/control-plane/internal/evidence"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/httpserver"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/projects"
	"github.com/systera/control-plane/internal/runs"
	"github.com/systera/control-plane/internal/scheduler"
)

func TestArchitectureFingerprintEndToEnd(t *testing.T) {
	l := newLensEnv(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(debugOut(), nil))
	ledger := evidence.NewStore(l.pool)
	gsvc := graph.NewService(l.pool)
	store := bi.NewStore(l.pool, projects.NewPostgresRepository(l.pool), gsvc)
	store.SetLedger(ledger)
	store.SetRuntime(runs.NewPostgresRepository(l.pool), scheduler.NewStore(l.pool, logger))
	issuer, _ := auth.NewTokenIssuer(testPrivateKey, "", testIssuer, testAudience)
	srv := httpserver.New(httpserver.Options{
		Logger: logger, Projects: projects.NewPostgresRepository(l.pool), Runs: runs.NewPostgresRepository(l.pool),
		Events: outbox.NewPostgresStore(l.pool, logger), Graph: gsvc, Impact: gsvc, Evidence: ledger, Browser: store, TokenIssuer: issuer,
		Authenticator: auth.NewAuthenticator(newVerifier(t), auth.NewPostgresMemberships(l.pool)),
		Readiness:     []httpserver.ReadinessCheck{{Name: "postgres", Check: postgres.PingCheck(l.pool)}},
	})
	l.c = client{t: t, handler: srv.Handler()}
	var tk struct {
		AccessToken string `json:"access_token"`
	}
	l.call(http.MethodPost, "/v1/auth/lens-token", l.token, nil, http.StatusCreated, &tk)
	lens := tk.AccessToken

	var sess bi.Session
	l.call(http.MethodPost, "/v1/browser/sessions", lens, sessionBody("acme-shop.com", []string{"https://acme-shop.com/", "https://acme-shop.com/cart", "https://acme-shop.com/pricing"}, 3), http.StatusCreated, &sess)
	var home, cart bi.Observation
	l.call(http.MethodPost, "/v1/browser/observations", lens, obsBody(sess.ID, "https://acme-shop.com/", nil), http.StatusCreated, &home)
	l.call(http.MethodPost, "/v1/browser/observations", lens, obsBody(sess.ID, "https://acme-shop.com/cart", func(o map[string]any) {
		o["observations"].(map[string]any)["headings"] = []string{"Your basket"}
	}), http.StatusCreated, &cart)
	var prof bi.WebsiteProfile
	l.call(http.MethodPost, "/v1/browser/analyze", lens, map[string]any{"session_id": sess.ID}, http.StatusCreated, &prof)
	base := "/v1/websites/" + prof.ID + "/fingerprint"

	checkShape := func(fp bi.ArchitectureFingerprint, what string) {
		t.Helper()
		// 5.1 explicitly a hypothesis, not a factual map of hidden infrastructure.
		if !strings.Contains(fp.Disclaimer, "not a factual map of hidden infrastructure") || fp.Domain != "acme-shop.com" {
			t.Fatalf("%s: disclaimer/domain: %+v", what, fp)
		}
		// 5.2 all eight sub-graphs.
		for _, layer := range bi.AllFingerprintLayers {
			sub, ok := fp.Layers[layer]
			if !ok || len(sub.Elements) == 0 {
				t.Fatalf("%s: layer %s empty", what, layer)
			}
			// 5.4 every element: evidence, confidence, provenance; nothing behind the UI is OBSERVED.
			for _, el := range sub.Elements {
				if len(el.EvidenceRefs) == 0 || el.Confidence == "" || el.Provenance == "" || el.EvidenceClass == "" {
					t.Fatalf("%s: element without evidence/confidence/provenance: %+v", what, el)
				}
				if el.EvidenceClass == "OBSERVED" && layer != bi.LayerCapability && layer != bi.LayerUserJourney {
					t.Fatalf("%s: %s layer element labelled OBSERVED: %+v", what, layer, el)
				}
			}
		}
		// 5.3 latency / availability / scale, each an explicit assumption or claim.
		attrs := map[string]bool{}
		for _, q := range fp.QualityAttributes {
			attrs[q.Attribute] = true
			if (q.Kind != "ASSUMPTION" && q.Kind != "CLAIM") || q.Rationale == "" || q.Target == "" {
				t.Fatalf("%s: quality attribute %+v", what, q)
			}
		}
		if !attrs["latency"] || !attrs["availability"] || !attrs["scale"] {
			t.Fatalf("%s: quality attributes %v", what, attrs)
		}
	}

	// ---- 5.5 GET before anything is built: derived on the fly, not persisted.
	var onTheFly bi.ArchitectureFingerprint
	l.call(http.MethodGet, base, lens, nil, http.StatusOK, &onTheFly)
	checkShape(onTheFly, "on-the-fly")
	if onTheFly.ID != "" || dbCount(t, l.pool, `SELECT count(*) FROM architecture_fingerprints WHERE website_id = $1`, prof.ID) != 0 {
		t.Fatal("GET persisted a fingerprint")
	}
	var layerOnly struct {
		Layer bi.SubGraphLayer `json:"layer"`
	}
	l.call(http.MethodGet, base+"?layer=data_entity", lens, nil, http.StatusOK, &layerOnly)
	if layerOnly.Layer.Layer != bi.LayerDataEntity || len(layerOnly.Layer.Elements) == 0 {
		t.Fatalf("layer filter: %+v", layerOnly)
	}
	l.call(http.MethodGet, base+"?layer=bogus", lens, nil, http.StatusBadRequest, nil)

	// ---- Ownership: other members and tenants cannot read or build it.
	l.call(http.MethodGet, base, l.engTok, nil, http.StatusNotFound, nil)
	l.call(http.MethodPost, base, l.engTok, nil, http.StatusNotFound, nil)
	l.call(http.MethodGet, base, l.otherTok, nil, http.StatusNotFound, nil)
	l.call(http.MethodPost, base, l.otherTok, nil, http.StatusNotFound, nil)

	// ---- POST before promotion: persisted, no model, no graph version.
	var fp0 bi.ArchitectureFingerprint
	l.call(http.MethodPost, base, lens, nil, http.StatusCreated, &fp0)
	checkShape(fp0, "unpromoted")
	if fp0.ID == "" || fp0.ModelID != "" || fp0.GraphVersion != 0 {
		t.Fatalf("unpromoted fingerprint: id=%q model=%q v=%d", fp0.ID, fp0.ModelID, fp0.GraphVersion)
	}
	var stored bi.ArchitectureFingerprint
	l.call(http.MethodGet, base, lens, nil, http.StatusOK, &stored)
	if stored.ID != fp0.ID {
		t.Fatal("GET did not return the persisted fingerprint")
	}

	// ---- After promotion: layers become graph versions (5.2), assumptions go to the ledger (5.3).
	var promoted bi.WebsiteProfile
	l.call(http.MethodPost, "/v1/websites/"+prof.ID+"/promote", lens, nil, http.StatusOK, &promoted)
	m, err := gsvc.ModelForProject(ctx, l.tenantID, promoted.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	seedVersion := m.CurrentVersion
	var fp1 bi.ArchitectureFingerprint
	l.call(http.MethodPost, base, lens, nil, http.StatusCreated, &fp1)
	checkShape(fp1, "promoted")
	if fp1.ID != fp0.ID || fp1.ModelID != promoted.ModelID {
		t.Fatalf("promoted fingerprint: %+v", fp1)
	}
	if fp1.GraphVersion != seedVersion+1 {
		t.Fatalf("fingerprint layers were not applied to the graph: graph_version %d, seed version %d", fp1.GraphVersion, seedVersion)
	}
	// Every fingerprint element is a live node carrying its layer.
	layerCounts := map[string]int{}
	rows, err := l.pool.Query(ctx, `SELECT properties->>'fingerprint_layer', count(*) FROM architecture_nodes
		WHERE model_id = $1 AND valid_to_version IS NULL AND properties ? 'fingerprint_layer' GROUP BY 1`, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var layer string
		var n int
		_ = rows.Scan(&layer, &n)
		layerCounts[layer] = n
	}
	rows.Close()
	for _, layer := range bi.AllFingerprintLayers {
		if layerCounts[string(layer)] != len(fp1.Layers[layer].Elements) {
			t.Fatalf("graph layer %s: %d nodes, fingerprint has %d", layer, layerCounts[string(layer)], len(fp1.Layers[layer].Elements))
		}
	}
	// Diff and impact work on the fingerprint version.
	var diff graph.Diff
	l.call(http.MethodGet, "/v1/architecture/"+m.ID+"/diff?from="+itoa(seedVersion)+"&to="+itoa(fp1.GraphVersion), l.token, nil, http.StatusOK, &diff)
	if len(diff.Nodes.Added) == 0 || len(diff.Edges.Added) == 0 {
		t.Fatalf("diff seed -> fingerprint: %d nodes, %d edges", len(diff.Nodes.Added), len(diff.Edges.Added))
	}
	svc := fp1.Layers[bi.LayerServiceDependency].Elements[0]
	impact, err := gsvc.ImpactOf(ctx, l.tenantID, m.ID, fp1.GraphVersion, []string{svc.NodeID}, graph.ImpactLimits{})
	if err != nil || len(impact.Nodes) == 0 {
		t.Fatalf("impact of the application service: %+v %v", impact, err)
	}
	// 5.4 element evidence resolves to the ledger once promoted: the cited observations are
	// ledger evidence on this model.
	for _, layer := range bi.AllFingerprintLayers {
		for _, el := range fp1.Layers[layer].Elements {
			for _, ref := range el.EvidenceRefs {
				if !strings.HasPrefix(ref, "evidence:") {
					t.Fatalf("promoted element %s cites %q, not ledger evidence", el.Name, ref)
				}
				if _, err := ledger.GetEvidence(ctx, l.tenantID, m.ID, strings.TrimPrefix(ref, "evidence:")); err != nil {
					t.Fatalf("element %s evidence %s: %v", el.Name, ref, err)
				}
			}
		}
	}
	// 5.3 every assumption exists in the ledger.
	for _, q := range fp1.QualityAttributes {
		if q.Kind == "ASSUMPTION" && q.AssumptionID == "" {
			t.Fatalf("assumption not recorded in the ledger: %+v", q)
		}
	}
	nAssumptions := dbCount(t, l.pool, `SELECT count(*) FROM assumptions WHERE model_id = $1`, m.ID)
	if nAssumptions != 3 {
		t.Fatalf("assumptions in ledger: %d", nAssumptions)
	}

	// ---- Rebuilding is safe: no duplicate assumptions, no error, the graph stays consistent.
	var fp2 bi.ArchitectureFingerprint
	l.call(http.MethodPost, base, lens, nil, http.StatusCreated, &fp2)
	checkShape(fp2, "rebuild")
	if n := dbCount(t, l.pool, `SELECT count(*) FROM assumptions WHERE model_id = $1`, m.ID); n != nAssumptions {
		t.Fatalf("rebuild duplicated assumptions: %d -> %d", nAssumptions, n)
	}
	if fp2.GraphVersion < fp1.GraphVersion {
		t.Fatalf("rebuild went back in graph version: %d -> %d", fp1.GraphVersion, fp2.GraphVersion)
	}
	if n := dbCount(t, l.pool, `SELECT count(*) FROM architecture_nodes WHERE model_id = $1 AND valid_to_version IS NULL AND properties ? 'fingerprint_layer'`, m.ID); n != layerTotal(fp2) {
		t.Fatalf("after rebuild the graph holds %d fingerprint nodes, fingerprint has %d", n, layerTotal(fp2))
	}
	for _, layer := range bi.AllFingerprintLayers {
		for _, el := range fp2.Layers[layer].Elements {
			var live int
			_ = l.pool.QueryRow(ctx, `SELECT count(*) FROM architecture_nodes WHERE model_id = $1 AND node_id = $2 AND valid_to_version IS NULL`, m.ID, el.NodeID).Scan(&live)
			if live != 1 {
				t.Fatalf("rebuilt element %s (%s) is not a live graph node", el.Name, el.NodeID)
			}
		}
	}

	// ---- New evidence: a third page adds a capability; the rebuild adds nodes and keeps ids.
	sysID := fp2.Layers[bi.LayerCapability].Elements[0].NodeID
	l.call(http.MethodPost, "/v1/browser/observations", lens, obsBody(sess.ID, "https://acme-shop.com/pricing", func(o map[string]any) {
		ob := o["observations"].(map[string]any)
		ob["headings"], ob["navigation"], ob["visible_features"] = []string{"Plans and pricing"}, []string{"Pricing"}, []string{}
		ob["forms"] = []any{}
	}), http.StatusCreated, nil)
	l.call(http.MethodPost, "/v1/browser/analyze", lens, map[string]any{"session_id": sess.ID}, http.StatusCreated, nil)
	var fp3 bi.ArchitectureFingerprint
	l.call(http.MethodPost, base, lens, nil, http.StatusCreated, &fp3)
	checkShape(fp3, "after new page")
	if fp3.GraphVersion != fp2.GraphVersion+1 {
		t.Fatalf("new capability did not produce a graph version: %d -> %d", fp2.GraphVersion, fp3.GraphVersion)
	}
	if fp3.Layers[bi.LayerCapability].Elements[0].NodeID != sysID {
		t.Fatal("rebuild changed the id of an unchanged node")
	}
	found := false
	for _, el := range fp3.Layers[bi.LayerCapability].Elements {
		found = found || el.Name == "Plans & Pricing"
	}
	if !found {
		t.Fatalf("new capability missing: %+v", fp3.Layers[bi.LayerCapability].Elements)
	}
	if n := dbCount(t, l.pool, `SELECT count(*) FROM architecture_nodes WHERE model_id = $1 AND valid_to_version IS NULL AND properties ? 'fingerprint_layer'`, m.ID); n != layerTotal(fp3) {
		t.Fatalf("graph holds %d fingerprint nodes, fingerprint has %d", n, layerTotal(fp3))
	}
	var d2 graph.Diff
	l.call(http.MethodGet, "/v1/architecture/"+m.ID+"/diff?from="+itoa(fp2.GraphVersion)+"&to="+itoa(fp3.GraphVersion), l.token, nil, http.StatusOK, &d2)
	if len(d2.Nodes.Removed) != 0 || len(d2.Nodes.Added) == 0 {
		t.Fatalf("diff for the new page: +%d -%d", len(d2.Nodes.Added), len(d2.Nodes.Removed))
	}
	if n := dbCount(t, l.pool, `SELECT count(*) FROM assumptions WHERE model_id = $1`, m.ID); n != nAssumptions {
		t.Fatalf("assumptions after new page: %d", n)
	}

	// ---- Deletion: removing a page removes the fingerprint derived from it.
	l.call(http.MethodDelete, "/v1/browser/observations/"+cart.ID, lens, nil, http.StatusOK, nil)
	if dbCount(t, l.pool, `SELECT count(*) FROM architecture_fingerprints WHERE website_id = $1`, prof.ID) != 0 {
		t.Fatal("fingerprint survived deletion of a page it was derived from")
	}
	l.call(http.MethodGet, base, lens, nil, http.StatusNotFound, nil)
	l.call(http.MethodDelete, "/v1/browser/sessions/"+sess.ID, lens, nil, http.StatusOK, nil)
	if dbCount(t, l.pool, `SELECT count(*) FROM architecture_fingerprints WHERE session_id = $1`, sess.ID) != 0 {
		t.Fatal("fingerprint survived session deletion")
	}
	_ = home
	_ = io.Discard
}

func layerTotal(fp bi.ArchitectureFingerprint) int {
	n := 0
	for _, l := range fp.Layers {
		n += len(l.Elements)
	}
	return n
}

func itoa(n int) string { return strconv.Itoa(n) }
