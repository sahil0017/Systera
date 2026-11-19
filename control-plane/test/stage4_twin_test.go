package test

// Stage 4 design-time Architecture Digital Twin (§23.4, §26) integration test against a live
// PostgreSQL (SYSTERA_TEST_DATABASE_URL).

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/systera/control-plane/internal/auth"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/platform/httpserver"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/projects"
	"github.com/systera/control-plane/internal/runs"
	"github.com/systera/control-plane/internal/twin"
)

func TestDigitalTwinBuildEndToEnd(t *testing.T) {
	l := newLensEnv(t)
	logger := slog.New(slog.NewTextHandler(debugOut(), nil))
	gsvc := graph.NewService(l.pool)
	issuer, _ := auth.NewTokenIssuer(testPrivateKey, "", testIssuer, testAudience)
	srv := httpserver.New(httpserver.Options{
		Logger: logger, Projects: projects.NewPostgresRepository(l.pool), Runs: runs.NewPostgresRepository(l.pool),
		Events: outbox.NewPostgresStore(l.pool, logger), Graph: gsvc, Impact: gsvc, TokenIssuer: issuer,
		Twins:         twin.NewStore(l.pool, gsvc),
		Authenticator: auth.NewAuthenticator(newVerifier(t), auth.NewPostgresMemberships(l.pool)),
		Readiness:     []httpserver.ReadinessCheck{{Name: "postgres", Check: postgres.PingCheck(l.pool)}},
	})
	l.c = client{t: t, handler: srv.Handler()}

	var proj struct {
		ID string `json:"id"`
	}
	l.call(http.MethodPost, "/v1/projects", l.token, map[string]any{"name": "Twin project"}, http.StatusCreated, &proj)
	var created struct {
		Model graph.Model `json:"model"`
	}
	l.call(http.MethodPost, "/v1/projects/"+proj.ID+"/architecture", l.token, nil, http.StatusCreated, &created)
	modelID := created.Model.ID

	nUser, nAPI, nSvc, nPSP, nDB, nSLO := ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
	op := func(kind, id string, payload map[string]any) map[string]any {
		return map[string]any{"op": kind, "target_id": id, "payload": payload, "rationale": "seed"}
	}
	l.call(http.MethodPost, "/v1/architecture/"+modelID+"/patches", l.token, map[string]any{
		"base_graph_version": 0, "idempotency_key": "twin-seed",
		"operations": []map[string]any{
			op("add_node", nUser, map[string]any{"key": "actor:shopper", "type": "actor", "name": "Shopper", "evidence_state": "PROPOSED"}),
			op("add_node", nAPI, map[string]any{"key": "api:checkout", "type": "api", "name": "POST /checkout", "evidence_state": "PROPOSED",
				"properties": map[string]any{"version": "v1", "schema_ref": "openapi/checkout.yaml"}}),
			op("add_node", nSvc, map[string]any{"key": "svc:checkout", "type": "service", "name": "Checkout", "evidence_state": "PROPOSED",
				"owner_domain": "commerce", "properties": map[string]any{"capacity_rps": 200}}),
			op("add_node", nPSP, map[string]any{"key": "ext:psp", "type": "external_dependency", "name": "Payment Provider", "evidence_state": "PROPOSED"}),
			op("add_node", nDB, map[string]any{"key": "db:orders", "type": "database", "name": "Orders DB", "evidence_state": "PROPOSED", "owner_domain": "commerce"}),
			op("add_node", nSLO, map[string]any{"key": "slo:checkout", "type": "slo", "name": "Checkout SLO", "evidence_state": "PROPOSED",
				"properties": map[string]any{"availability_pct": 99.9, "latency_p99_ms": 800}}),
			op("add_edge", ids.New(), map[string]any{"source_node_id": nUser, "target_node_id": nAPI, "relation_type": "calls", "protocol": "HTTPS", "sync_mode": "SYNC", "constraints": map[string]any{"timeout_ms": 3000}}),
			op("add_edge", ids.New(), map[string]any{"source_node_id": nSvc, "target_node_id": nAPI, "relation_type": "owns"}),
			op("add_edge", ids.New(), map[string]any{"source_node_id": nSvc, "target_node_id": nPSP, "relation_type": "calls", "protocol": "HTTPS", "sync_mode": "SYNC", "constraints": map[string]any{"retries": 2}}),
			op("add_edge", ids.New(), map[string]any{"source_node_id": nSvc, "target_node_id": nDB, "relation_type": "writes", "protocol": "PostgreSQL"}),
			op("add_edge", ids.New(), map[string]any{"source_node_id": nSLO, "target_node_id": nAPI, "relation_type": "depends_on"}),
		},
	}, http.StatusCreated, nil)

	path := "/v1/architecture/" + modelID + "/twins"
	assumptions := map[string]any{"workload": map[string]any{"peak_rps": 500}, "environment": map[string]any{"regions": []string{"eu-west-1"}},
		"failures": []map[string]any{{"kind": "DEPENDENCY_OUTAGE", "target_node_id": nPSP}}}

	// Build: recorded, labelled as design intent, and the flaws are found.
	var b1 twin.BuildRecord
	res := l.call(http.MethodPost, path, l.token, map[string]any{"assumptions": assumptions}, http.StatusCreated, &b1)
	if res.header.Get("Location") != "/v1/twins/"+b1.ID {
		t.Fatalf("location: %q", res.header.Get("Location"))
	}
	if b1.Basis != twin.Basis || b1.GraphVersion != 1 || b1.Twin == nil || b1.Report == nil || b1.Report.Disclaimer == "" {
		t.Fatalf("build: %+v", b1)
	}
	if b1.Status != twin.ReportInvalid || b1.ErrorCount == 0 {
		t.Fatalf("retries without a timeout to an external provider and 1×200 rps < 500 must be errors: %s %d", b1.Status, b1.ErrorCount)
	}
	statuses := map[string]string{}
	for _, c := range b1.Report.Checks {
		statuses[c.ID] = c.Status
	}
	for id, want := range map[string]string{"BEHAV_TIMEOUTS": twin.StatusFail, "BEHAV_RETRY_POLICY": twin.StatusFail,
		"RUNTIME_CAPACITY": twin.StatusFail, "IFACE_CONTRACTS": twin.StatusPass, "QUAL_PATH_COVERAGE": twin.StatusPass} {
		if statuses[id] != want {
			t.Errorf("%s: got %s want %s", id, statuses[id], want)
		}
	}
	if len(b1.Twin.Behaviour.Paths) != 1 || b1.Twin.Quality.CoveragePct != 100 {
		t.Fatalf("one covered path expected: %+v", b1.Twin.Behaviour.Paths)
	}

	// Same inputs → same build (200, same id); different assumptions → new build.
	var b2 twin.BuildRecord
	l.call(http.MethodPost, path, l.token, map[string]any{"assumptions": assumptions, "version": "1"}, http.StatusOK, &b2)
	if b2.ID != b1.ID || b2.AssumptionsHash != b1.AssumptionsHash {
		t.Fatalf("deterministic build must dedupe: %s vs %s", b2.ID, b1.ID)
	}
	var b3 twin.BuildRecord
	l.call(http.MethodPost, path, l.token, map[string]any{"assumptions": map[string]any{"workload": map[string]any{"peak_rps": 100}}}, http.StatusCreated, &b3)
	if b3.ID == b1.ID {
		t.Fatal("different assumptions must be a different build")
	}

	// Dry run: validated, not recorded.
	var dry twin.BuildRecord
	l.call(http.MethodPost, path, l.token, map[string]any{"assumptions": map[string]any{"workload": map[string]any{"peak_rps": 1}}, "dry_run": true}, http.StatusOK, &dry)
	if dry.ID != "" || dry.Report == nil {
		t.Fatalf("dry run: %+v", dry)
	}
	var list struct {
		Items []twin.BuildRecord `json:"items"`
	}
	l.call(http.MethodGet, path, l.token, nil, http.StatusOK, &list)
	if len(list.Items) != 2 || list.Items[0].ID != b3.ID || list.Items[0].Twin != nil {
		t.Fatalf("list (newest first, no bodies, dry run not recorded): %+v", list.Items)
	}

	var got twin.BuildRecord
	l.call(http.MethodGet, "/v1/twins/"+b1.ID, l.token, nil, http.StatusOK, &got)
	if got.Report == nil || got.Report.Status != b1.Status || len(got.Assumptions.Failures) != 1 {
		t.Fatalf("get: %+v", got)
	}

	// Errors.
	l.call(http.MethodPost, path, l.token, map[string]any{"assumptions": map[string]any{"workload": map[string]any{"peak_rps": -5}}}, http.StatusBadRequest, nil)
	l.call(http.MethodPost, path, l.token, map[string]any{"assumptions": map[string]any{"failures": []map[string]any{{"kind": "NODE_LOSS", "target_node_id": ids.New()}}}}, http.StatusBadRequest, nil)
	l.call(http.MethodPost, path, l.token, map[string]any{"version": "9"}, http.StatusNotFound, nil)
	l.call(http.MethodPost, "/v1/architecture/"+ids.New()+"/twins", l.token, map[string]any{}, http.StatusNotFound, nil)
	l.call(http.MethodGet, "/v1/architecture/"+ids.New()+"/twins", l.token, nil, http.StatusNotFound, nil)
	l.call(http.MethodGet, "/v1/twins/"+ids.New(), l.token, nil, http.StatusNotFound, nil)

	// Tenant isolation: another tenant sees neither the build nor the model's list.
	l.call(http.MethodGet, "/v1/twins/"+b1.ID, l.otherTok, nil, http.StatusNotFound, nil)
	l.call(http.MethodGet, path, l.otherTok, nil, http.StatusNotFound, nil)

	// Lens tokens cannot reach the twin.
	var tk struct {
		AccessToken string `json:"access_token"`
	}
	l.call(http.MethodPost, "/v1/auth/lens-token", l.token, nil, http.StatusCreated, &tk)
	l.call(http.MethodGet, "/v1/twins/"+b1.ID, tk.AccessToken, nil, http.StatusForbidden, nil)

	// One outbox event per recorded build; builds are immutable.
	ctx := context.Background()
	var events int
	if err := l.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_type = $1 AND payload->>'model_id' = $2`, twin.EventBuilt, modelID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 2 {
		t.Fatalf("twin.built events: %d", events)
	}
	err := postgres.WithTenantRoleTx(ctx, l.pool, l.tenantID, postgres.AppRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE digital_twins SET status = 'VALID' WHERE id = $1`, b1.ID)
		return err
	})
	if err == nil {
		t.Fatal("twin builds must be immutable for the app role")
	}
}
