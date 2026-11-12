package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vandelay-labs/importer/internal/items"
	"github.com/vandelay-labs/importer/internal/ratelimit"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	var ready atomic.Bool
	ready.Store(true)
	srv := httptest.NewServer(NewRouter(LoadConfig(), newMemStore(), ReadinessHandler(&ready, nil), nil))
	t.Cleanup(srv.Close)
	return srv
}

func TestItemsCRUD(t *testing.T) {
	srv := newTestServer(t)

	res, err := http.Post(srv.URL+"/api/items", "application/json", strings.NewReader(`{"name":"latex","kind":"import"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create: got %d, want 201", res.StatusCode)
	}

	res, _ = http.Get(srv.URL + "/api/items/1")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("get: got %d, want 200", res.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/items/1", nil)
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204", res.StatusCode)
	}

	res, _ = http.Get(srv.URL + "/api/items/1")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: got %d, want 404", res.StatusCode)
	}
}

func TestCreateValidation(t *testing.T) {
	srv := newTestServer(t)
	for _, body := range []string{
		`{`,
		`{"name":"","kind":"import"}`,
		`{"name":"x","kind":"smuggle"}`,
		`{"name":"x","kind":"import","priority":"urgent"}`,
	} {
		res, _ := http.Post(srv.URL+"/api/items", "application/json", strings.NewReader(body))
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("body %s: got %d, want 400", body, res.StatusCode)
		}
	}
}

// Priority is optional and defaults to normal.
func TestCreatePriority(t *testing.T) {
	srv := newTestServer(t)
	for body, want := range map[string]string{
		`{"name":"latex","kind":"import"}`:                   "normal",
		`{"name":"latex","kind":"import","priority":""}`:     "normal",
		`{"name":"latex","kind":"import","priority":"low"}`:  "low",
		`{"name":"latex","kind":"import","priority":"high"}`: "high",
	} {
		res, err := http.Post(srv.URL+"/api/items", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var it items.Item
		err = json.NewDecoder(res.Body).Decode(&it)
		res.Body.Close()
		if res.StatusCode != http.StatusCreated || err != nil {
			t.Fatalf("body %s: got %d, %v; want 201", body, res.StatusCode, err)
		}
		if it.Priority != want {
			t.Errorf("body %s: priority = %q, want %q", body, it.Priority, want)
		}
	}
}

func TestProbes(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		res, _ := http.Get(srv.URL + path)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: got %d, want 200", path, res.StatusCode)
		}
	}
}

// allowN is a rate limiter that allows the first n requests, from any IP.
type allowN struct {
	mu sync.Mutex
	n  int
}

func (a *allowN) Allow(context.Context, string) (ratelimit.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n--
	return ratelimit.Result{Allowed: a.n >= 0, Limit: 2, Remaining: max(a.n, 0), Reset: time.Minute}, nil
}

func TestRateLimit(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	srv := httptest.NewServer(NewRouter(LoadConfig(), newMemStore(), ReadinessHandler(&ready, nil), RateLimit(&allowN{n: 2})))
	t.Cleanup(srv.Close)

	for i := range 2 {
		if res, _ := http.Get(srv.URL + "/api/items"); res.StatusCode != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200", i+1, res.StatusCode)
		}
	}
	res, err := http.Get(srv.URL + "/api/items")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusTooManyRequests || strings.TrimSpace(string(body)) != `{"error":"rate limit exceeded"}` {
		t.Fatalf("over the limit: got %d %s, want 429 with the error body", res.StatusCode, body)
	}
	if res.Header.Get("Retry-After") != "60" || res.Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers: %v", res.Header)
	}

	// The probes and the info page aren't limited.
	for _, path := range []string{"/healthz", "/readyz", "/"} {
		if res, _ := http.Get(srv.URL + path); res.StatusCode != http.StatusOK {
			t.Errorf("%s: got %d, want 200", path, res.StatusCode)
		}
	}
}
