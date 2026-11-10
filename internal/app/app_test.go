package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequire(t *testing.T) {
	if err := Require(Setting{"A", "x"}, Setting{"B", "y"}); err != nil {
		t.Errorf("all set: got %v, want nil", err)
	}
	if err := Require(); err != nil {
		t.Errorf("none required: got %v, want nil", err)
	}
	err := Require(Setting{"DATABASE_URL", ""}, Setting{"REDIS_URL", "redis://x"}, Setting{"RABBITMQ_URL", ""})
	if err == nil || err.Error() != "required settings missing: DATABASE_URL, RABBITMQ_URL" {
		t.Errorf("two missing: got %v, want both named, in order", err)
	}
}

// freeAddr returns a localhost address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// waitServing polls url until it answers, so the test doesn't race the listener.
func waitServing(t *testing.T, url string) {
	t.Helper()
	for range 100 {
		if res, err := http.Get(url); err == nil {
			_ = res.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never answered", url)
}

func ok(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }

func TestRunServesUntilCancelled(t *testing.T) {
	addrA, addrB := freeAddr(t), freeAddr(t)
	servers := []*http.Server{
		{Addr: addrA, Handler: http.HandlerFunc(ok)},
		{Addr: addrB, Handler: http.HandlerFunc(ok)},
	}
	var ready atomic.Bool
	var steps []string
	step := func(name string) func(context.Context) error {
		return func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Errorf("%s: cleanup context has no deadline", name)
			}
			steps = append(steps, name)
			return errors.New(name + " failed") // logged; the next step still runs
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, &ready, servers, step("relay"), step("telemetry")) }()

	waitServing(t, "http://"+addrA)
	waitServing(t, "http://"+addrB)
	if !ready.Load() {
		t.Error("not ready while serving")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("got %v, want nil after a signal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return after cancel")
	}
	if ready.Load() {
		t.Error("still ready after shutdown")
	}
	if len(steps) != 2 || steps[0] != "relay" || steps[1] != "telemetry" {
		t.Errorf("cleanup: got %v, want [relay telemetry]", steps)
	}
	if _, err := http.Get("http://" + addrA); err == nil {
		t.Error("server still answering after Run returned")
	}
}

func TestRunDrainsInFlightRequests(t *testing.T) {
	addr := freeAddr(t)
	started, release := make(chan struct{}), make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", ok)
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "finished")
	})
	var ready atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, &ready, []*http.Server{{Addr: addr, Handler: mux}}) }()
	waitServing(t, "http://"+addr+"/ping")

	type result struct {
		body string
		err  error
	}
	slow := make(chan result, 1)
	go func() {
		res, err := http.Get("http://" + addr + "/slow")
		if err != nil {
			slow <- result{err: err}
			return
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		slow <- result{body: string(b)}
	}()
	<-started

	cancel()
	// Not ready as soon as shutdown starts, while the request is still running.
	deadline := time.Now().Add(2 * time.Second)
	for ready.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ready.Load() {
		t.Fatal("still ready after the signal")
	}
	select {
	case <-done:
		t.Fatal("Run returned with a request in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if r := <-slow; r.err != nil || r.body != "finished" {
		t.Errorf("in-flight request: got %q, %v, want it to finish", r.body, r.err)
	}
	if err := <-done; err != nil {
		t.Errorf("got %v, want nil", err)
	}
}

func TestRunReturnsServerError(t *testing.T) {
	// Something else holds the port, so ListenAndServe fails.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	var ready atomic.Bool
	cleaned := false
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), &ready, []*http.Server{{Addr: l.Addr().String()}},
			func(context.Context) error { cleaned = true; return nil })
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("got nil, want the listen error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return after its server failed")
	}
	if ready.Load() {
		t.Error("still ready after a server failed")
	}
	if !cleaned {
		t.Error("cleanup didn't run after a server failed")
	}
}
