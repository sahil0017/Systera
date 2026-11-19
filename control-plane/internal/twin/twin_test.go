package twin

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/systera/control-plane/internal/graph"
)

type gb struct {
	g    graph.Graph
	next int
}

func newGB() *gb { return &gb{g: graph.Graph{Version: graph.Version{Number: 3, GraphHash: "hash-3"}}} }

func (b *gb) node(key, typ, name string, props map[string]any) string {
	id := key // readable ids keep failures legible
	if props == nil {
		props = map[string]any{}
	}
	b.g.Nodes = append(b.g.Nodes, graph.Node{ID: id, Key: key, Type: typ, Name: name, Properties: props, LifecycleState: "ACTIVE", EvidenceState: "PROPOSED"})
	return id
}

func (b *gb) edge(from, rel, to string, sync string, constraints map[string]any) string {
	b.next++
	id := "e" + string(rune('a'+b.next/26)) + string(rune('a'+b.next%26))
	if constraints == nil {
		constraints = map[string]any{}
	}
	b.g.Edges = append(b.g.Edges, graph.Edge{ID: id, SourceNodeID: from, TargetNodeID: to, RelationType: rel, SyncMode: sync,
		Protocol: "HTTPS", Constraints: constraints, Properties: map[string]any{}})
	return id
}

func build(t *testing.T, g graph.Graph, a Assumptions) (Twin, Report) {
	t.Helper()
	na, hash, err := NormalizeAssumptions(a, g)
	if err != nil {
		t.Fatalf("assumptions: %v", err)
	}
	tw := Build("m1", g, na, hash)
	return tw, Validate(tw, g)
}

func check(t *testing.T, r Report, id string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no check %s", id)
	return Check{}
}

// A small, well-specified URL shortener.
func healthy() graph.Graph {
	b := newGB()
	user := b.node("actor:user", "actor", "User", nil)
	api := b.node("api:redirect", "api", "GET /{code}", map[string]any{"version": "v1", "schema_ref": "openapi/redirect.yaml"})
	svc := b.node("svc:redirect", "service", "Redirect Service", map[string]any{"replicas": json.Number("3"), "capacity_rps": 2000.0})
	db := b.node("db:links", "database", "Links DB", map[string]any{"consistency": "strong", "retention": "forever"})
	replica := b.node("db:links-replica", "database", "Links DB replica", map[string]any{"consistency": "eventual", "retention": "forever"})
	cache := b.node("cache:links", "cache", "Links cache", nil)
	topic := b.node("topic:clicks", "topic", "clicks", map[string]any{"version": "1", "schema_ref": "asyncapi/clicks.yaml"})
	analytics := b.node("svc:analytics", "service", "Analytics", map[string]any{"replicas": 2.0, "capacity_rps": 5000.0})
	slo := b.node("slo:redirect", "slo", "Redirect SLO", map[string]any{"availability_pct": 99.9, "latency_p99_ms": 300.0, "rto_minutes": 30.0, "rpo_minutes": 5.0})
	region := b.node("region:eu", "region", "eu-west-1", nil)
	region2 := b.node("region:us", "region", "us-east-1", nil)
	dom := b.node("domain:links", "domain", "Links", nil)

	b.edge(user, "calls", api, "SYNC", map[string]any{"timeout_ms": 250.0})
	b.edge(svc, "owns", api, "", nil)
	b.edge(svc, "reads", cache, "SYNC", nil)
	b.edge(svc, "writes", cache, "SYNC", nil)
	b.edge(svc, "reads", db, "SYNC", nil)
	b.edge(svc, "writes", db, "SYNC", nil)
	b.edge(db, "replicates", replica, "", nil)
	b.edge(svc, "publishes", topic, "ASYNC", nil)
	b.edge(analytics, "subscribes", topic, "ASYNC", nil)
	b.edge(analytics, "reads", replica, "SYNC", nil)
	b.edge(slo, "depends_on", api, "", nil)
	b.edge(slo, "depends_on", db, "", nil)
	for _, n := range []string{svc, analytics, db, cache, topic} {
		b.edge(n, "deploys_to", region, "", nil)
	}
	b.edge(replica, "deploys_to", region2, "", nil)
	b.edge(dom, "owns", svc, "", nil)
	b.edge(dom, "owns", analytics, "", nil)
	b.edge(dom, "owns", db, "", nil)
	b.edge(dom, "owns", replica, "", nil)
	b.edge(dom, "owns", cache, "", nil)
	return b.g
}

func TestHealthyDesignHasNoErrors(t *testing.T) {
	tw, r := build(t, healthy(), Assumptions{Workload: Workload{PeakRPS: 1500}})
	if tw.Basis != Basis || tw.Disclaimer == "" || r.Basis != Basis || r.Disclaimer == "" {
		t.Fatal("twin and report must be labelled as design intent")
	}
	if r.Errors != 0 {
		for _, c := range r.Checks {
			for _, f := range c.Findings {
				if f.Severity == SevError {
					t.Errorf("%s: %s", c.ID, f.Message)
				}
			}
		}
		t.Fatalf("healthy design reported %d errors", r.Errors)
	}
	if len(r.Dimensions) != 6 {
		t.Fatalf("six dimensions expected, got %d", len(r.Dimensions))
	}
	if len(tw.Behaviour.Paths) != 1 || tw.Behaviour.Paths[0].EndsAt != "DATA" || len(tw.Behaviour.Paths[0].Nodes) != 3 {
		t.Fatalf("one path user → api → service ending at data: %+v", tw.Behaviour.Paths)
	}
	if tw.Quality.CoveragePct != 100 {
		t.Fatalf("redirect path is covered by the SLO: %+v", tw.Quality)
	}
	svc := unit(tw, "svc:redirect")
	if svc.Replicas != 3 || svc.ReplicasSource != SourceDeclared || len(svc.Regions) != 1 {
		t.Fatalf("json.Number replicas must be read as declared: %+v", svc)
	}
	for _, id := range []string{"STRUCT_ORPHANS", "STRUCT_DEPENDENCY_CYCLES", "STRUCT_OWNERSHIP", "IFACE_PROVIDER", "IFACE_CHANNEL_ENDS",
		"IFACE_CONTRACTS", "DATA_SINGLE_WRITER", "DATA_CACHE_BACKING", "RUNTIME_REDUNDANCY", "RUNTIME_REGIONS", "RUNTIME_DATA_REDUNDANCY",
		"RUNTIME_CAPACITY", "QUAL_SLOS_DEFINED", "QUAL_PATH_COVERAGE", "QUAL_LATENCY_BUDGET"} {
		if c := check(t, r, id); c.Status != StatusPass {
			t.Errorf("%s: %s %+v", id, c.Status, c.Findings)
		}
	}
	// A 250 ms timeout against a 300 ms p99: the budget check runs and passes.
	if c := check(t, r, "QUAL_LATENCY_BUDGET"); c.Status == StatusNA {
		t.Fatalf("latency budget should be compared: %+v", c)
	}
}

func unit(tw Twin, key string) RuntimeUnit {
	for _, u := range tw.Runtime.Units {
		if u.Key == key {
			return u
		}
	}
	return RuntimeUnit{}
}

func TestFlawedDesignIsInvalidWithSpecificFindings(t *testing.T) {
	b := newGB()
	user := b.node("actor:user", "actor", "User", nil)
	gw := b.node("svc:gateway", "service", "Gateway", nil)
	orders := b.node("svc:orders", "service", "Orders", map[string]any{"capacity_rps": 100.0})
	inv := b.node("svc:inventory", "service", "Inventory", nil)
	pay := b.node("ext:psp", "external_dependency", "Payment Provider", nil)
	db := b.node("db:shared", "database", "Shared DB", nil)
	b.node("svc:billing", "service", "Billing", map[string]any{"critical": true}) // orphan
	topic := b.node("topic:orders", "topic", "orders", nil)                       // nobody subscribes
	slo := b.node("slo:checkout", "slo", "Checkout SLO", map[string]any{"availability_pct": 99.95, "latency_p99_ms": 500.0})

	b.edge(user, "calls", gw, "SYNC", map[string]any{"timeout_ms": 2000.0})
	b.edge(gw, "calls", orders, "SYNC", map[string]any{"timeout_ms": 1000.0, "retries": 3.0})
	b.edge(orders, "calls", inv, "SYNC", map[string]any{"timeout_ms": 800.0, "retries": 3.0, "backoff": "exponential"})
	b.edge(inv, "calls", orders, "SYNC", map[string]any{"timeout_ms": 500.0}) // sync cycle
	b.edge(orders, "calls", pay, "SYNC", map[string]any{"retries": 2.0})      // retries, no timeout, external
	b.edge(orders, "writes", db, "SYNC", nil)
	b.edge(inv, "writes", db, "SYNC", nil)
	b.edge(orders, "publishes", topic, "ASYNC", nil)
	b.edge(slo, "depends_on", gw, "", nil)

	tw, r := build(t, b.g, Assumptions{Workload: Workload{PeakRPS: 500}})
	if r.Status != ReportInvalid || r.Errors == 0 {
		t.Fatalf("flawed design must be INVALID: %s errors=%d", r.Status, r.Errors)
	}
	expect := map[string]string{
		"STRUCT_ORPHANS":            StatusFail, // critical orphan
		"STRUCT_DEPENDENCY_CYCLES":  StatusFail, // orders ⇄ inventory, all sync
		"IFACE_CHANNEL_ENDS":        StatusWarn,
		"DATA_SINGLE_WRITER":        StatusWarn,
		"RUNTIME_REDUNDANCY":        StatusFail, // single replica under 99.95 %
		"RUNTIME_REGIONS":           StatusWarn,
		"RUNTIME_CAPACITY":          StatusFail, // 1 × 100 rps < 500
		"BEHAV_TIMEOUTS":            StatusFail, // external call without timeout
		"BEHAV_RETRY_POLICY":        StatusFail, // retries without timeout
		"BEHAV_RETRY_AMPLIFICATION": StatusFail, // 4 × 4 = 16
		"BEHAV_TIMEOUT_ORDER":       StatusWarn, // 800 × 4 ≥ 1000
		"QUAL_LATENCY_BUDGET":       StatusWarn,
	}
	for id, want := range expect {
		if c := check(t, r, id); c.Status != want {
			t.Errorf("%s: got %s want %s (%+v)", id, c.Status, want, c.Findings)
		}
	}
	red := check(t, r, "RUNTIME_REDUNDANCY")
	if !red.Findings[0].ReliesOnDefaults {
		t.Fatal("single replica came from the builder default and must say so")
	}
	if len(tw.Defaults) == 0 {
		t.Fatal("defaults applied must be listed")
	}
	cyc := check(t, r, "STRUCT_DEPENDENCY_CYCLES").Findings[0]
	if !strings.Contains(cyc.Message, "Synchronous call cycle") || len(cyc.Subjects) != 2 {
		t.Fatalf("cycle: %+v", cyc)
	}
	var cycleEnd bool
	for _, p := range tw.Behaviour.Paths {
		cycleEnd = cycleEnd || p.EndsAt == "CYCLE"
	}
	if !cycleEnd {
		t.Fatalf("path walk must stop at the cycle: %+v", tw.Behaviour.Paths)
	}
}

func TestBuildIsDeterministicAndAssumptionsCanonical(t *testing.T) {
	g := healthy()
	a1 := Assumptions{Environment: Environment{Regions: []string{"us-east-1", " eu-west-1", "us-east-1"}}, Workload: Workload{PeakRPS: 10}}
	a2 := Assumptions{Environment: Environment{Regions: []string{"eu-west-1", "us-east-1"}}, Workload: Workload{PeakRPS: 10}}
	n1, h1, err1 := NormalizeAssumptions(a1, g)
	n2, h2, err2 := NormalizeAssumptions(a2, g)
	if err1 != nil || err2 != nil || h1 != h2 || !reflect.DeepEqual(n1, n2) {
		t.Fatalf("equivalent assumptions must hash equally: %s %s", h1, h2)
	}
	// Shuffle graph order: the twin must not change.
	g2 := g
	g2.Nodes = append([]graph.Node{}, g.Nodes...)
	g2.Edges = append([]graph.Edge{}, g.Edges...)
	for i, j := 0, len(g2.Nodes)-1; i < j; i, j = i+1, j-1 {
		g2.Nodes[i], g2.Nodes[j] = g2.Nodes[j], g2.Nodes[i]
	}
	for i, j := 0, len(g2.Edges)-1; i < j; i, j = i+1, j-1 {
		g2.Edges[i], g2.Edges[j] = g2.Edges[j], g2.Edges[i]
	}
	j1, _ := json.Marshal([]any{Build("m", g, n1, h1), Validate(Build("m", g, n1, h1), g)})
	j2, _ := json.Marshal([]any{Build("m", g2, n1, h1), Validate(Build("m", g2, n1, h1), g2)})
	if string(j1) != string(j2) {
		t.Fatal("twin must be independent of graph row order")
	}
	_, h3, _ := NormalizeAssumptions(Assumptions{Workload: Workload{PeakRPS: 11}}, g)
	if h3 == h1 {
		t.Fatal("different assumptions must hash differently")
	}
}

func TestAssumptionValidation(t *testing.T) {
	g := healthy()
	bad := -0.5
	cases := []Assumptions{
		{Workload: Workload{PeakRPS: -1}},
		{Workload: Workload{ReadRatio: &bad}},
		{Environment: Environment{DefaultReplicas: 5000}},
		{Failures: []FailureAssumption{{Kind: "METEOR"}}},
		{Failures: []FailureAssumption{{Kind: "NODE_LOSS"}}},
		{Failures: []FailureAssumption{{Kind: "DEPENDENCY_OUTAGE", TargetNodeID: "not-a-node"}}},
	}
	for i, a := range cases {
		if _, _, err := NormalizeAssumptions(a, g); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("case %d: want ErrInvalidInput, got %v", i, err)
		}
	}
	ok := Assumptions{Failures: []FailureAssumption{{Kind: "node_loss", TargetNodeID: "svc:redirect"}, {Kind: "REGION_LOSS"}}}
	n, _, err := NormalizeAssumptions(ok, g)
	if err != nil || n.Failures[0].Kind != "NODE_LOSS" {
		t.Fatalf("valid failures: %v %+v", err, n.Failures)
	}
}

func TestDefaultsAreAssumedNotDeclared(t *testing.T) {
	b := newGB()
	user := b.node("actor:u", "actor", "User", nil)
	svc := b.node("svc:a", "service", "A", nil)
	ext := b.node("ext:b", "external_dependency", "B", nil)
	b.edge(user, "calls", svc, "", nil)
	b.edge(svc, "calls", ext, "SYNC", nil)
	tw, r := build(t, b.g, Assumptions{Environment: Environment{DefaultReplicas: 2, DefaultTimeoutMS: 250}})
	if u := unit(tw, "svc:a"); u.Replicas != 2 || u.ReplicasSource != SourceAssumed {
		t.Fatalf("assumed replicas: %+v", u)
	}
	for _, it := range tw.Behaviour.Interactions {
		if it.TimeoutMS != 250 || it.TimeoutSource != SourceAssumed {
			t.Fatalf("assumed timeout: %+v", it)
		}
		if it.From.Key == "actor:u" && it.SyncSource != SourceDefault {
			t.Fatalf("an unstated sync mode is a default: %+v", it)
		}
	}
	if c := check(t, r, "BEHAV_TIMEOUTS"); c.Status != StatusPass {
		t.Fatalf("assumed timeouts satisfy the check: %+v", c)
	}
	if c := check(t, r, "QUAL_SLOS_DEFINED"); c.Status != StatusWarn {
		t.Fatalf("no SLOs must warn: %+v", c)
	}
}
