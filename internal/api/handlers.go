package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/vandelay-labs/importer/internal/items"
	"github.com/vandelay-labs/importer/internal/ratelimit"
)

// itemStore is what the handlers need: items.Store in production, memStore
// in tests. Get and Delete return items.ErrNotFound for an unknown id.
type itemStore interface {
	List(ctx context.Context) ([]items.Item, error)
	Get(ctx context.Context, id int64) (items.Item, error)
	Create(ctx context.Context, name, kind string) (items.Item, error)
	Delete(ctx context.Context, id int64) error
}

// NewRouter serves the API. readyz is the readiness check (see
// ReadinessHandler). limit wraps the /api/items routes (see RateLimit); nil
// means no limit.
func NewRouter(cfg Config, s itemStore, readyz http.Handler, limit func(http.Handler) http.Handler) http.Handler {
	mux := http.NewServeMux()
	if limit == nil {
		limit = func(h http.Handler) http.Handler { return h }
	}
	// The item endpoints are rate limited; the probes and GET / aren't.
	limited := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, limit(h)) }

	// Liveness: "is the process alive?" If this fails, k8s restarts the container.
	// Keep it dependency-free: restarting this pod can't fix a down database.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Readiness: "can it take traffic?" If this fails, k8s removes the pod from the Service.
	// Pings postgres, redis and rabbitmq (see health.go).
	mux.Handle("GET /readyz", readyz)

	// Info: shows which pod answered. Hit it repeatedly to watch the Service load-balance.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		hostname, _ := os.Hostname()
		writeJSON(w, http.StatusOK, map[string]string{
			"message": cfg.Message,
			"version": cfg.Version,
			"env":     cfg.Env,
			"pod":     hostname,
		})
	})

	limited("GET /api/items", func(w http.ResponseWriter, r *http.Request) {
		items, err := s.List(r.Context())
		if err != nil {
			writeStoreError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	})

	limited("POST /api/items", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		body.Name = strings.TrimSpace(body.Name)
		if body.Name == "" {
			writeError(w, http.StatusBadRequest, "name is required")
			return
		}
		if body.Kind != "import" && body.Kind != "export" {
			writeError(w, http.StatusBadRequest, `kind must be "import" or "export"`)
			return
		}
		it, err := s.Create(r.Context(), body.Name, body.Kind)
		if err != nil {
			writeStoreError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, it)
	})

	limited("GET /api/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := itemID(w, r)
		if !ok {
			return
		}
		it, err := s.Get(r.Context(), id)
		if err != nil {
			writeStoreError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, it)
	})

	limited("DELETE /api/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := itemID(w, r)
		if !ok {
			return
		}
		if err := s.Delete(r.Context(), id); err != nil {
			writeStoreError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

// RateLimit returns the middleware that limits the item endpoints per client
// IP, answering 429 {"error": "rate limit exceeded"} past the limit.
func RateLimit(l ratelimit.Allower) func(http.Handler) http.Handler {
	return ratelimit.Middleware(l, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// itemID parses the {id} path value. A non-numeric id can't exist, so it's a
// 404 like any other unknown id.
func itemID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "item not found")
		return 0, false
	}
	return id, true
}

// writeStoreError maps a store error to a response. Anything but "not found"
// is logged with its trace and returned as a bare 500, so database details
// don't leak to clients.
func writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, items.ErrNotFound) {
		writeError(w, http.StatusNotFound, "item not found")
		return
	}
	slog.ErrorContext(r.Context(), "store error", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}
