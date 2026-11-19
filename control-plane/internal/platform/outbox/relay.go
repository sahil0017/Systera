package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/platform/postgres"
)

// Publisher delivers one event to the durable event backbone. msgID must be used for
// broker-side de-duplication so redelivery after a crash is idempotent.
type Publisher interface {
	Publish(ctx context.Context, subject, msgID string, data []byte) error
}

// WireEnvelope is the contracts/events/event-envelope.v1.json shape published to NATS
// (the internal outbox sequence is not part of the public contract).
type WireEnvelope struct {
	EventID       string         `json:"event_id"`
	EventType     string         `json:"event_type"`
	SchemaVersion string         `json:"schema_version"`
	OccurredAt    time.Time      `json:"occurred_at"`
	TenantID      string         `json:"tenant_id"`
	ProjectID     *string        `json:"project_id"`
	RunID         *string        `json:"run_id"`
	CorrelationID string         `json:"correlation_id"`
	CausationID   *string        `json:"causation_id"`
	Payload       map[string]any `json:"payload"`
}

func ToWire(e EventEnvelope) WireEnvelope {
	opt := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	return WireEnvelope{
		EventID: e.EventID, EventType: e.EventType, SchemaVersion: e.SchemaVersion, OccurredAt: e.OccurredAt.UTC(),
		TenantID: e.TenantID, ProjectID: opt(e.ProjectID), RunID: opt(e.RunID), CorrelationID: e.CorrelationID,
		CausationID: opt(e.CausationID), Payload: e.Payload,
	}
}

// Subject maps an event type to its NATS subject, e.g. run.created -> systera.events.run.created.
func Subject(prefix, eventType string) string {
	return strings.TrimSuffix(prefix, ".") + "." + eventType
}

// RelayOptions tunes the relay.
type RelayOptions struct {
	SubjectPrefix string        // default "systera.events"
	BatchSize     int           // per partition, default 100
	Interval      time.Duration // idle poll interval, default 500ms
	Partitions    int           // ordering partitions, default 16
}

// relayLockClass namespaces the relay's advisory locks (key 1 of the two-key form).
const relayLockClass int32 = 0x5359_5245 // "SYRE"

// Relay moves committed outbox rows to the event backbone and marks them published.
// Delivery is at-least-once from the relay's side; broker de-duplication on event_id
// (Nats-Msg-Id) makes it effectively-once within the stream's dedup window.
//
// Ordering: events are hashed into Partitions by run_id (tenant_id for run-less events).
// A replica relays a partition only while holding that partition's transaction-scoped
// advisory lock, and within it publishes in seq order, stopping at the first failure. So
// any number of replicas can run, and each run's events always reach the stream in the
// order they committed (writers of one run serialize on its design_runs row, so seq order
// equals commit order per run). There is no ordering guarantee across different runs.
type Relay struct {
	pool   *pgxpool.Pool
	pub    Publisher
	logger *slog.Logger
	opts   RelayOptions
}

func NewRelay(pool *pgxpool.Pool, pub Publisher, logger *slog.Logger, opts RelayOptions) *Relay {
	if opts.SubjectPrefix == "" {
		opts.SubjectPrefix = "systera.events"
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 100
	}
	if opts.Interval <= 0 {
		opts.Interval = 500 * time.Millisecond
	}
	if opts.Partitions <= 0 {
		opts.Partitions = 16
	}
	return &Relay{pool: pool, pub: pub, logger: logger, opts: opts}
}

// Run relays until ctx is done.
func (r *Relay) Run(ctx context.Context) {
	for ctx.Err() == nil {
		n, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			r.logger.Warn("outbox relay batch failed", "error", err)
		}
		if n > 0 && err == nil {
			continue // backlog: keep draining
		}
		select {
		case <-ctx.Done():
		case <-time.After(r.opts.Interval):
		}
	}
}

// RunOnce visits every partition once (starting at a random one, so replicas spread
// out), relaying up to BatchSize events from each partition whose lock it obtains. It
// returns how many events were published and the first publish error, if any; a failed
// event keeps its error on the row and blocks only its own partition until it succeeds.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	total := 0
	var firstErr error
	start := rand.IntN(r.opts.Partitions)
	for i := 0; i < r.opts.Partitions; i++ {
		n, err := r.relayPartition(ctx, (start+i)%r.opts.Partitions)
		total += n
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return total, firstErr
}

func (r *Relay) relayPartition(ctx context.Context, partition int) (int, error) {
	published := 0
	var publishErr error
	err := postgres.WithSystemTx(ctx, r.pool, func(tx pgx.Tx) error {
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1, $2)`, relayLockClass, int32(partition)).Scan(&locked); err != nil {
			return err
		}
		if !locked {
			return nil // another replica owns this partition right now
		}
		rows, err := tx.Query(ctx, `
			SELECT seq, event_id::text, event_type, schema_version, occurred_at, tenant_id::text,
			       COALESCE(project_id::text, ''), COALESCE(run_id::text, ''), correlation_id::text,
			       COALESCE(causation_id::text, ''), payload
			FROM outbox_events
			WHERE published_at IS NULL
			  AND abs(hashtext(COALESCE(run_id, tenant_id)::text)::bigint) % $1 = $2
			ORDER BY seq
			LIMIT $3`, r.opts.Partitions, partition, r.opts.BatchSize)
		if err != nil {
			return err
		}
		batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (EventEnvelope, error) {
			var e EventEnvelope
			var payload []byte
			if err := row.Scan(&e.Seq, &e.EventID, &e.EventType, &e.SchemaVersion, &e.OccurredAt, &e.TenantID,
				&e.ProjectID, &e.RunID, &e.CorrelationID, &e.CausationID, &payload); err != nil {
				return e, err
			}
			return e, json.Unmarshal(payload, &e.Payload)
		})
		if err != nil {
			return err
		}

		done := make([]int64, 0, len(batch))
		for _, evt := range batch {
			data, err := json.Marshal(ToWire(evt))
			if err != nil {
				publishErr = fmt.Errorf("marshal event %s: %w", evt.EventID, err)
			} else {
				publishErr = r.pub.Publish(ctx, Subject(r.opts.SubjectPrefix, evt.EventType), evt.EventID, data)
			}
			if publishErr != nil {
				if _, err := tx.Exec(ctx, `
					UPDATE outbox_events SET publish_attempts = publish_attempts + 1, last_error = $2 WHERE seq = $1`,
					evt.Seq, truncate(publishErr.Error(), 1000)); err != nil {
					return err
				}
				break // later events of this partition wait, preserving per-run order
			}
			done = append(done, evt.Seq)
		}
		if len(done) > 0 {
			if _, err := tx.Exec(ctx, `
				UPDATE outbox_events SET published_at = NOW(), publish_attempts = publish_attempts + 1, last_error = NULL
				WHERE seq = ANY($1)`, done); err != nil {
				return err
			}
		}
		published = len(done)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return published, publishErr
}

// Lag reports how many events are waiting to be published (for readiness/metrics).
func (r *Relay) Lag(ctx context.Context) (int64, error) {
	var n int64
	err := postgres.WithSystemTx(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&n)
	})
	return n, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
