package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeLimiter allows limit requests per key, forever, and records the keys
// it was asked about. Set err to make every call fail.
type fakeLimiter struct {
	mu     sync.Mutex
	limit  int
	counts map[string]int
	err    error
}

func (f *fakeLimiter) Allow(_ context.Context, key string) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return Result{}, f.err
	}
	if f.counts == nil {
		f.counts = map[string]int{}
	}
	f.counts[key]++
	n := f.counts[key]
	return Result{Allowed: n <= f.limit, Limit: f.limit, Remaining: max(f.limit-n, 0), Reset: 1500 * time.Millisecond}, nil
}

func newLimited(l Allower) http.Handler {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	return Middleware(l, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})(ok)
}

func serve(h http.Handler, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMiddlewareLimits(t *testing.T) {
	h := newLimited(&fakeLimiter{limit: 2})

	for i, wantRemaining := range []string{"1", "0"} {
		rec := serve(h, "10.0.0.1:1234")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200", i+1, rec.Code)
		}
		if got := rec.Header().Get("RateLimit-Remaining"); got != wantRemaining {
			t.Errorf("request %d: RateLimit-Remaining %q, want %q", i+1, got, wantRemaining)
		}
		if got := rec.Header().Get("Retry-After"); got != "" {
			t.Errorf("request %d: Retry-After %q on an allowed request", i+1, got)
		}
	}

	rec := serve(h, "10.0.0.1:5678") // same IP, another port
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over the limit: got %d, want 429", rec.Code)
	}
	for header, want := range map[string]string{
		"RateLimit-Limit":     "2",
		"RateLimit-Remaining": "0",
		"RateLimit-Reset":     "2", // 1.5s, rounded up
		"Retry-After":         "2",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s: got %q, want %q", header, got, want)
		}
	}

	if rec := serve(h, "10.0.0.2:1234"); rec.Code != http.StatusOK {
		t.Errorf("another IP: got %d, want 200", rec.Code)
	}
}

func TestMiddlewareFailsOpen(t *testing.T) {
	h := newLimited(&fakeLimiter{limit: 1, err: errors.New("redis: connection refused")})
	for range 3 {
		rec := serve(h, "10.0.0.1:1234")
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d, want 200 while the limiter fails", rec.Code)
		}
		if got := rec.Header().Get("RateLimit-Limit"); got != "" {
			t.Errorf("RateLimit-Limit %q without a decision", got)
		}
	}
}

func TestClientIP(t *testing.T) {
	for remoteAddr, want := range map[string]string{
		"10.0.0.1:1234":      "10.0.0.1",
		"[2001:db8::1]:1234": "2001:db8::1",
		"[::1]:80":           "::1",
		"not-an-address":     "not-an-address",
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remoteAddr
		req.Header.Set("X-Forwarded-For", "1.2.3.4") // spoofable, so ignored
		if got := clientIP(req); got != want {
			t.Errorf("%q: got %q, want %q", remoteAddr, got, want)
		}
	}
}

func TestCeilSeconds(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                        "1",
		time.Millisecond:         "1",
		time.Second:              "1",
		time.Second + 1:          "2",
		59500 * time.Millisecond: "60",
	} {
		if got := ceilSeconds(d); got != want {
			t.Errorf("%s: got %s, want %s", d, got, want)
		}
	}
}
