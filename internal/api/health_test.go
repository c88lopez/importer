package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestReadyz(t *testing.T) {
	ok := func(ctx context.Context) error { return nil }
	fail := func(ctx context.Context) error { return errors.New("connection refused") }

	tests := []struct {
		name       string
		checks     []checker
		wantCode   int
		wantStatus string
		want       map[string]string // dependency -> status
	}{
		{
			name:       "all healthy",
			checks:     []checker{{"postgres", true, ok}, {"redis", false, ok}, {"rabbitmq", false, ok}},
			wantCode:   http.StatusOK,
			wantStatus: "ready",
			want:       map[string]string{"postgres": "ok", "redis": "ok", "rabbitmq": "ok"},
		},
		{
			name:       "critical down",
			checks:     []checker{{"postgres", true, fail}, {"redis", false, ok}, {"rabbitmq", false, ok}},
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: "not ready",
			want:       map[string]string{"postgres": "error", "redis": "ok", "rabbitmq": "ok"},
		},
		{
			name:       "non-critical down is degraded but ready",
			checks:     []checker{{"postgres", true, ok}, {"redis", false, fail}, {"rabbitmq", false, ok}},
			wantCode:   http.StatusOK,
			wantStatus: "degraded",
			want:       map[string]string{"postgres": "ok", "redis": "error", "rabbitmq": "ok"},
		},
		{
			name:       "critical down wins over degraded",
			checks:     []checker{{"postgres", true, fail}, {"redis", false, fail}, {"rabbitmq", false, ok}},
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: "not ready",
			want:       map[string]string{"postgres": "error", "redis": "error", "rabbitmq": "ok"},
		},
		{
			name:       "unconfigured is skipped",
			checks:     []checker{{"postgres", true, nil}, {"redis", false, ok}, {"rabbitmq", false, nil}},
			wantCode:   http.StatusOK,
			wantStatus: "ready",
			want:       map[string]string{"postgres": "skipped", "redis": "ok", "rabbitmq": "skipped"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ready atomic.Bool
			ready.Store(true)
			rec := httptest.NewRecorder()
			ReadinessHandler(&ready, tt.checks)(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if rec.Code != tt.wantCode {
				t.Fatalf("got %d, want %d", rec.Code, tt.wantCode)
			}
			var body struct {
				Status string                 `json:"status"`
				Checks map[string]checkResult `json:"checks"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Status != tt.wantStatus {
				t.Errorf("status: got %q, want %q", body.Status, tt.wantStatus)
			}
			for dep, want := range tt.want {
				if got := body.Checks[dep].Status; got != want {
					t.Errorf("%s: got %q, want %q", dep, got, want)
				}
			}
		})
	}
}

func TestMarkCritical(t *testing.T) {
	checks := []checker{{name: "postgres"}, {name: "redis"}, {name: "rabbitmq"}}
	if err := markCritical(checks, []string{"redis"}); err != nil {
		t.Fatal(err)
	}
	if checks[0].critical || !checks[1].critical || checks[2].critical {
		t.Errorf("only redis should be critical: %+v", checks)
	}
	if err := markCritical(checks, []string{"postgress"}); err == nil {
		t.Error("expected an error for an unknown dependency name")
	}
}

func TestReadyzShuttingDown(t *testing.T) {
	var ready atomic.Bool // false: the pod received SIGTERM
	called := false
	checks := []checker{{"postgres", true, func(ctx context.Context) error { called = true; return nil }}}

	rec := httptest.NewRecorder()
	ReadinessHandler(&ready, checks)(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rec.Code)
	}
	if called {
		t.Error("dependency checks should not run while shutting down")
	}
}

func TestHealthzIgnoresDependencies(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	down := []checker{{"postgres", true, func(ctx context.Context) error { return errors.New("down") }}}
	srv := httptest.NewServer(NewRouter(LoadConfig(), newMemStore(), &ready, down, nil))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/healthz: got %d, want 200 even with postgres down", res.StatusCode)
	}
	res, _ = http.Get(srv.URL + "/readyz")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/readyz: got %d, want 503 with postgres down", res.StatusCode)
	}
}

func TestCriticalDepsDefault(t *testing.T) {
	t.Setenv("CRITICAL_DEPENDENCIES", "")
	if got := criticalDeps(); len(got) != 0 {
		t.Errorf("empty value: got %v, want none", got)
	}
	t.Setenv("CRITICAL_DEPENDENCIES", " redis , rabbitmq ")
	if got := criticalDeps(); len(got) != 2 || got[0] != "redis" || got[1] != "rabbitmq" {
		t.Errorf("list: got %v, want [redis rabbitmq]", got)
	}
}
