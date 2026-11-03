package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewOpsRouter serves the probes and Prometheus metrics. The worker has no
// Service, so nothing routes traffic by readiness. It still matters: a
// rollout waits for new pods to be ready before stopping old ones, so a
// worker that can't consume never replaces one that can.
func NewOpsRouter(metrics http.Handler, c *Consumer, db *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics)

	// Liveness: the process answers. The consumer reconnects on its own, so a
	// broker or database outage is not a reason to restart the pod.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Readiness: consuming from RabbitMQ, and Postgres answers.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		checks := map[string]string{"rabbitmq": "ok", "postgres": "ok"}
		status, code := "ready", http.StatusOK
		if !c.Connected() {
			checks["rabbitmq"] = "not consuming"
			status, code = "not ready", http.StatusServiceUnavailable
		}
		if err := db.Ping(ctx); err != nil {
			checks["postgres"] = err.Error()
			status, code = "not ready", http.StatusServiceUnavailable
		}
		writeJSON(w, code, map[string]any{"status": status, "checks": checks})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
