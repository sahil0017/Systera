package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func newTestGateway(t *testing.T, ran *int) *Gateway {
	t.Helper()
	reg := NewRegistry()
	err := reg.Register(Tool{
		Name:        "run.search",
		Description: "search",
		Capability:  "run.read",
		InputSchema: MustSchema(`{"type":"object","required":["query"],"properties":{
			"query":{"type":"string","minLength":1,"maxLength":20},
			"limit":{"type":"integer","minimum":1,"maximum":10},
			"mode":{"type":"string","enum":["fast","deep"]},
			"tags":{"type":"array","items":{"type":"string"},"maxItems":2}}}`),
		OutputSchema: MustSchema(`{"type":"object","required":["hits"],"properties":{"hits":{"type":"integer"}}}`),
		Handler: func(_ context.Context, _ Invocation, args map[string]any) (map[string]any, error) {
			*ran++
			if args["query"] == "badout" {
				return map[string]any{"hits": "many"}, nil
			}
			return map[string]any{"hits": 3}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(Tool{Name: "fs.write", Capability: "fs.write", InputSchema: MustSchema(`{"type":"object"}`),
		Handler: func(context.Context, Invocation, map[string]any) (map[string]any, error) { *ran++; return nil, nil }}); err != nil {
		t.Fatal(err)
	}
	return NewGateway(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestToolGatewayEnforcesPolicyBeforeExecution(t *testing.T) {
	ran := 0
	g := newTestGateway(t, &ran)
	s := g.NewSession(Invocation{TaskID: "t1"}, Policy{
		AllowedTools: []string{"run.search"}, GrantedCapabilities: []string{"run.read"}, MaxCalls: 8,
	})

	if out, err := s.Execute(context.Background(), "run.search", json.RawMessage(`{"query":"cache","limit":5,"mode":"fast"}`)); err != nil || out["hits"] != 3 {
		t.Fatalf("valid call: %v %v", out, err)
	}

	cases := []struct {
		tool, args string
		want       error
		contains   string
	}{
		{"nope.tool", `{}`, ErrUnknownTool, ""},
		{"fs.write", `{}`, ErrToolNotAllowed, ""},
		{"run.search", `{}`, ErrInvalidArguments, `missing required property "query"`},
		{"run.search", `{"query":"x","limit":11}`, ErrInvalidArguments, "<= 10"},
		{"run.search", `{"query":"x","limit":1.5}`, ErrInvalidArguments, "integer"},
		{"run.search", `{"query":"x","mode":"slow"}`, ErrInvalidArguments, "one of"},
		{"run.search", `{"query":"x","extra":1}`, ErrInvalidArguments, "unexpected property"},
		{"run.search", `{"query":"x","tags":["a","b","c"]}`, ErrInvalidArguments, "at most 2"},
		{"run.search", `not json`, ErrInvalidArguments, ""},
	}
	before := ran
	for _, c := range cases {
		_, err := s.Execute(context.Background(), c.tool, json.RawMessage(c.args))
		if !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.contains) {
			t.Errorf("%s %s: got %v, want %v containing %q", c.tool, c.args, err, c.want, c.contains)
		}
	}
	// Unknown/not-allowed calls are free; the 7 schema-invalid calls consumed budget
	// (so a model cannot loop on malformed calls), but no handler executed.
	if ran != before {
		t.Fatalf("handler ran %d times for rejected calls", ran-before)
	}
	if _, err := s.Execute(context.Background(), "run.search", json.RawMessage(`{"query":"x"}`)); !errors.Is(err, ErrToolBudgetExceeded) {
		t.Fatalf("expected budget exhaustion, got %v", err)
	}
}

func TestToolGatewayCapabilityAndOutputSchema(t *testing.T) {
	ran := 0
	g := newTestGateway(t, &ran)
	denied := g.NewSession(Invocation{}, Policy{AllowedTools: []string{"run.search"}, MaxCalls: 5})
	if _, err := denied.Execute(context.Background(), "run.search", json.RawMessage(`{"query":"x"}`)); !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("expected capability denial, got %v", err)
	}
	if len(denied.Available()) != 0 {
		t.Fatal("ungranted tools must not be advertised")
	}
	ok := g.NewSession(Invocation{}, Policy{AllowedTools: []string{"run.search"}, GrantedCapabilities: []string{"run.read"}, MaxCalls: 5})
	if len(ok.Available()) != 1 {
		t.Fatalf("available: %+v", ok.Available())
	}
	if _, err := ok.Execute(context.Background(), "run.search", json.RawMessage(`{"query":"badout"}`)); !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("expected output schema violation, got %v", err)
	}
}

func TestSchemaRejectsUnsupportedKeywords(t *testing.T) {
	if _, err := ParseSchema([]byte(`{"type":"object","pattern":"x"}`)); err == nil {
		t.Fatal("unsupported keyword must be rejected")
	}
	if _, err := ParseSchema([]byte(`{"type":"object","required":["a"]}`)); err == nil {
		t.Fatal("undeclared required property must be rejected")
	}
}
