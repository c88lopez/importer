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
	"sync/atomic"

	"github.com/vandelay-labs/importer/internal/items"
)

// itemStore is what the handlers need: items.Store in production, memStore
// in tests. Get and Delete return items.ErrNotFound for an unknown id.
type itemStore interface {
	List(ctx context.Context) ([]items.Item, error)
	Get(ctx context.Context, id int64) (items.Item, error)
	Create(ctx context.Context, name, kind string) (items.Item, error)
	Delete(ctx context.Context, id int64) error
}

// NewRouter serves the API. checks are the dependency pings behind /readyz.
func NewRouter(cfg Config, s itemStore, ready *atomic.Bool, checks []checker) http.Handler {
	mux := http.NewServeMux()

	// Liveness: "is the process alive?" If this fails, k8s restarts the container.
	// Keep it dependency-free: restarting this pod can't fix a down database.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Readiness: "can it take traffic?" If this fails, k8s removes the pod from the Service.
	// Pings postgres, redis and rabbitmq (see health.go).
	mux.HandleFunc("GET /readyz", ReadinessHandler(ready, checks))

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

	mux.HandleFunc("GET /api/items", func(w http.ResponseWriter, r *http.Request) {
		items, err := s.List(r.Context())
		if err != nil {
			writeStoreError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	})

	mux.HandleFunc("POST /api/items", func(w http.ResponseWriter, r *http.Request) {
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

	mux.HandleFunc("GET /api/items/{id}", func(w http.ResponseWriter, r *http.Request) {
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

	mux.HandleFunc("DELETE /api/items/{id}", func(w http.ResponseWriter, r *http.Request) {
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
