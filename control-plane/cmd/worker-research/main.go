// Command worker-research runs the Engineering Pulse pipeline (spec §2.2): it fetches approved
// sources within their terms and rate limits, deduplicates and classifies items, scores trends,
// researches and validates topics, publishes validated knowledge and notifies subscribed agents.
//
// Environment: SYSTERA_DATABASE_URL (or SYSTERA_POSTGRES_*), SYSTERA_PULSE_INTERVAL (15m),
// SYSTERA_PULSE_REGISTRY_FILE (optional registry synced at start; sources stay PENDING_REVIEW
// until approved with `systera-admin pulse-approve`), SYSTERA_GITHUB_TOKEN (optional),
// SYSTERA_PULSE_USER_AGENT, SYSTERA_RESEARCH_HTTP_ADDR (:8083).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	ep "github.com/systera/control-plane/internal/engineeringpulse"
	"github.com/systera/control-plane/internal/knowledge"
	"github.com/systera/control-plane/internal/models"
	"github.com/systera/control-plane/internal/observability"
	"github.com/systera/control-plane/internal/platform/config"
	"github.com/systera/control-plane/internal/platform/httpserver"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/platform/process"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "systera-worker-research:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := observability.NewLogger(cfg.LogLevel, "systera-worker-research")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Open(ctx, cfg.PostgresDSN())
	if err != nil {
		return err
	}
	defer pool.Close()

	embedder := models.NewDeterministicHashEmbedder(models.DefaultEmbeddingDimensions)
	store := ep.NewStore(pool, knowledge.NewStore(pool, embedder, ""))
	if cfg.PulseRegistryFile != "" {
		defs, err := ep.LoadRegistryFile(cfg.PulseRegistryFile)
		if err != nil {
			return err
		}
		res, err := store.SyncSources(ctx, defs)
		if err != nil {
			return err
		}
		logger.Info("source registry synced", "created", res.Created, "updated", len(res.Updated), "needs_review", res.NeedsReview)
	}

	go process.ServeProbes(ctx, cfg.ResearchHTTPAddr, httpserver.ProbeHandler(logger, []httpserver.ReadinessCheck{
		{Name: "postgres", Check: postgres.PingCheck(pool)},
		{Name: "schema", Check: postgres.SchemaCheck(pool)},
	}), logger)

	p := &ep.Pipeline{Store: store, Fetcher: ep.NewFetcher(cfg.PulseUserAgent, cfg.PulseGitHubToken), Logger: logger}
	logger.Info("engineering pulse worker started", "interval", cfg.PulseInterval)
	return p.Run(ctx, cfg.PulseInterval)
}
