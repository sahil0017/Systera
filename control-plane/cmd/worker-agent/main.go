// Command worker-agent runs specialist agents: it leases READY tasks, executes them via
// the Model Gateway and Tool Gateway, checkpoints progress and reports AgentResults.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/systera/control-plane/internal/agent"
	"github.com/systera/control-plane/internal/artifacts"
	"github.com/systera/control-plane/internal/browserintel"
	"github.com/systera/control-plane/internal/decisions"
	"github.com/systera/control-plane/internal/engineeringpulse"
	"github.com/systera/control-plane/internal/evidence"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/knowledge"
	"github.com/systera/control-plane/internal/memory"
	"github.com/systera/control-plane/internal/models"
	"github.com/systera/control-plane/internal/observability"
	"github.com/systera/control-plane/internal/platform/config"
	"github.com/systera/control-plane/internal/platform/httpserver"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/platform/process"
	"github.com/systera/control-plane/internal/policy"
	"github.com/systera/control-plane/internal/projects"
	"github.com/systera/control-plane/internal/scheduler"
	"github.com/systera/control-plane/internal/tools"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "systera-worker-agent:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := observability.NewLogger(cfg.LogLevel, "systera-worker-agent").With("worker_id", cfg.WorkerID)

	if err := cfg.ValidateModel(); err != nil {
		return err
	}
	pricing, err := models.ParsePricing(cfg.ModelPricingJSON)
	if err != nil {
		return err
	}
	provider := models.NewOpenAICompatible(cfg.ModelBaseURL, cfg.ModelAPIKey, cfg.ModelTimeout)
	if _, ok := pricing[cfg.ModelDefault]; !ok {
		return fmt.Errorf("default model %q has no entry in SYSTERA_MODEL_PRICING (use 0 for free/local models)", cfg.ModelDefault)
	}
	verifyCtx, cancelVerify := context.WithTimeout(context.Background(), 15*time.Second)
	err = provider.VerifyModel(verifyCtx, cfg.ModelDefault)
	cancelVerify()
	switch {
	case errors.Is(err, models.ErrModelNotServed):
		return err
	case err != nil:
		logger.Warn("could not verify the default model is served; continuing", "error", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Open(ctx, cfg.PostgresDSN())
	if err != nil {
		return err
	}
	defer pool.Close()

	graphSvc := graph.NewService(pool)
	reg := tools.NewRegistry()
	if err := agent.RegisterBuiltinTools(reg); err != nil {
		return err
	}
	evStore := evidence.NewStore(pool)
	if err := agent.RegisterStage2Tools(reg, graphSvc, evStore, decisions.NewStore(pool), policy.NewApprovalStore(pool)); err != nil {
		return err
	}
	if err := agent.RegisterProjectionTool(reg, graphSvc, artifacts.NewProjector(artifacts.NewRLPolicy(0.2, 0.25, 0.2))); err != nil {
		return err
	}
	embedder := models.NewDeterministicHashEmbedder(models.DefaultEmbeddingDimensions)
	kStore := knowledge.NewStore(pool, embedder, "")
	if err := agent.RegisterKnowledgeTools(reg, kStore, memory.NewStore(pool, embedder, ""), graphSvc, evStore); err != nil {
		return err
	}
	pulseRetriever := engineeringpulse.NewRetriever(engineeringpulse.NewStore(pool, kStore), kStore)
	if err := agent.RegisterPulseTools(reg, pulseRetriever); err != nil {
		return err
	}
	lensStore := browserintel.NewStore(pool, projects.NewPostgresRepository(pool), graphSvc)
	lensStore.SetLedger(evStore)
	if err := agent.RegisterWebsiteTools(reg, lensStore); err != nil {
		return err
	}
	gateway := models.NewGateway(provider, pricing, cfg.ModelDefault)

	schedStore := scheduler.NewStore(pool, logger)
	runner := agent.NewRunner(agent.Config{
		WorkerID:     cfg.WorkerID,
		AgentTypes:   cfg.WorkerAgentTypes,
		LeaseTTL:     cfg.LeaseTTL,
		PollInterval: cfg.WorkerPollInterval,
		Temperature:  cfg.ModelTemperature,
	}, schedStore, agent.NewPostgresCatalog(pool), gateway, tools.NewGateway(reg, logger), logger)
	runner.SetGraphVersionResolver(graphVersionLookup{svc: graphSvc})
	runner.SetTaskSubmitter(schedStore)
	runner.SetKnowledgeAdvisor(pulseRetriever)

	go process.ServeProbes(ctx, cfg.WorkerHTTPAddr, httpserver.ProbeHandler(logger, []httpserver.ReadinessCheck{
		{Name: "postgres", Check: postgres.PingCheck(pool)},
		{Name: "schema", Check: postgres.SchemaCheck(pool)},
	}), logger)

	logger.Info("model gateway configured", "provider", cfg.ModelProvider, "base_url", cfg.ModelBaseURL, "default_model", cfg.ModelDefault)
	return runner.Run(ctx)
}

type graphVersionLookup struct{ svc *graph.Service }

func (g graphVersionLookup) CurrentGraphVersion(ctx context.Context, tenantID, projectID string) int {
	m, err := g.svc.ModelForProject(ctx, tenantID, projectID)
	if err != nil {
		return 0
	}
	return m.CurrentVersion
}
