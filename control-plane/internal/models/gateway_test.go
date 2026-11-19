package models

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGatewayCapsTokensToBudgetAndPricesUsage(t *testing.T) {
	var seen Request
	p := FuncProvider(func(_ context.Context, r Request) (Response, error) {
		seen = r
		return Response{Content: "ok", Usage: Usage{PromptTokens: 1000, CompletionTokens: 500}}, nil
	})
	g := NewGateway(p, Pricing{"m": {InputPerMTok: 1, OutputPerMTok: 10}}, "m")
	// prompt est is tiny; $0.001 buys 100 output tokens at $10/MTok.
	resp, err := g.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 4096}, Budget{MaxTokens: 2000, MaxCostUSD: 0.001})
	if err != nil {
		t.Fatal(err)
	}
	if seen.Model != "m" || seen.MaxTokens >= 100 || seen.MaxTokens < 90 {
		t.Fatalf("expected default model and ~99 max tokens, got %q %d", seen.Model, seen.MaxTokens)
	}
	if want := (1000*1.0 + 500*10.0) / 1e6; resp.CostUSD != want {
		t.Fatalf("cost %v want %v", resp.CostUSD, want)
	}
}

func TestGatewayRefusesWhenBudgetTooSmallOrUnpriced(t *testing.T) {
	called := false
	p := FuncProvider(func(context.Context, Request) (Response, error) { called = true; return Response{}, nil })
	g := NewGateway(p, Pricing{"m": {OutputPerMTok: 10}}, "m")
	if _, err := g.Complete(context.Background(), Request{Messages: []Message{{Content: "x"}}}, Budget{MaxTokens: 100, MaxCostUSD: 0.0000001}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("want ErrBudgetExceeded, got %v", err)
	}
	if _, err := g.Complete(context.Background(), Request{Model: "unknown"}, Budget{MaxTokens: 100, MaxCostUSD: 1}); !errors.Is(err, ErrUnpricedModel) {
		t.Fatalf("want ErrUnpricedModel, got %v", err)
	}
	if called {
		t.Fatal("provider must not be called when the budget check fails")
	}
	if IsRetryable(ErrBudgetExceeded) {
		t.Fatal("budget errors are not retryable")
	}
}

func TestGatewayEstimatesMissingUsage(t *testing.T) {
	g := NewGateway(FuncProvider(func(context.Context, Request) (Response, error) {
		return Response{Content: strings.Repeat("a", 400)}, nil
	}), Pricing{"local": {}}, "local")
	resp, err := g.Complete(context.Background(), Request{Messages: []Message{{Content: "hello"}}}, Budget{MaxTokens: 50})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.UsageEstimated || resp.Usage.CompletionTokens < 100 || resp.CostUSD != 0 {
		t.Fatalf("estimated usage: %+v", resp)
	}
}

func TestOpenAICompatibleAdapter(t *testing.T) {
	var got chatRequest
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		if status != http.StatusOK {
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"model":"llama3.2","choices":[{"message":{"role":"assistant","content":"{\"a\":1}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":5}}`))
	}))
	defer srv.Close()

	p := NewOpenAICompatible(srv.URL+"/v1/", "k", time.Second)
	resp, err := p.Complete(context.Background(), Request{Model: "llama3.2", Messages: []Message{{Role: "user", Content: "q"}}, MaxTokens: 64, JSON: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != `{"a":1}` || resp.Usage.PromptTokens != 12 || got.ResponseFormat["type"] != "json_object" || got.MaxTokens != 64 || got.Stream {
		t.Fatalf("adapter mismatch: resp=%+v req=%+v", resp, got)
	}

	status = http.StatusTooManyRequests
	_, err = p.Complete(context.Background(), Request{Model: "llama3.2"})
	if !IsRetryable(err) || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("429 must be retryable with provider message, got %v", err)
	}
	status = http.StatusBadRequest
	if _, err = p.Complete(context.Background(), Request{Model: "llama3.2"}); IsRetryable(err) {
		t.Fatalf("400 must not be retryable: %v", err)
	}
}

func TestVerifyModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"google/gemma-4-31b-it:free"},{"id":"llama3.2:latest"}]}`))
	}))
	defer srv.Close()
	p := NewOpenAICompatible(srv.URL+"/v1", "", time.Second)
	for _, ok := range []string{"google/gemma-4-31b-it:free", "llama3.2"} {
		if err := p.VerifyModel(context.Background(), ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	if err := p.VerifyModel(context.Background(), "meta-llama/llama-3.3-70b-instruct:free"); !errors.Is(err, ErrModelNotServed) {
		t.Fatalf("missing model: %v", err)
	}
	if err := NewOpenAICompatible(srv.URL+"/nope", "", time.Second).VerifyModel(context.Background(), "x"); err == nil || errors.Is(err, ErrModelNotServed) {
		t.Fatalf("unavailable list must be a soft error: %v", err)
	}
}
