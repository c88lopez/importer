package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
)

const checkTimeout = 2 * time.Second

// checker pings one dependency. A nil check means the dependency isn't
// configured (its URL env var is empty), and it's reported as "skipped".
// Only critical dependencies make the pod not ready when they fail.
type checker struct {
	name     string
	critical bool
	check    func(ctx context.Context) error
}

type checkResult struct {
	Status   string `json:"status"` // "ok", "error" or "skipped"
	Critical bool   `json:"critical"`
	Latency  string `json:"latency,omitempty"`
	Error    string `json:"error,omitempty"`
}

// newCheckers builds a checker per dependency. Clients connect lazily, so the
// app still starts when a dependency is down; the failure shows up in /readyz.
func newCheckers(cfg config) ([]checker, func(), error) {
	var closers []func()
	cleanup := func() {
		for _, c := range closers {
			c()
		}
	}

	pg := checker{name: "postgres"}
	if cfg.DatabaseURL != "" {
		pool, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
		if err != nil {
			return nil, nil, err
		}
		closers = append(closers, pool.Close)
		pg.check = pool.Ping
	}

	rd := checker{name: "redis"}
	if cfg.RedisURL != "" {
		opts, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		// Fail fast: a health check should report "down", not retry until the timeout.
		opts.MaxRetries, opts.DialerRetries = -1, 1
		client := redis.NewClient(opts)
		closers = append(closers, func() { _ = client.Close() })
		rd.check = func(ctx context.Context) error { return client.Ping(ctx).Err() }
	}

	mq := checker{name: "rabbitmq"}
	if cfg.RabbitMQURL != "" {
		// Open and close a connection per check: simple, and proves the
		// broker accepts our credentials right now.
		mq.check = func(ctx context.Context) error {
			deadline, _ := ctx.Deadline()
			conn, err := amqp.DialConfig(cfg.RabbitMQURL, amqp.Config{
				Dial: amqp.DefaultDial(time.Until(deadline)),
			})
			if err != nil {
				return err
			}
			return conn.Close()
		}
	}

	checks := []checker{pg, rd, mq}
	if err := markCritical(checks, cfg.CriticalDeps); err != nil {
		cleanup()
		return nil, nil, err
	}
	return checks, cleanup, nil
}

// markCritical flags the named checkers as critical, rejecting unknown names
// so a typo in CRITICAL_DEPENDENCIES fails at startup instead of silently.
func markCritical(checks []checker, names []string) error {
	for _, name := range names {
		found := false
		for i := range checks {
			if checks[i].name == name {
				checks[i].critical, found = true, true
			}
		}
		if !found {
			return fmt.Errorf("unknown dependency %q in CRITICAL_DEPENDENCIES (want postgres, redis or rabbitmq)", name)
		}
	}
	return nil
}

// readinessHandler returns 503 while the pod is shutting down or a critical
// dependency is down. A non-critical failure returns 200 with status
// "degraded", so it's visible without pulling the pod out of the Service.
// Checks run concurrently.
func readinessHandler(ready *atomic.Bool, checks []checker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "shutting down"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
		defer cancel()

		var (
			mu             sync.Mutex
			wg             sync.WaitGroup
			results        = make(map[string]checkResult, len(checks))
			criticalFailed bool
			degraded       bool
		)
		for _, c := range checks {
			if c.check == nil {
				results[c.name] = checkResult{Status: "skipped", Critical: c.critical}
				continue
			}
			wg.Add(1)
			go func(c checker) {
				defer wg.Done()
				start := time.Now()
				err := c.check(ctx)
				res := checkResult{Status: "ok", Critical: c.critical, Latency: time.Since(start).String()}
				if err != nil {
					res.Status, res.Error = "error", err.Error()
				}
				mu.Lock()
				defer mu.Unlock()
				results[c.name] = res
				switch {
				case err != nil && c.critical:
					criticalFailed = true
				case err != nil:
					degraded = true
				}
			}(c)
		}
		wg.Wait()

		status, code := "ready", http.StatusOK
		switch {
		case criticalFailed:
			status, code = "not ready", http.StatusServiceUnavailable
		case degraded:
			status = "degraded"
		}
		writeJSON(w, code, map[string]any{"status": status, "checks": results})
	}
}
