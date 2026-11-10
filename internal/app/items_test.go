package app

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vandelay-labs/importer/internal/api"
	"github.com/vandelay-labs/importer/internal/ratelimit"
	"github.com/vandelay-labs/importer/internal/telemetry"
)

// Nothing listens on port 1, and nothing connects until a request or the
// relay needs it, so these tests run without Postgres, RabbitMQ or Redis.
const (
	unreachableDB    = "postgres://u:p@127.0.0.1:1/db?connect_timeout=1"
	unreachableMQ    = "amqp://u:p@127.0.0.1:1/"
	unreachableRedis = "redis://127.0.0.1:1/0"
)

func testConfig() api.Config {
	cfg := api.LoadConfig()
	cfg.DatabaseURL, cfg.RabbitMQURL, cfg.RedisURL = unreachableDB, unreachableMQ, ""
	cfg.CriticalDeps = nil
	cfg.RateLimit, cfg.RateLimitWindow = 10, time.Minute
	return cfg
}

var testSvc = telemetry.Service{Name: "importer-test", Version: "test"}

// wrapCalls is a rateLimit builder that records whether it was used.
func wrapCalls(called *bool) func(ratelimit.Allower) func(http.Handler) http.Handler {
	return func(l ratelimit.Allower) func(http.Handler) http.Handler {
		*called = true
		return api.RateLimit(l)
	}
}

func newService(t *testing.T, cfg api.Config, rateLimit func(ratelimit.Allower) func(http.Handler) http.Handler) *ItemsService {
	t.Helper()
	s, err := NewItemsService(context.Background(), cfg, testSvc, rateLimit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestNewItemsServiceRequiresDatabaseAndRabbitMQ(t *testing.T) {
	cfg := testConfig()
	cfg.DatabaseURL, cfg.RabbitMQURL = "", ""
	_, err := NewItemsService(context.Background(), cfg, testSvc, api.RateLimit)
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL, RABBITMQ_URL") {
		t.Fatalf("got %v, want both settings named", err)
	}
}

func TestNewItemsServiceInvalidURLs(t *testing.T) {
	for name, mutate := range map[string]func(*api.Config){
		"database": func(c *api.Config) { c.DatabaseURL = "postgres://u:p@host:notaport/db" },
		"redis":    func(c *api.Config) { c.RedisURL = "not-a-redis-url" },
		"critical": func(c *api.Config) { c.CriticalDeps = []string{"postgress"} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			mutate(&cfg)
			if _, err := NewItemsService(context.Background(), cfg, testSvc, api.RateLimit); err == nil {
				t.Fatal("got nil, want an error")
			}
		})
	}
}

func TestNewItemsServiceRateLimit(t *testing.T) {
	tests := []struct {
		name      string
		redisURL  string
		limit     int
		wantLimit bool
		wantRedis string // /readyz status for redis
	}{
		{"no redis", "", 10, false, "skipped"},
		{"limit 0", unreachableRedis, 0, false, "error"},
		{"on", unreachableRedis, 10, true, "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.RedisURL, cfg.RateLimit = tt.redisURL, tt.limit
			called := false
			s := newService(t, cfg, wrapCalls(&called))
			if got := s.Limit != nil; got != tt.wantLimit || called != tt.wantLimit {
				t.Errorf("Limit set: %v (builder called: %v), want %v", got, called, tt.wantLimit)
			}

			// Readiness is down until Serve, so flip it to see the checks.
			s.ready.Store(true)
			rec := httptest.NewRecorder()
			s.Readyz.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			var body struct {
				Checks map[string]struct{ Status string }
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if got := body.Checks["redis"].Status; got != tt.wantRedis {
				t.Errorf("redis check: got %q, want %q", got, tt.wantRedis)
			}
		})
	}
}

func TestItemsServiceNotReadyBeforeServe(t *testing.T) {
	s := newService(t, testConfig(), api.RateLimit)
	rec := httptest.NewRecorder()
	s.Readyz.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503 before Serve", rec.Code)
	}
}

func TestItemsServiceServe(t *testing.T) {
	cfg := testConfig()
	_, cfg.Port, _ = net.SplitHostPort(freeAddr(t))
	_, cfg.MetricsPort, _ = net.SplitHostPort(freeAddr(t))
	s := newService(t, cfg, api.RateLimit)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.Readyz.ServeHTTP(w, r) // the router would mount it on /readyz
	})
	metrics := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "metrics") })
	telemetryFlushed := false
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- s.Serve(ctx, handler, metrics, func(context.Context) error { telemetryFlushed = true; return nil })
	}()

	waitServing(t, "http://localhost:"+cfg.MetricsPort)
	res, err := http.Get("http://localhost:" + cfg.MetricsPort)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if string(b) != "metrics" {
		t.Errorf("metrics port: got %q, want the metrics handler", b)
	}
	waitServing(t, "http://localhost:"+cfg.Port)
	res, err = http.Get("http://localhost:" + cfg.Port)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	// No critical deps in testConfig, so serving means ready, even with
	// every dependency unreachable.
	if res.StatusCode != http.StatusOK {
		t.Errorf("readyz while serving: got %d, want 200", res.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("got %v, want nil", err)
		}
	case <-time.After(ShutdownTimeout + time.Second):
		t.Fatal("Serve didn't return after cancel")
	}
	if !telemetryFlushed {
		t.Error("telemetry wasn't flushed")
	}
}
