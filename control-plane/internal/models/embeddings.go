package models

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// DefaultEmbeddingDimensions matches the pgvector column width in migrations/000012.
const DefaultEmbeddingDimensions = 384

// EmbeddingProvider is the provider-neutral embeddings contract (spec §6.2, App. E).
type EmbeddingProvider interface {
	Embed(ctx context.Context, model string, texts []string) ([][]float32, error)
	Dimensions() int
}

// OpenAICompatibleEmbedder calls any OpenAI-compatible /v1/embeddings endpoint
// (OpenAI, Ollama, Together, DeepInfra, Hugging Face Router, vLLM).
type OpenAICompatibleEmbedder struct {
	baseURL    string
	apiKey     string
	dimensions int
	client     *http.Client
}

func NewOpenAICompatibleEmbedder(baseURL, apiKey string, dimensions int, timeout time.Duration) *OpenAICompatibleEmbedder {
	if dimensions <= 0 {
		dimensions = DefaultEmbeddingDimensions
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &OpenAICompatibleEmbedder{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		dimensions: dimensions,
		client:     &http.Client{Timeout: timeout},
	}
}

func (e *OpenAICompatibleEmbedder) Dimensions() int { return e.dimensions }

func (e *OpenAICompatibleEmbedder) Embed(ctx context.Context, model string, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if e.baseURL == "" {
		return nil, errors.New("embeddings base URL is required")
	}
	payload := map[string]any{
		"model":      model,
		"input":      texts,
		"dimensions": e.dimensions,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("embeddings endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var parsed struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode embeddings response: %w", err)
	}
	out := make([][]float32, len(texts))
	for _, item := range parsed.Data {
		if item.Index >= 0 && item.Index < len(out) {
			out[item.Index] = fitDimensions(item.Embedding, e.dimensions)
		}
	}
	return out, nil
}

// DeterministicHashEmbedder generates L2-normalized feature-hashed embeddings locally
// with zero external dependencies (used as an offline fallback and in unit tests).
type DeterministicHashEmbedder struct {
	dims int
}

func NewDeterministicHashEmbedder(dims int) *DeterministicHashEmbedder {
	if dims <= 0 {
		dims = DefaultEmbeddingDimensions
	}
	return &DeterministicHashEmbedder{dims: dims}
}

func (d *DeterministicHashEmbedder) Dimensions() int { return d.dims }

func (d *DeterministicHashEmbedder) Embed(_ context.Context, _ string, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		vec := make([]float32, d.dims)
		tokens := tokenize(text)
		for _, tok := range tokens {
			h := fnv.New32a()
			_, _ = h.Write([]byte(tok))
			idx := int(h.Sum32()) % d.dims
			vec[idx] += 1.0
		}
		out[i] = normalizeL2(vec)
	}
	return out, nil
}

// FormatPGVector renders a float32 slice as a pgvector literal, e.g. "[0.1,-0.2,0.3]".
func FormatPGVector(vec []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'f', 6, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// CosineSimilarity computes cosine similarity in [-1, 1] between two equal-length vectors.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		normA += fa * fa
		normB += fb * fb
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func normalizeL2(vec []float32) []float32 {
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		return vec
	}
	inv := float32(1.0 / math.Sqrt(sum))
	for i := range vec {
		vec[i] *= inv
	}
	return vec
}

func fitDimensions(vec []float32, dims int) []float32 {
	if len(vec) == dims {
		return vec
	}
	out := make([]float32, dims)
	copy(out, vec)
	return normalizeL2(out)
}
