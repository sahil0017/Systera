package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/systera/control-plane/internal/observability"
	"github.com/systera/control-plane/internal/platform/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		os.Exit(1)
	}
	logger := observability.NewLogger(cfg.LogLevel, "systera-worker-simulation")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("digital twin simulation worker initialized (Stage 0 skeleton)")
	<-ctx.Done()
	logger.Info("shutting down simulation worker")
}
