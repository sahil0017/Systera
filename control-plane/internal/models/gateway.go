// Package models is the provider-neutral Model Gateway. Agents never call providers
// directly: every completion goes through Gateway, which enforces the caller's token and
// cost budget before the call, prices actual usage after it, and classifies errors as
// retryable or not.
package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
)

var (
	ErrBudgetExceeded = errors.New("model call would exceed the remaining budget")
	ErrUnpricedModel  = errors.New("model has no configured pricing")
)

// Message is one chat turn.
type Message struct {
	Role    string `json:"role"` // system | user | assistant
	Content string `json:"content"`
}

// Request is a provider-neutral chat completion request.
type Request struct {
	Model       string
	Messages    []Message
	MaxTokens   int
	Temperature *float64
	// JSON asks the provider for a JSON object response when it supports it.
	JSON bool
}

// Usage is token consumption reported by the provider.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Response is a provider-neutral completion.
type Response struct {
	Model        string  `json:"model"`
	Content      string  `json:"content"`
	FinishReason string  `json:"finish_reason"`
	Usage        Usage   `json:"usage"`
	CostUSD      float64 `json:"cost_usd"`
	// UsageEstimated is true when the provider returned no usage and tokens were estimated.
	UsageEstimated bool `json:"usage_estimated"`
}

// Provider is implemented by model backends (OpenAI-compatible HTTP, fakes in tests).
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// ProviderError carries the provider's status so the gateway can classify retryability.
type ProviderError struct {
	StatusCode int
	Message    string
	Retryable  bool
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("model provider error (status %d): %s", e.StatusCode, e.Message)
}

// IsRetryable reports whether err is transient (rate limits, 5xx, timeouts, network).
func IsRetryable(err error) bool {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	if errors.Is(err, ErrBudgetExceeded) || errors.Is(err, ErrUnpricedModel) {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, context.Canceled)
}

// Price is USD per million tokens.
type Price struct {
	InputPerMTok  float64 `json:"input_per_mtok"`
	OutputPerMTok float64 `json:"output_per_mtok"`
}

// Pricing maps model name -> price. Local models should be listed explicitly at 0 so an
// unpriced (and therefore un-budgetable) model is never used by accident.
type Pricing map[string]Price

// ParsePricing decodes SYSTERA_MODEL_PRICING JSON.
func ParsePricing(raw string) (Pricing, error) {
	p := Pricing{}
	if strings.TrimSpace(raw) == "" {
		return p, nil
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("parse model pricing: %w", err)
	}
	for m, pr := range p {
		if pr.InputPerMTok < 0 || pr.OutputPerMTok < 0 {
			return nil, fmt.Errorf("model pricing for %q must be non-negative", m)
		}
	}
	return p, nil
}

func (p Pricing) cost(model string, u Usage) (float64, error) {
	pr, ok := p[model]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrUnpricedModel, model)
	}
	return (float64(u.PromptTokens)*pr.InputPerMTok + float64(u.CompletionTokens)*pr.OutputPerMTok) / 1e6, nil
}

// Budget bounds one call.
type Budget struct {
	MaxTokens  int     // completion token ceiling
	MaxCostUSD float64 // worst-case cost ceiling for this call
}

// Gateway fronts a Provider with pricing and budget enforcement.
type Gateway struct {
	provider     Provider
	pricing      Pricing
	defaultModel string
}

func NewGateway(p Provider, pricing Pricing, defaultModel string) *Gateway {
	return &Gateway{provider: p, pricing: pricing, defaultModel: defaultModel}
}

// DefaultModel is used when an agent definition does not pin a model.
func (g *Gateway) DefaultModel() string { return g.defaultModel }

// EstimateTokens is a conservative prompt-size estimate (~4 chars/token, plus framing).
func EstimateTokens(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += 4 + (len(m.Content)+3)/4
	}
	return n
}

// Complete runs one completion within budget. Before calling, max_tokens is capped so
// that the worst case (estimated prompt + max completion) fits MaxCostUSD; if not even a
// minimal completion fits, ErrBudgetExceeded is returned without calling the provider.
// After the call, cost is computed from reported (or, if absent, estimated) usage.
func (g *Gateway) Complete(ctx context.Context, req Request, b Budget) (Response, error) {
	if req.Model == "" {
		req.Model = g.defaultModel
	}
	pr, ok := g.pricing[req.Model]
	if !ok {
		return Response{}, fmt.Errorf("%w: %q (add it to SYSTERA_MODEL_PRICING; use 0 for local models)", ErrUnpricedModel, req.Model)
	}
	maxTokens := req.MaxTokens
	if b.MaxTokens > 0 && (maxTokens == 0 || b.MaxTokens < maxTokens) {
		maxTokens = b.MaxTokens
	}
	if maxTokens <= 0 {
		return Response{}, fmt.Errorf("%w: no completion tokens left", ErrBudgetExceeded)
	}
	promptEst := EstimateTokens(req.Messages)
	if pr.OutputPerMTok > 0 || pr.InputPerMTok > 0 {
		promptCost := float64(promptEst) * pr.InputPerMTok / 1e6
		remaining := b.MaxCostUSD - promptCost
		if pr.OutputPerMTok > 0 {
			affordable := int(math.Floor(remaining * 1e6 / pr.OutputPerMTok))
			maxTokens = min(maxTokens, affordable)
		} else if remaining < 0 {
			maxTokens = 0
		}
		const minUsefulTokens = 16
		if maxTokens < minUsefulTokens {
			return Response{}, fmt.Errorf("%w: $%.6f left, prompt ~%d tokens", ErrBudgetExceeded, b.MaxCostUSD, promptEst)
		}
	}
	req.MaxTokens = maxTokens

	resp, err := g.provider.Complete(ctx, req)
	if err != nil {
		return Response{}, err
	}
	if resp.Model == "" {
		resp.Model = req.Model
	}
	if resp.Usage.PromptTokens == 0 && resp.Usage.CompletionTokens == 0 {
		resp.Usage = Usage{PromptTokens: promptEst, CompletionTokens: EstimateTokens([]Message{{Content: resp.Content}})}
		resp.UsageEstimated = true
	}
	// Price by the requested model: providers may report a versioned alias.
	if resp.CostUSD, err = g.pricing.cost(req.Model, resp.Usage); err != nil {
		return Response{}, err
	}
	return resp, nil
}

// Meter accumulates usage across the calls of one task attempt.
type Meter struct {
	mu               sync.Mutex
	Calls            int
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
}

func (m *Meter) Add(r Response) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls++
	m.PromptTokens += r.Usage.PromptTokens
	m.CompletionTokens += r.Usage.CompletionTokens
	m.CostUSD += r.CostUSD
}

func (m *Meter) Snapshot() (calls, prompt, completion int, cost float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Calls, m.PromptTokens, m.CompletionTokens, m.CostUSD
}
