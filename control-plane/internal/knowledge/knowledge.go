// Package knowledge implements versioned engineering knowledge ingestion, normalization,
// chunking, deduplication, hybrid pgvector+keyword retrieval, provenance tracking,
// conflict recording, and prompt-injection defence (spec §6.1-§6.5).
package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

var (
	ErrNotFound     = errors.New("knowledge document not found")
	ErrInvalidInput = errors.New("invalid knowledge input")
)

// Document statuses (§6.5).
const (
	StatusActive     = "ACTIVE"
	StatusStale      = "STALE"
	StatusSuperseded = "SUPERSEDED"
	StatusConflicted = "CONFLICTED"
)

// Document represents a versioned, provenance-tracked knowledge source (§6.1).
type Document struct {
	ID              string         `json:"id"`
	TenantID        string         `json:"tenant_id,omitempty"`
	CanonicalKey    string         `json:"canonical_key"`
	VersionNumber   int            `json:"version_number"`
	Title           string         `json:"title"`
	SourceURL       string         `json:"source_url"`
	SourceType      string         `json:"source_type"`
	IsPrimarySource bool           `json:"is_primary_source"`
	Licence         string         `json:"licence"`
	AccessPolicy    string         `json:"access_policy"`
	PublishedAt     *time.Time     `json:"published_at,omitempty"`
	Maturity        string         `json:"maturity"`
	ConfidenceBand  string         `json:"confidence_band"`
	Status          string         `json:"status"`
	SupersededBy    string         `json:"superseded_by,omitempty"`
	ContentHash     string         `json:"content_hash"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

// CodeSnippet holds an extracted fenced code block from source content.
type CodeSnippet struct {
	Language string `json:"language,omitempty"`
	Code     string `json:"code"`
}

// Chunk is a normalized, embedded segment of a knowledge document.
type Chunk struct {
	ID           string         `json:"id"`
	DocumentID   string         `json:"document_id"`
	ChunkIndex   int            `json:"chunk_index"`
	Content      string         `json:"content"`
	CodeSnippets []CodeSnippet  `json:"code_snippets,omitempty"`
	TokenCount   int            `json:"token_count"`
	ContentHash  string         `json:"content_hash"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// IngestInput describes a source document to extract, normalize, chunk, dedupe, and embed.
type IngestInput struct {
	CanonicalKey    string         `json:"canonical_key"`
	Title           string         `json:"title"`
	SourceURL       string         `json:"source_url"`
	SourceType      string         `json:"source_type"` // PRIMARY_DOC|RFC|RELEASE_NOTES|ENGINEERING_BLOG|PAPER|COMMUNITY|INTERNAL
	IsPrimarySource bool           `json:"is_primary_source"`
	Licence         string         `json:"licence"`
	AccessPolicy    string         `json:"access_policy"` // PUBLIC|TENANT|RESTRICTED
	PublishedAt     *time.Time     `json:"published_at,omitempty"`
	Maturity        string         `json:"maturity"`        // EXPERIMENTAL|EMERGING|MAINSTREAM|LEGACY|DEPRECATED
	ConfidenceBand  string         `json:"confidence_band"` // HIGH|MEDIUM|LOW
	Collector       string         `json:"collector,omitempty"`
	TrustScore      float64        `json:"trust_score,omitempty"`
	RawContent      string         `json:"raw_content"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

// SearchQuery configures hybrid retrieval (§6.3).
type SearchQuery struct {
	Query        string   `json:"query"`
	SourceTypes  []string `json:"source_types,omitempty"`
	Maturities   []string `json:"maturities,omitempty"`
	IncludeStale bool     `json:"include_stale,omitempty"`
	PrimaryOnly  bool     `json:"primary_only,omitempty"`
	ProjectID    string   `json:"project_id,omitempty"`
	GraphNodeIDs []string `json:"graph_node_ids,omitempty"`
	// KeyPrefix restricts results to documents whose canonical_key starts with it (e.g. "pulse:").
	KeyPrefix string `json:"key_prefix,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

// SearchResult is a ranked, provenance-bearing retrieval result framed as untrusted data.
type SearchResult struct {
	DocumentID         string     `json:"document_id"`
	ChunkID            string     `json:"chunk_id"`
	ProvenanceRef      string     `json:"provenance_ref"`
	Title              string     `json:"title"`
	SourceURL          string     `json:"source_url"`
	SourceType         string     `json:"source_type"`
	IsPrimarySource    bool       `json:"is_primary_source"`
	Licence            string     `json:"licence"`
	PublishedAt        *time.Time `json:"published_at,omitempty"`
	Maturity           string     `json:"maturity"`
	ConfidenceBand     string     `json:"confidence_band"`
	Status             string     `json:"status"`
	ConflictsWith      []string   `json:"conflicts_with,omitempty"`
	ConflictNote       string     `json:"conflict_note,omitempty"`
	Score              float64    `json:"score"`
	KeywordScore       float64    `json:"keyword_score"`
	VectorScore        float64    `json:"vector_score"`
	InjectionSuspected bool       `json:"injection_suspected"`
	Untrusted          bool       `json:"untrusted"`
	Content            string     `json:"content"`
}

var (
	codeBlockRe  = regexp.MustCompile("(?s)```([a-zA-Z0-9_+-]*)[ \t]*\n(.*?)```")
	multiSpaceRe = regexp.MustCompile(`[ \t]+`)
	multiNLRe    = regexp.MustCompile(`\n{3,}`)

	injectionPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)ignore\s+(all\s+)?(previous|prior|above)\s+instructions`),
		regexp.MustCompile(`(?i)disregard\s+(the\s+)?system\s+prompt`),
		regexp.MustCompile(`(?i)you\s+are\s+now\s+(in\s+)?developer\s+mode`),
		regexp.MustCompile(`(?i)<\|im_start\|>|<\|system\|>|\[INST\]`),
		regexp.MustCompile(`(?i)override\s+(tool|security|permission)\s+policy`),
	}
)

// ExtractAndNormalize extracts fenced code snippets, normalizes whitespace, and returns
// normalized text, extracted code snippets, and the SHA-256 content hash for deduplication (§6.2).
func ExtractAndNormalize(raw string) (normalized string, snippets []CodeSnippet, contentHash string) {
	matches := codeBlockRe.FindAllStringSubmatch(raw, -1)
	for _, m := range matches {
		snippets = append(snippets, CodeSnippet{
			Language: strings.TrimSpace(m[1]),
			Code:     strings.TrimSpace(m[2]),
		})
	}
	text := strings.ReplaceAll(raw, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(multiSpaceRe.ReplaceAllString(l, " "), " ")
	}
	normalized = strings.TrimSpace(multiNLRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
	sum := sha256.Sum256([]byte(normalized))
	contentHash = hex.EncodeToString(sum[:])
	return normalized, snippets, contentHash
}

// ChunkText splits normalized text into overlapping chunks sized for embedding & retrieval.
func ChunkText(normalized string, maxChars, overlap int) []string {
	if maxChars <= 0 {
		maxChars = 1200
	}
	if overlap < 0 || overlap >= maxChars {
		overlap = maxChars / 6
	}
	if len(normalized) == 0 {
		return nil
	}
	if len(normalized) <= maxChars {
		return []string{normalized}
	}

	var chunks []string
	start := 0
	for start < len(normalized) {
		end := start + maxChars
		if end >= len(normalized) {
			chunks = append(chunks, strings.TrimSpace(normalized[start:]))
			break
		}
		// Prefer breaking on paragraph or whitespace boundary.
		cut := strings.LastIndex(normalized[start:end], "\n\n")
		if cut < maxChars/2 {
			cut = strings.LastIndexByte(normalized[start:end], ' ')
		}
		if cut > 0 {
			end = start + cut
		}
		chunk := strings.TrimSpace(normalized[start:end])
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		nextStart := end - overlap
		if nextStart <= start {
			nextStart = end
		}
		start = nextStart
	}
	return chunks
}

// DetectPromptInjection scans untrusted external content for common instruction-override patterns (§6.5).
func DetectPromptInjection(text string) bool {
	for _, re := range injectionPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// FrameUntrustedContent wraps retrieved external text so prompts treat it strictly as data (§6.5).
func FrameUntrustedContent(provenanceRef, content string, injectionSuspected bool) string {
	flag := ""
	if injectionSuspected {
		flag = " warning=\"prompt_injection_pattern_detected\""
	}
	return fmt.Sprintf("<untrusted_knowledge source=%q%s>\n%s\n</untrusted_knowledge>", provenanceRef, flag, content)
}

// ScoreFactors holds individual retrieval signals for hybrid ranking (§6.3, §6.5).
type ScoreFactors struct {
	VectorSim       float64
	KeywordRank     float64
	PublishedAt     *time.Time
	Now             time.Time
	IsPrimarySource bool
	ConfidenceBand  string
	Status          string
	GraphLinked     bool
}

// ComputeHybridScore combines vector similarity, keyword relevance, recency decay,
// primary-source preference, confidence, and graph/project links into one score.
func ComputeHybridScore(f ScoreFactors) float64 {
	vec := clamp01(f.VectorSim)
	kw := clamp01(f.KeywordRank)

	recency := 0.5
	if f.PublishedAt != nil && !f.Now.IsZero() {
		ageDays := f.Now.Sub(*f.PublishedAt).Hours() / 24.0
		if ageDays < 0 {
			ageDays = 0
		}
		// Half-life ~365 days
		recency = math.Exp(-ageDays / 525.0)
	}

	primaryBoost := 0.0
	if f.IsPrimarySource {
		primaryBoost = 0.12
	}

	confBoost := 0.0
	switch f.ConfidenceBand {
	case "HIGH":
		confBoost = 0.08
	case "LOW":
		confBoost = -0.08
	}

	graphBoost := 0.0
	if f.GraphLinked {
		graphBoost = 0.10
	}

	stalePenalty := 0.0
	if f.Status == StatusStale || f.Status == StatusConflicted {
		stalePenalty = 0.15
	}

	score := 0.45*vec + 0.30*kw + 0.15*recency + primaryBoost + confBoost + graphBoost - stalePenalty
	if score < 0 {
		return 0
	}
	return score
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func estimateTokens(s string) int {
	n := len(strings.Fields(s))
	return int(math.Ceil(float64(n) * 1.3))
}
