// Command scheduler runs the control plane's background loops:
//   - the transactional outbox relay (outbox_events -> NATS JetStream),
//   - expired-lease reclamation (tasks whose worker stopped heartbeating are retried),
//   - starvation checks (runs that can no longer fit any task in their budget fail).
//
// Every loop is safe to run in several replicas concurrently.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/systera/control-plane/internal/artifacts"
	"github.com/systera/control-plane/internal/browserintel"
	"github.com/systera/control-plane/internal/evidence"
	"github.com/systera/control-plane/internal/observability"
	"github.com/systera/control-plane/internal/platform/config"
	"github.com/systera/control-plane/internal/platform/httpserver"
	"github.com/systera/control-plane/internal/platform/natsbus"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/platform/process"
	"github.com/systera/control-plane/internal/runs"
	"github.com/systera/control-plane/internal/scheduler"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "systera-scheduler:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := observability.NewLogger(cfg.LogLevel, "systera-scheduler")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Open(ctx, cfg.PostgresDSN())
	if err != nil {
		return err
	}
	defer pool.Close()

	bus, err := natsbus.Connect(ctx, cfg.NATSURL, "systera-scheduler", natsbus.StreamConfig{
		Name: cfg.NATSStream, SubjectPrefix: cfg.NATSSubjectPrefix,
	}, logger)
	if err != nil {
		return err
	}
	defer bus.Close()

	store := scheduler.NewStore(pool, logger)
	ledger := evidence.NewStore(pool)
	artifactStore := artifacts.NewStore(pool)
	lensStore := browserintel.NewStore(pool, nil, nil)
	runRepo := runs.NewPostgresRepository(pool)
	relay := outbox.NewRelay(pool, bus, logger, outbox.RelayOptions{
		SubjectPrefix: cfg.NATSSubjectPrefix, BatchSize: cfg.RelayBatchSize, Interval: cfg.RelayInterval,
	})

	var wg sync.WaitGroup
	start := func(fn func()) { wg.Add(1); go func() { defer wg.Done(); fn() }() }

	start(func() { relay.Run(ctx) })
	start(func() {
		process.Every(ctx, cfg.ReclaimInterval, func(ctx context.Context) {
			if n, err := store.ReclaimExpired(ctx, 100); err != nil && ctx.Err() == nil {
				logger.Error("reclaim expired leases", "error", err)
			} else if n > 0 {
				logger.Info("reclaimed expired leases", "count", n)
			}
			if res, err := ledger.Sweep(ctx); err != nil && ctx.Err() == nil {
				logger.Error("evidence revalidation sweep", "error", err)
			} else if res.EvidenceExpired+res.AssumptionsNeedingReview+res.ClaimsMarkedStale > 0 {
				logger.Info("evidence revalidation sweep", "evidence_expired", res.EvidenceExpired,
					"assumptions_needing_revalidation", res.AssumptionsNeedingReview, "claims_marked_stale", res.ClaimsMarkedStale)
			}
			if n, err := lensStore.SweepExpired(ctx); err != nil && ctx.Err() == nil {
				logger.Error("browser retention sweep failed", "error", err)
			} else if n > 0 {
				logger.Info("browser sessions past retention deleted", "sessions", n)
			}
			if res, err := artifactStore.Sweep(ctx); err != nil && ctx.Err() == nil {
				logger.Error("artifact drift sweep", "error", err)
			} else if res.ArtifactsInvalidated > 0 {
				logger.Info("artifact drift sweep", "artifacts_invalidated", res.ArtifactsInvalidated)
			}
			if res, err := artifactStore.ScheduleRevalidations(ctx, runRepo, store, 10); err != nil && ctx.Err() == nil {
				logger.Error("schedule artifact revalidation", "error", err)
			} else if res.Scheduled+res.Failed > 0 {
				logger.Info("artifact revalidation scheduled", "scheduled", res.Scheduled, "failed", res.Failed)
			}
			if n, err := store.FailStarvedRuns(ctx); err != nil && ctx.Err() == nil {
				logger.Error("fail starved runs", "error", err)
			} else if n > 0 {
				logger.Warn("failed runs that exhausted their budget", "count", n)
			}
		})
	})
	start(func() {
		process.ServeProbes(ctx, cfg.SchedulerHTTPAddr, httpserver.ProbeHandler(logger, []httpserver.ReadinessCheck{
			{Name: "postgres", Check: postgres.PingCheck(pool)},
			{Name: "schema", Check: postgres.SchemaCheck(pool)},
			{Name: "nats", Check: bus.Healthy},
		}), logger)
	})

	logger.Info("scheduler started", "nats_url", cfg.NATSURL, "stream", cfg.NATSStream)
	<-ctx.Done()
	logger.Info("shutting down scheduler")
	wg.Wait()
	return nil
}
