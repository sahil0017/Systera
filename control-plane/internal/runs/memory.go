package runs

import (
	"context"
	"sync"
	"time"

	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
)

// MemoryRepository is an in-memory Repository for unit tests. It holds its lock across
// the outbox append and only commits the run change if the append succeeds, mirroring
// the single-transaction behaviour of PostgresRepository.
type MemoryRepository struct {
	mu          sync.RWMutex
	runs        map[string]DesignRun
	idempotency map[string]string
	outbox      *outbox.MemoryStore
}

func NewMemoryRepository(out *outbox.MemoryStore) *MemoryRepository {
	return &MemoryRepository{
		runs:        make(map[string]DesignRun),
		idempotency: make(map[string]string),
		outbox:      out,
	}
}

func (r *MemoryRepository) CreateIdempotent(ctx context.Context, run DesignRun) (DesignRun, bool, error) {
	run, err := Prepare(run)
	if err != nil {
		return DesignRun{}, false, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	idemComposite := run.TenantID + "\x00" + run.ProjectID + "\x00" + run.IdempotencyKey
	if run.IdempotencyKey != "" {
		if existingID, ok := r.idempotency[idemComposite]; ok {
			return checkReplay(r.runs[existingID], run)
		}
	}

	run.ID = ids.New()
	now := time.Now().UTC()
	run.State = StateCreated
	run.SpentBudgetUSD = 0
	run.CreatedAt = now
	run.UpdatedAt = now

	if _, err := r.outbox.Append(ctx, createdEvent(run)); err != nil {
		return DesignRun{}, false, err // nothing committed
	}
	r.runs[run.ID] = run
	if run.IdempotencyKey != "" {
		r.idempotency[idemComposite] = run.ID
	}
	return run, true, nil
}

func (r *MemoryRepository) GetByID(_ context.Context, tenantID, runID string) (DesignRun, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	run, ok := r.runs[runID]
	if !ok || run.TenantID != tenantID {
		return DesignRun{}, ErrNotFound
	}
	return run, nil
}

func (r *MemoryRepository) UpdateState(ctx context.Context, tenantID, runID string, next State) (DesignRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	run, ok := r.runs[runID]
	if !ok || run.TenantID != tenantID {
		return DesignRun{}, ErrNotFound
	}
	if !isValidTransition(run.State, next) {
		return DesignRun{}, ErrInvalidTransition
	}
	from := run.State
	run.State = next
	run.UpdatedAt = time.Now().UTC()
	if _, err := r.outbox.Append(ctx, stateChangedEvent(run, from)); err != nil {
		return DesignRun{}, err
	}
	r.runs[runID] = run
	return run, nil
}
