package agent

// Checklist 1.7: knowledge.search through the Tool Gateway, citing ledger evidence.
// Requires SYSTERA_TEST_DATABASE_URL.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/systera/control-plane/internal/evidence"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/knowledge"
	"github.com/systera/control-plane/internal/memory"
	"github.com/systera/control-plane/internal/models"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/runtime"
	"github.com/systera/control-plane/internal/tools"
)

func TestKnowledgeSearchToolCitesLedgerEvidence(t *testing.T) {
	dsn := os.Getenv("SYSTERA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SYSTERA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := postgres.MigrateUp(ctx, pool); err != nil {
		t.Fatal(err)
	}
	sfx := ids.New()[:8]
	var tenant, user, project, run string
	q := func(sql string, dst *string, args ...any) {
		if err := pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	q(`INSERT INTO tenants (slug, name) VALUES ($1, $1) RETURNING id::text`, &tenant, "kt-"+sfx)
	q(`INSERT INTO users (email, display_name, external_sub) VALUES ($1, $1, $1) RETURNING id::text`, &user, "kt-"+sfx+"@example.test")
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_memberships (tenant_id, user_id, role) VALUES ($1, $2, 'OWNER')`, tenant, user); err != nil {
		t.Fatal(err)
	}
	q(`INSERT INTO projects (tenant_id, name, created_by) VALUES ($1, 'k', $2) RETURNING id::text`, &project, tenant, user)
	q(`INSERT INTO design_runs (tenant_id, project_id, objective, created_by) VALUES ($1, $2, 'x', $3) RETURNING id::text`, &run, tenant, project, user)
	gsvc := graph.NewService(pool)
	if _, _, err := gsvc.CreateModel(ctx, tenant, project, graph.Author{Type: "USER", ID: user}, ids.New()); err != nil {
		t.Fatal(err)
	}
	emb := models.NewDeterministicHashEmbedder(models.DefaultEmbeddingDimensions)
	ks := knowledge.NewStore(pool, emb, "")
	doc, _, err := ks.Ingest(ctx, tenant, knowledge.IngestInput{Title: "Idempotency keys", SourceType: "PRIMARY_DOC", IsPrimarySource: true,
		ConfidenceBand: "HIGH", RawContent: "Use idempotency keys quuxwidget so retries are safe."})
	if err != nil {
		t.Fatal(err)
	}

	reg := tools.NewRegistry()
	ev := evidence.NewStore(pool)
	if err := RegisterKnowledgeTools(reg, ks, memory.NewStore(pool, emb, ""), gsvc, ev); err != nil {
		t.Fatal(err)
	}
	gw := tools.NewGateway(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	lease := &runtime.Lease{Task: runtime.Task{TenantID: tenant, AgentType: "hld_architect"}, Run: runtime.RunContext{RunID: run, TenantID: tenant, ProjectID: project}}
	lctx := withLease(ctx, lease)
	args := json.RawMessage(`{"query":"idempotency keys quuxwidget","cite_as_evidence":true}`)
	inv := tools.Invocation{TenantID: tenant, RunID: run, AgentType: "hld_architect"}

	// Policy: not allowlisted, or capability not granted -> refused.
	if _, err := gw.NewSession(inv, tools.Policy{GrantedCapabilities: []string{CapKnowledgeRead}, MaxCalls: 3}).Execute(lctx, "knowledge.search", args); err == nil {
		t.Fatal("tool ran without being allowlisted")
	}
	if _, err := gw.NewSession(inv, tools.Policy{AllowedTools: []string{"knowledge.search"}, MaxCalls: 3}).Execute(lctx, "knowledge.search", args); err == nil {
		t.Fatal("tool ran without the knowledge.read capability")
	}
	sess := gw.NewSession(inv, tools.Policy{AllowedTools: []string{"knowledge.search", "memory.record", "memory.recall"}, GrantedCapabilities: []string{CapKnowledgeRead, CapMemoryRead, CapMemoryWrite}, MaxCalls: 4})
	out, err := sess.Execute(lctx, "knowledge.search", args)
	if err != nil {
		t.Fatal(err)
	}
	results, _ := out["results"].([]map[string]any)
	if len(results) == 0 {
		t.Fatalf("no results: %+v", out)
	}
	r := results[0]
	if r["document_id"] != doc.ID || !strings.HasPrefix(r["content"].(string), "<untrusted_knowledge") || !strings.HasPrefix(r["evidence_ref"].(string), "evidence:") {
		t.Fatalf("result: %+v", r)
	}
	var srcType string
	if err := pool.QueryRow(ctx, `SELECT source_type FROM evidence_records WHERE id = $1`, r["evidence_id"]).Scan(&srcType); err != nil || srcType != "RETRIEVED_KNOWLEDGE" {
		t.Fatalf("ledger evidence: %q %v", srcType, err)
	}
	if _, err := sess.Execute(lctx, "knowledge.search", json.RawMessage(`{"limit":3}`)); err == nil {
		t.Fatal("schema-invalid arguments accepted")
	}
	// Memory tools are scoped to the lease's project and run.
	if _, err := sess.Execute(lctx, "memory.record", json.RawMessage(`{"memory_type":"SEMANTIC","summary":"Team prefers Postgres."}`)); err != nil {
		t.Fatal(err)
	}
	rec, err := sess.Execute(lctx, "memory.recall", json.RawMessage(`{"query":"postgres"}`))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(rec); !strings.Contains(string(b), "Team prefers Postgres.") || !strings.Contains(string(b), project) {
		t.Fatalf("recall: %s", b)
	}
	// MaxCalls is enforced (4 used).
	if _, err := sess.Execute(lctx, "memory.recall", json.RawMessage(`{}`)); err == nil {
		t.Fatal("call budget not enforced")
	}
}
