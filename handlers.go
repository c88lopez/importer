package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
)

func newRouter(cfg config, s *store, ready *atomic.Bool, checks []checker) http.Handler {
	mux := http.NewServeMux()

	// Liveness: "is the process alive?" If this fails, k8s restarts the container.
	// Keep it dependency-free: restarting this pod can't fix a down database.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Readiness: "can it take traffic?" If this fails, k8s removes the pod from the Service.
	// Pings postgres, redis and rabbitmq (see health.go).
	mux.HandleFunc("GET /readyz", readinessHandler(ready, checks))

	// Info: shows which pod answered. Hit it repeatedly to watch the Service load-balance.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		hostname, _ := os.Hostname()
		writeJSON(w, http.StatusOK, map[string]string{
			"message": cfg.Message,
			"version": version,
			"env":     cfg.Env,
			"pod":     hostname,
		})
	})

	mux.HandleFunc("GET /api/items", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.list())
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
		writeJSON(w, http.StatusCreated, s.create(body.Name, body.Kind))
	})

	mux.HandleFunc("GET /api/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		it, ok := s.get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "item not found")
			return
		}
		writeJSON(w, http.StatusOK, it)
	})

	mux.HandleFunc("DELETE /api/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !s.delete(r.PathValue("id")) {
			writeError(w, http.StatusNotFound, "item not found")
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
