package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/postgres"
)

// NotifyChannel is the PostgreSQL LISTEN/NOTIFY channel fired by trg_outbox_notify.
const NotifyChannel = "systera_outbox"

// ErrListenerDown is reported by PostgresStore.Healthy while the LISTEN connection is lost.
var ErrListenerDown = errors.New("outbox listener not connected")

// InsertTx appends evt inside the caller's transaction, so the event commits or rolls
// back together with the state change that produced it.
func InsertTx(ctx context.Context, tx pgx.Tx, evt EventEnvelope) (EventEnvelope, error) {
	fillDefaults(&evt)
	payload, err := json.Marshal(evt.Payload)
	if err != nil {
		return EventEnvelope{}, fmt.Errorf("marshal event payload: %w", err)
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO outbox_events (event_id, event_type, schema_version, occurred_at, tenant_id, project_id, run_id, correlation_id, causation_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING seq`,
		evt.EventID, evt.EventType, evt.SchemaVersion, evt.OccurredAt, evt.TenantID,
		nullable(evt.ProjectID), nullable(evt.RunID), evt.CorrelationID, nullable(evt.CausationID), payload,
	).Scan(&evt.Seq)
	if err != nil {
		return EventEnvelope{}, fmt.Errorf("insert outbox event: %w", err)
	}
	return evt, nil
}

// PostgresStore reads outbox events and turns NOTIFY messages into subscriber wake-ups.
type PostgresStore struct {
	pool      *pgxpool.Pool
	logger    *slog.Logger
	notifier  *Notifier
	listening atomic.Bool
}

func NewPostgresStore(pool *pgxpool.Pool, logger *slog.Logger) *PostgresStore {
	return &PostgresStore{pool: pool, logger: logger, notifier: NewNotifier()}
}

func (s *PostgresStore) ListByRunAfter(ctx context.Context, tenantID, runID string, afterSeq int64, limit int) ([]EventEnvelope, error) {
	if !ids.Valid(runID) {
		return []EventEnvelope{}, nil
	}
	if limit <= 0 {
		limit = 1000
	}
	var out []EventEnvelope
	err := postgres.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT seq, event_id::text, event_type, schema_version, occurred_at, tenant_id::text,
			       COALESCE(project_id::text, ''), COALESCE(run_id::text, ''), correlation_id::text,
			       COALESCE(causation_id::text, ''), payload
			FROM outbox_events
			WHERE run_id = $1 AND seq > $2
			ORDER BY seq
			LIMIT $3`, runID, afterSeq, limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (EventEnvelope, error) {
			var e EventEnvelope
			var payload []byte
			if err := row.Scan(&e.Seq, &e.EventID, &e.EventType, &e.SchemaVersion, &e.OccurredAt, &e.TenantID,
				&e.ProjectID, &e.RunID, &e.CorrelationID, &e.CausationID, &payload); err != nil {
				return e, err
			}
			e.OccurredAt = e.OccurredAt.UTC()
			return e, json.Unmarshal(payload, &e.Payload)
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []EventEnvelope{}
	}
	return out, nil
}

func (s *PostgresStore) Subscribe(runID string) (<-chan struct{}, func()) {
	return s.notifier.Subscribe(runID)
}

// Healthy reports whether the LISTEN connection is currently established.
func (s *PostgresStore) Healthy(context.Context) error {
	if !s.listening.Load() {
		return ErrListenerDown
	}
	return nil
}

// Listen holds a dedicated connection LISTENing on NotifyChannel until ctx is done,
// reconnecting with backoff. After every (re)connect all subscribers are woken so they
// re-read anything committed while the listener was down.
func (s *PostgresStore) Listen(ctx context.Context) {
	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		err := s.listenOnce(ctx)
		s.listening.Store(false)
		if ctx.Err() != nil {
			return
		}
		s.logger.Warn("outbox listener disconnected; reconnecting", "error", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

func (s *PostgresStore) listenOnce(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// The connection carries LISTEN state; never hand it back to the pool.
	defer func() { _ = conn.Hijack().Close(context.Background()) }()

	if _, err := conn.Exec(ctx, "LISTEN "+NotifyChannel); err != nil {
		return err
	}
	s.listening.Store(true)
	s.logger.Info("outbox listener connected", "channel", NotifyChannel)
	s.notifier.NotifyAll()

	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		s.notifier.Notify(n.Payload)
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
