// Package outbox implements the transactional outbox: domain events are written in the
// same database transaction as the state change that caused them, then streamed to
// clients (SSE) and, later, relayed to NATS JetStream.
package outbox

import (
	"context"
	"time"
)

// EventEnvelope defines the canonical domain event envelope (Appendix D of V4 spec).
type EventEnvelope struct {
	// Seq is the global, monotonically increasing outbox sequence. It is the SSE event id
	// and the resume cursor for Last-Event-ID.
	Seq           int64          `json:"seq"`
	EventID       string         `json:"event_id"`
	EventType     string         `json:"event_type"`
	SchemaVersion string         `json:"schema_version"`
	OccurredAt    time.Time      `json:"occurred_at"`
	TenantID      string         `json:"tenant_id"`
	ProjectID     string         `json:"project_id,omitempty"`
	RunID         string         `json:"run_id,omitempty"`
	CorrelationID string         `json:"correlation_id"`
	CausationID   string         `json:"causation_id,omitempty"`
	Payload       map[string]any `json:"payload"`
}

// Reader lists persisted events for a run in sequence order.
type Reader interface {
	// ListByRunAfter returns up to limit events for the run with Seq > afterSeq.
	ListByRunAfter(ctx context.Context, tenantID, runID string, afterSeq int64, limit int) ([]EventEnvelope, error)
}

// Stream is the read side used by live subscribers: persisted events plus a wake-up
// signal when new events for a run may be available.
type Stream interface {
	Reader
	// Subscribe returns a channel that receives a value whenever an event for runID is
	// committed. Signals are coalesced; callers must re-read with ListByRunAfter.
	Subscribe(runID string) (<-chan struct{}, func())
}
