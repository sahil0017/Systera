package outbox

import (
	"context"
	"sync"
	"time"

	"github.com/systera/control-plane/internal/platform/ids"
)

// MemoryStore is an in-memory outbox for unit tests. Production uses PostgresStore.
type MemoryStore struct {
	mu        sync.RWMutex
	seq       int64
	events    []EventEnvelope
	appendErr error
	notifier  *Notifier
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{notifier: NewNotifier()}
}

// FailAppends makes subsequent Append calls return err (nil restores normal behaviour).
// It exists to exercise atomicity of callers in tests.
func (s *MemoryStore) FailAppends(err error) {
	s.mu.Lock()
	s.appendErr = err
	s.mu.Unlock()
}

// Append records an event. Callers that need atomicity with their own state must hold
// their own lock across the call and roll back on error.
func (s *MemoryStore) Append(_ context.Context, evt EventEnvelope) (EventEnvelope, error) {
	s.mu.Lock()
	if s.appendErr != nil {
		err := s.appendErr
		s.mu.Unlock()
		return EventEnvelope{}, err
	}
	s.seq++
	evt.Seq = s.seq
	fillDefaults(&evt)
	s.events = append(s.events, evt)
	s.mu.Unlock()

	if evt.RunID != "" {
		s.notifier.Notify(evt.RunID)
	}
	return evt, nil
}

func (s *MemoryStore) ListByRunAfter(_ context.Context, tenantID, runID string, afterSeq int64, limit int) ([]EventEnvelope, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]EventEnvelope, 0)
	for _, evt := range s.events {
		if evt.TenantID == tenantID && evt.RunID == runID && evt.Seq > afterSeq {
			out = append(out, evt)
			if limit > 0 && len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

// ListByRun returns all events for a run.
func (s *MemoryStore) ListByRun(ctx context.Context, tenantID, runID string) ([]EventEnvelope, error) {
	return s.ListByRunAfter(ctx, tenantID, runID, 0, 0)
}

func (s *MemoryStore) Subscribe(runID string) (<-chan struct{}, func()) {
	return s.notifier.Subscribe(runID)
}

func fillDefaults(evt *EventEnvelope) {
	if evt.EventID == "" {
		evt.EventID = ids.New()
	}
	if evt.SchemaVersion == "" {
		evt.SchemaVersion = "1.0"
	}
	if evt.OccurredAt.IsZero() {
		evt.OccurredAt = time.Now().UTC()
	}
	if evt.Payload == nil {
		evt.Payload = map[string]any{}
	}
}
