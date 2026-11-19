// Package memory implements tenant- and project-scoped working, episodic, semantic,
// and project memory with pgvector retrieval and explicit entity links (spec §6.4).
// Whole transcripts are rejected; only distilled, source-referenced memories are stored.
package memory

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const MaxSummaryChars = 4000

var (
	ErrInvalidInput       = errors.New("invalid memory input")
	ErrTranscriptTooLarge = errors.New("memory summary exceeds maximum distilled length (never store full transcripts)")
	ErrNotFound           = errors.New("memory record not found")
)

// MemoryType enumerates the four spec §6.4 memory categories.
type MemoryType string

const (
	TypeWorking  MemoryType = "WORKING"
	TypeEpisodic MemoryType = "EPISODIC"
	TypeSemantic MemoryType = "SEMANTIC"
	TypeProject  MemoryType = "PROJECT"
)

// ValidType reports whether t is a supported memory type.
func ValidType(t MemoryType) bool {
	switch t {
	case TypeWorking, TypeEpisodic, TypeSemantic, TypeProject:
		return true
	default:
		return false
	}
}

// Record is a distilled, tenant- and project-scoped memory entry.
type Record struct {
	ID             string         `json:"id"`
	TenantID       string         `json:"tenant_id"`
	ProjectID      string         `json:"project_id,omitempty"`
	RunID          string         `json:"run_id,omitempty"`
	MemoryType     MemoryType     `json:"memory_type"`
	Summary        string         `json:"summary"`
	Content        map[string]any `json:"content,omitempty"`
	SourceRefs     []string       `json:"source_refs"`
	RelevanceScore float64        `json:"relevance_score"`
	AccessPolicy   string         `json:"access_policy"`
	ExpiresAt      *time.Time     `json:"expires_at,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
}

// RecordInput captures a new distilled memory record.
type RecordInput struct {
	ProjectID      string         `json:"project_id,omitempty"`
	RunID          string         `json:"run_id,omitempty"`
	MemoryType     MemoryType     `json:"memory_type"`
	Summary        string         `json:"summary"`
	Content        map[string]any `json:"content,omitempty"`
	SourceRefs     []string       `json:"source_refs,omitempty"`
	RelevanceScore float64        `json:"relevance_score,omitempty"`
	AccessPolicy   string         `json:"access_policy,omitempty"`
	ExpiresAt      *time.Time     `json:"expires_at,omitempty"`
}

// RetrieveQuery selects relevant, permission-checked memories for a project/run.
type RetrieveQuery struct {
	ProjectID   string       `json:"project_id,omitempty"`
	MemoryTypes []MemoryType `json:"memory_types,omitempty"`
	Query       string       `json:"query,omitempty"`
	Limit       int          `json:"limit,omitempty"`
}

// ValidateRecord enforces §6.4 rules (valid type, non-empty distilled summary, max length).
func ValidateRecord(in *RecordInput) error {
	if !ValidType(in.MemoryType) {
		return fmt.Errorf("%w: memory_type must be WORKING, EPISODIC, SEMANTIC, or PROJECT", ErrInvalidInput)
	}
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		return fmt.Errorf("%w: summary is required", ErrInvalidInput)
	}
	if len(summary) > MaxSummaryChars {
		return fmt.Errorf("%w: got %d chars, max %d", ErrTranscriptTooLarge, len(summary), MaxSummaryChars)
	}
	in.Summary = summary
	if in.AccessPolicy == "" {
		in.AccessPolicy = "PROJECT"
	}
	if in.RelevanceScore <= 0 {
		in.RelevanceScore = 0.5
	}
	if in.SourceRefs == nil {
		in.SourceRefs = []string{}
	}
	return nil
}
