package test

// Stage 3 Engineering Learning Mode (§2.7, §8.5) and Compare websites / analysis modes (§7.6)
// integration test against a live PostgreSQL (SYSTERA_TEST_DATABASE_URL).

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/systera/control-plane/internal/auth"
	bi "github.com/systera/control-plane/internal/browserintel"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/learning"
	"github.com/systera/control-plane/internal/platform/httpserver"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/projects"
	"github.com/systera/control-plane/internal/runs"
	"github.com/systera/control-plane/internal/scheduler"
)

func TestLearningModeAndWebsiteComparisonEndToEnd(t *testing.T) {
	l := newLensEnv(t)
	logger := slog.New(slog.NewTextHandler(debugOut(), nil))
	gsvc := graph.NewService(l.pool)
	projRepo := projects.NewPostgresRepository(l.pool)
	runRepo := runs.NewPostgresRepository(l.pool)
	sched := scheduler.NewStore(l.pool, logger)
	browserStore := bi.NewStore(l.pool, projRepo, gsvc)
	browserStore.SetRuntime(runRepo, sched)
	issuer, _ := auth.NewTokenIssuer(testPrivateKey, "", testIssuer, testAudience)
	srv := httpserver.New(httpserver.Options{
		Logger: logger, Projects: projRepo, Runs: runRepo, Events: outbox.NewPostgresStore(l.pool, logger),
		Graph: gsvc, Impact: gsvc, Tasks: sched, Browser: browserStore, TokenIssuer: issuer,
		Learning:      learning.NewStore(l.pool, gsvc, nil),
		Authenticator: auth.NewAuthenticator(newVerifier(t), auth.NewPostgresMemberships(l.pool)),
		Readiness:     []httpserver.ReadinessCheck{{Name: "postgres", Check: postgres.PingCheck(l.pool)}},
	})
	l.c = client{t: t, handler: srv.Handler()}

	// ---- Learning Mode on a small architecture model.
	var proj struct {
		ID string `json:"id"`
	}
	l.call(http.MethodPost, "/v1/projects", l.token, map[string]any{"name": "Learning project"}, http.StatusCreated, &proj)
	var created struct {
		Model graph.Model `json:"model"`
	}
	l.call(http.MethodPost, "/v1/projects/"+proj.ID+"/architecture", l.token, nil, http.StatusCreated, &created)
	modelID := created.Model.ID
	nSvc, nAPI, nDB, nTopic := ids.New(), ids.New(), ids.New(), ids.New()
	l.call(http.MethodPost, "/v1/architecture/"+modelID+"/patches", l.token, map[string]any{
		"base_graph_version": 0, "idempotency_key": "learning-seed",
		"operations": []map[string]any{
			{"op": "add_node", "target_id": nSvc, "payload": map[string]any{"key": "svc:orders", "type": "service", "name": "Order Service", "description": "Places and tracks orders", "evidence_state": "PROPOSED"}, "rationale": "seed"},
			{"op": "add_node", "target_id": nAPI, "payload": map[string]any{"key": "api:orders", "type": "api", "name": "POST /orders", "evidence_state": "PROPOSED"}, "rationale": "seed"},
			{"op": "add_node", "target_id": nDB, "payload": map[string]any{"key": "db:orders", "type": "database", "name": "Orders DB", "evidence_state": "PROPOSED"}, "rationale": "seed"},
			{"op": "add_node", "target_id": nTopic, "payload": map[string]any{"key": "topic:orders", "type": "topic", "name": "orders.placed", "evidence_state": "PROPOSED"}, "rationale": "seed"},
			{"op": "add_edge", "target_id": ids.New(), "payload": map[string]any{"source_node_id": nSvc, "target_node_id": nAPI, "relation_type": "owns", "protocol": "HTTPS"}, "rationale": "seed"},
			{"op": "add_edge", "target_id": ids.New(), "payload": map[string]any{"source_node_id": nSvc, "target_node_id": nDB, "relation_type": "writes", "protocol": "PostgreSQL"}, "rationale": "seed"},
			{"op": "add_edge", "target_id": ids.New(), "payload": map[string]any{"source_node_id": nSvc, "target_node_id": nTopic, "relation_type": "publishes", "sync_mode": "ASYNC"}, "rationale": "seed"},
		},
	}, http.StatusCreated, nil)

	var ov learning.Overview
	l.call(http.MethodGet, "/v1/architecture/"+modelID+"/learning", l.token, nil, http.StatusOK, &ov)
	if len(ov.Components) != 4 || ov.Disclaimer == "" || len(ov.InterviewQuestions) == 0 || len(ov.Exercises) == 0 {
		t.Fatalf("overview: %+v", ov)
	}

	var lesson learning.Lesson
	l.call(http.MethodGet, "/v1/architecture/"+modelID+"/learning/nodes/"+nSvc, l.token, nil, http.StatusOK, &lesson)
	if lesson.NodeName != "Order Service" || lesson.EvidenceState != "PROPOSED" || lesson.WhyItExists.Summary == "" ||
		len(lesson.Alternatives) == 0 || len(lesson.FailureScenarios) == 0 || len(lesson.InterviewQuestions) == 0 || len(lesson.Exercises) == 0 {
		t.Fatalf("lesson: %+v", lesson)
	}
	if lesson.ResearchStatus != "UNAVAILABLE" { // no Pulse retriever wired in this test
		t.Fatalf("research status: %s", lesson.ResearchStatus)
	}
	outbox := false
	for _, e := range lesson.Exercises {
		outbox = outbox || e.Title == "Transactional outbox publisher"
	}
	if !outbox {
		t.Fatalf("a publishing service must get the outbox exercise: %+v", lesson.Exercises)
	}
	l.call(http.MethodGet, "/v1/architecture/"+modelID+"/learning/nodes/"+ids.New(), l.token, nil, http.StatusNotFound, nil)

	// Challenge the design: revised explanation, recorded with increasing revisions; graph unchanged.
	var c1, c2 learning.Challenge
	l.call(http.MethodPost, "/v1/architecture/"+modelID+"/learning/nodes/"+nSvc+"/challenges", l.token,
		map[string]any{"challenge": "This seems overkill, why not keep it simpler?"}, http.StatusCreated, &c1)
	l.call(http.MethodPost, "/v1/architecture/"+modelID+"/learning/nodes/"+nSvc+"/challenges", l.token,
		map[string]any{"challenge": "What happens to orders if the database is down?"}, http.StatusCreated, &c2)
	if c1.Revision != 1 || c2.Revision != 2 || c1.Revised.Concern != learning.ConcernComplexity || c2.Revised.Concern != learning.ConcernReliability || !c2.Revised.DesignUnchanged {
		t.Fatalf("challenges: %+v / %+v", c1, c2)
	}
	l.call(http.MethodPost, "/v1/architecture/"+modelID+"/learning/nodes/"+nSvc+"/challenges", l.token, map[string]any{"challenge": "  "}, http.StatusBadRequest, nil)
	l.call(http.MethodPost, "/v1/architecture/"+modelID+"/learning/nodes/"+nSvc+"/challenges", l.token,
		map[string]any{"challenge": strings.Repeat("x", learning.MaxChallengeChars+1)}, http.StatusBadRequest, nil)
	l.call(http.MethodGet, "/v1/architecture/"+modelID+"/learning/nodes/"+nSvc, l.token, nil, http.StatusOK, &lesson)
	if len(lesson.Challenges) != 2 || lesson.Challenges[0].Revision != 2 || lesson.GraphVersion != 1 {
		t.Fatalf("lesson challenges / graph version: %d %+v", lesson.GraphVersion, lesson.Challenges)
	}

	// ---- Analysis modes and comparison through the lens API.
	var tk struct {
		AccessToken string `json:"access_token"`
	}
	l.call(http.MethodPost, "/v1/auth/lens-token", l.token, nil, http.StatusCreated, &tk)
	lens := tk.AccessToken

	quick := sessionBody("shop-a.com", []string{"https://shop-a.com/", "https://shop-a.com/cart"}, 2)
	quick["analysis_mode"] = "quick_scan"
	l.call(http.MethodPost, "/v1/browser/sessions", lens, quick, http.StatusBadRequest, nil) // quick scan = current page only

	investigate := func(domain, mode string, pages map[string][]string) bi.WebsiteProfile {
		body := sessionBody(domain, nil, len(pages))
		body["analysis_mode"] = mode
		var sess bi.Session
		l.call(http.MethodPost, "/v1/browser/sessions", lens, body, http.StatusCreated, &sess)
		for url, features := range pages {
			l.call(http.MethodPost, "/v1/browser/observations", lens, obsBody(sess.ID, url, func(o map[string]any) {
				ob := o["observations"].(map[string]any)
				ob["headings"], ob["navigation"], ob["visible_features"], ob["forms"] = []string{"Welcome"}, features, []string{}, []any{}
			}), http.StatusCreated, nil)
		}
		var prof bi.WebsiteProfile
		l.call(http.MethodPost, "/v1/browser/analyze", lens, map[string]any{"session_id": sess.ID}, http.StatusCreated, &prof)
		return prof
	}
	a := investigate("shop-a.com", "quick_scan", map[string][]string{"https://shop-a.com/": {"Search", "Cart", "Checkout"}})
	b := investigate("shop-b.com", "compare_websites", map[string][]string{"https://shop-b.com/": {"Cart", "Pricing"}})

	var start bi.AnalysisStart
	r := l.c.do(http.MethodPost, "/v1/websites/"+a.ID+"/analysis", lens, "", map[string]string{"Idempotency-Key": "quick-1"})
	expect(t, r, http.StatusAccepted, "quick-scan analysis")
	r.decode(t, &start)
	// Nothing executes this run here; cancel it so later tests' workers (claims span the
	// whole database) never lease its tasks.
	t.Cleanup(func() {
		_, _ = l.pool.Exec(context.Background(), `UPDATE design_runs SET state = 'CANCELLED', reserved_budget_usd = 0 WHERE id = $1`, start.Run.ID)
	})
	if len(start.Tasks) != len(bi.ModeAgents[bi.ModeQuickScan]) || start.Run.Constraints["analysis_mode"] != "quick_scan" {
		t.Fatalf("quick scan analysis must run only its specialists: %d tasks, %+v", len(start.Tasks), start.Run.Constraints)
	}

	var cmp bi.WebsiteComparison
	l.call(http.MethodPost, "/v1/websites/compare", lens, map[string]any{"website_ids": []string{a.ID, b.ID}}, http.StatusOK, &cmp)
	if len(cmp.Sites) != 2 || len(cmp.Capabilities) != len(bi.CapabilityTaxonomy) || cmp.Sites[1].AnalysisMode != bi.ModeCompareWebsites {
		t.Fatalf("comparison: %+v", cmp)
	}
	shared := strings.Join(cmp.Shared, ",")
	if !strings.Contains(shared, "Shopping Cart") || strings.Contains(shared, "Checkout") || len(cmp.Pairs) != 1 ||
		cmp.Pairs[0].CapabilitySimilarity <= 0 || cmp.Pairs[0].CapabilitySimilarity >= 1 {
		t.Fatalf("shared / pairs: %v %+v", cmp.Shared, cmp.Pairs)
	}
	if u := strings.Join(cmp.Unique[a.ID], ","); !strings.Contains(u, "Checkout & Payments") || !strings.Contains(u, "Product / Content Search") {
		t.Fatalf("unique to shop-a: %v", cmp.Unique)
	}
	if u := strings.Join(cmp.Unique[b.ID], ","); u != "Plans & Pricing" {
		t.Fatalf("unique to shop-b: %v", cmp.Unique)
	}
	l.call(http.MethodPost, "/v1/websites/compare", lens, map[string]any{"website_ids": []string{a.ID}}, http.StatusBadRequest, nil)
	l.call(http.MethodPost, "/v1/websites/compare", lens, map[string]any{"website_ids": []string{a.ID, a.ID}}, http.StatusBadRequest, nil)
	// Another member of the tenant cannot compare someone else's investigations.
	l.call(http.MethodPost, "/v1/websites/compare", l.engTok, map[string]any{"website_ids": []string{a.ID, b.ID}}, http.StatusNotFound, nil)
}
