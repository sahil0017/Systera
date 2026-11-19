// Package process holds small helpers shared by the long-running binaries.
package process

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// ServeProbes runs an HTTP probe server until ctx is done.
func ServeProbes(ctx context.Context, addr string, h http.Handler, logger *slog.Logger) {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	logger.Info("probe server listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("probe server failed", "error", err)
	}
}

// Every calls fn immediately and then every interval until ctx is done.
func Every(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		fn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
