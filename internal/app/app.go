// Package app is the startup and shutdown the importer binaries share, so
// each cmd/ main only wires what's its own.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// ShutdownTimeout bounds everything Run does after the signal: draining the
// servers and the cleanup steps share it. It fits in the Deployments'
// terminationGracePeriodSeconds (20s).
const ShutdownTimeout = 10 * time.Second

// Setting is a required config value and the env var it comes from.
type Setting struct {
	Name  string // e.g. DATABASE_URL
	Value string
}

// Require returns an error naming every setting that's empty, so a pod
// missing several reports them all at once.
func Require(settings ...Setting) error {
	var missing []string
	for _, s := range settings {
		if s.Value == "" {
			missing = append(missing, s.Name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("required settings missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Run serves until ctx is cancelled (main cancels it on SIGTERM, which
// Kubernetes sends when it deletes a pod) or a server fails, then shuts down:
//
//  1. ready flips to false, so /readyz fails and Kubernetes stops routing
//     new requests to the pod. ready may be nil.
//  2. Each server stops accepting and waits for its requests to finish.
//  3. cleanup runs in order, e.g. stop the outbox relay, flush telemetry.
//     A failing step is logged and the rest still run.
//
// It returns the error of the server that failed, or nil after a signal.
func Run(ctx context.Context, ready *atomic.Bool, servers []*http.Server, cleanup ...func(context.Context) error) error {
	failed := make(chan error, len(servers))
	for _, srv := range servers {
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				failed <- fmt.Errorf("server on %s failed: %w", srv.Addr, err)
			}
		}()
	}
	if ready != nil {
		ready.Store(true)
	}

	var err error
	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining connections")
	case err = <-failed:
		slog.Error("server failed, shutting down", "err", err)
	}
	if ready != nil {
		ready.Store(false)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("graceful shutdown failed", "addr", srv.Addr, "err", err)
		}
	}
	for _, step := range cleanup {
		if err := step(shutdownCtx); err != nil {
			slog.Error("shutdown step failed", "err", err)
		}
	}
	slog.Info("server stopped")
	return err
}
