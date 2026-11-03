package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	var ready atomic.Bool
	ready.Store(true)
	srv := httptest.NewServer(newRouter(loadConfig(), newMemStore(), &ready, nil))
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
	for _, body := range []string{`{`, `{"name":"","kind":"import"}`, `{"name":"x","kind":"smuggle"}`} {
		res, _ := http.Post(srv.URL+"/api/items", "application/json", strings.NewReader(body))
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("body %s: got %d, want 400", body, res.StatusCode)
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
