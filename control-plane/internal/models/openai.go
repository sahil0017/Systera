package models

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAICompatible talks to any /v1/chat/completions endpoint: Ollama
// (http://localhost:11434/v1), vLLM, LM Studio, OpenAI, and other compatible gateways.
type OpenAICompatible struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func NewOpenAICompatible(baseURL, apiKey string, timeout time.Duration) *OpenAICompatible {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	return &OpenAICompatible{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		client:  &http.Client{Timeout: timeout},
	}
}

type chatRequest struct {
	Model          string            `json:"model"`
	Messages       []Message         `json:"messages"`
	MaxTokens      int               `json:"max_tokens,omitempty"`
	Temperature    *float64          `json:"temperature,omitempty"`
	ResponseFormat map[string]string `json:"response_format,omitempty"`
	Stream         bool              `json:"stream"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (o *OpenAICompatible) Complete(ctx context.Context, req Request) (Response, error) {
	body := chatRequest{Model: req.Model, Messages: req.Messages, MaxTokens: req.MaxTokens, Temperature: req.Temperature}
	if req.JSON {
		body.ResponseFormat = map[string]string{"type": "json_object"}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.client.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("model request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Response{}, &ProviderError{StatusCode: resp.StatusCode, Message: err.Error(), Retryable: true}
	}

	var parsed chatResponse
	_ = json.Unmarshal(data, &parsed)
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(data))
		if parsed.Error != nil && parsed.Error.Message != "" {
			msg = parsed.Error.Message
		}
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return Response{}, &ProviderError{
			StatusCode: resp.StatusCode,
			Message:    msg,
			Retryable:  resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500,
		}
	}
	if len(parsed.Choices) == 0 {
		return Response{}, &ProviderError{StatusCode: resp.StatusCode, Message: "response had no choices", Retryable: true}
	}
	out := Response{
		Model:        parsed.Model,
		Content:      parsed.Choices[0].Message.Content,
		FinishReason: parsed.Choices[0].FinishReason,
	}
	if parsed.Usage != nil {
		out.Usage = Usage{PromptTokens: parsed.Usage.PromptTokens, CompletionTokens: parsed.Usage.CompletionTokens}
	}
	return out, nil
}

// ErrModelNotServed means the endpoint's model list does not include the requested model.
var ErrModelNotServed = errors.New("model is not served by the provider")

// VerifyModel checks GET {base}/models for the model. It returns ErrModelNotServed when
// the list loads and lacks the model, and a plain error when the list is unavailable
// (some endpoints do not implement /models), which callers may treat as a warning.
func (o *OpenAICompatible) VerifyModel(ctx context.Context, model string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/models", nil)
	if err != nil {
		return err
	}
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("list models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("list models: status %d", resp.StatusCode)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&list); err != nil {
		return fmt.Errorf("list models: %w", err)
	}
	for _, m := range list.Data {
		if m.ID == model || strings.TrimSuffix(m.ID, ":latest") == model {
			return nil
		}
	}
	return fmt.Errorf("%w: %q (%d models listed at %s/models)", ErrModelNotServed, model, len(list.Data), o.baseURL)
}

// FuncProvider adapts a function into a Provider (tests, local stubs).
type FuncProvider func(ctx context.Context, req Request) (Response, error)

func (f FuncProvider) Complete(ctx context.Context, req Request) (Response, error) {
	return f(ctx, req)
}
