package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/systera/control-plane/internal/artifacts"
	"github.com/systera/control-plane/internal/auth"
	"github.com/systera/control-plane/internal/browserintel"
	"github.com/systera/control-plane/internal/counterfactual"
	"github.com/systera/control-plane/internal/decisions"
	"github.com/systera/control-plane/internal/engineeringpulse"
	"github.com/systera/control-plane/internal/evidence"
	"github.com/systera/control-plane/internal/graph"
	"github.com/systera/control-plane/internal/knowledge"
	"github.com/systera/control-plane/internal/learning"
	"github.com/systera/control-plane/internal/memory"
	"github.com/systera/control-plane/internal/models"
	"github.com/systera/control-plane/internal/observability"
	"github.com/systera/control-plane/internal/platform/config"
	"github.com/systera/control-plane/internal/platform/httpserver"
	"github.com/systera/control-plane/internal/platform/objectstore"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/policy"
	"github.com/systera/control-plane/internal/projects"
	"github.com/systera/control-plane/internal/runs"
	"github.com/systera/control-plane/internal/scheduler"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "systera-api:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := observability.NewLogger(cfg.LogLevel, "systera-api")

	// Refuse to start without a verifiable identity configuration.
	if err := cfg.ValidateAuth(); err != nil {
		return err
	}
	var keySet auth.KeySet
	if cfg.AuthJWKSURL != "" {
		keySet, err = auth.NewJWKSKeySet(cfg.AuthJWKSURL, 5*time.Minute)
		if err != nil {
			return err
		}
	} else {
		pemBytes := []byte(cfg.AuthPublicKeyPEM)
		if len(pemBytes) == 0 {
			pemBytes, err = os.ReadFile(cfg.AuthPublicKeyFile)
			if err != nil {
				return fmt.Errorf("read public key file: %w", err)
			}
		}
		pubKey, err := auth.ParsePublicKeyPEM(pemBytes)
		if err != nil {
			return err
		}
		keySet, err = auth.NewStaticKeySet(pubKey, nil)
		if err != nil {
			return err
		}
	}
	verifier, err := auth.NewVerifier(keySet, cfg.AuthIssuer, cfg.AuthAudience)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Open(ctx, cfg.PostgresDSN())
	if err != nil {
		return err
	}
	defer pool.Close()

	if cfg.AutoMigrate {
		applied, err := postgres.MigrateUp(ctx, pool)
		if err != nil {
			return err
		}
		logger.Info("migrations applied", "versions", applied)
	}

	events := outbox.NewPostgresStore(pool, logger)
	listenCtx, stopListening := context.WithCancel(ctx)
	defer stopListening()
	go events.Listen(listenCtx)

	graphService := graph.NewService(pool)
	artifactStore := artifacts.NewStore(pool)
	proj := artifacts.NewProjector(artifacts.NewRLPolicy(0.2, 0.25, 0.2))
	var objStore objectstore.Store = objectstore.NewMemoryStore(cfg.S3Bucket)
	if s3Client, sErr := objectstore.NewS3Store(objectstore.S3Config{
		Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket,
		AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey,
	}); sErr == nil {
		objStore = s3Client
	}
	asyncProj := artifacts.NewAsyncProjector(proj, objStore, artifactStore, graphService, logger)
	go asyncProj.Run(ctx)

	embedder := models.NewDeterministicHashEmbedder(models.DefaultEmbeddingDimensions)
	knowledgeStore := knowledge.NewStore(pool, embedder, "")
	memoryStore := memory.NewStore(pool, embedder, "")
	pulseStore := engineeringpulse.NewStore(pool, knowledgeStore)

	projRepo := projects.NewPostgresRepository(pool)
	browserStore := browserintel.NewStore(pool, projRepo, graphService)
	evidenceStore := evidence.NewStore(pool)
	schedulerStore := scheduler.NewStore(pool, logger)
	runRepo := runs.NewPostgresRepository(pool)
	browserStore.SetLedger(evidenceStore)            // 4.3: findings become ledger claims
	browserStore.SetRuntime(runRepo, schedulerStore) // 4.2: analysis DAG through the scheduler

	// Lens token exchange (3.5) needs the signing key; in production tokens come from the
	// IdP and this endpoint stays disabled unless SYSTERA_AUTH_PRIVATE_KEY_FILE is set.
	var tokenIssuer *auth.TokenIssuer
	if cfg.AuthPrivateKeyFile != "" {
		pemBytes, err := os.ReadFile(cfg.AuthPrivateKeyFile)
		if err != nil {
			return fmt.Errorf("read SYSTERA_AUTH_PRIVATE_KEY_FILE: %w", err)
		}
		priv, err := auth.ParsePrivateKeyPEM(pemBytes)
		if err != nil {
			return err
		}
		if tokenIssuer, err = auth.NewTokenIssuer(priv, cfg.AuthKeyID, cfg.AuthIssuer, cfg.AuthAudience); err != nil {
			return err
		}
	}

	decisionStore := decisions.NewStore(pool)
	counterfactualStore := counterfactual.NewStore(pool, graphService, decisionStore)
	patternRetriever := engineeringpulse.NewRetriever(pulseStore, knowledgeStore)
	learningStore := learning.NewStore(pool, graphService, patternRetriever)

	srv := httpserver.New(httpserver.Options{
		Logger:         logger,
		Projects:       projRepo,
		Runs:           runRepo,
		Events:         events,
		Tasks:          schedulerStore,
		Graph:          graphService,
		Artifacts:      artifactStore,
		Projector:      proj,
		AsyncProjector: asyncProj,
		Impact:         graphService,
		Evidence:       evidenceStore,
		Decisions:      decisionStore,
		Approvals:      policy.NewApprovalStore(pool),
		Knowledge:      knowledgeStore,
		Memory:         memoryStore,
		Browser:        browserStore,
		TokenIssuer:    tokenIssuer,
		Pulse:          pulseStore,
		Patterns:       patternRetriever,
		Counterfactual: counterfactualStore,
		Learning:       learningStore,
		WebDir:         cfg.WebDir,
		Authenticator:  auth.NewAuthenticator(verifier, auth.NewPostgresMemberships(pool)),
		Readiness: []httpserver.ReadinessCheck{
			{Name: "postgres", Check: postgres.PingCheck(pool)},
			{Name: "schema", Check: postgres.SchemaCheck(pool)},
			{Name: "event_listener", Check: events.Healthy},
		},
		SSEPollInterval:      cfg.SSEPollInterval,
		SSEHeartbeatInterval: cfg.SSEHeartbeatInterval,
	})

	// Cancelled when shutdown begins so long-lived SSE streams end promptly.
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("control plane API listening", "addr", cfg.HTTPAddr, "env", cfg.Env)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
	}
	logger.Info("shutting down control plane API")

	cancelBase()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}
