package gql

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/99designs/gqlgen/client"

	"github.com/vandelay-labs/importer/internal/items"
	"github.com/vandelay-labs/importer/internal/ratelimit"
)

// memStore is an in-memory itemStore, so these tests run without a database.
// items.Store's own tests cover the SQL. Set err to make every call fail.
type memStore struct {
	mu     sync.Mutex
	items  []items.Item
	nextID int64
	err    error
}

func (s *memStore) List(ctx context.Context) ([]items.Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]items.Item(nil), s.items...), s.err
}

func (s *memStore) Get(ctx context.Context, id int64) (items.Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return items.Item{}, s.err
	}
	for _, it := range s.items {
		if it.ID == id {
			return it, nil
		}
	}
	return items.Item{}, items.ErrNotFound
}

func (s *memStore) Create(ctx context.Context, name, kind, priority string) (items.Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return items.Item{}, s.err
	}
	s.nextID++
	it := items.Item{ID: s.nextID, Name: name, Kind: kind, Priority: priority, Status: "pending", CreatedAt: time.Now()}
	s.items = append(s.items, it)
	return it, nil
}

func (s *memStore) Delete(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	for i, it := range s.items {
		if it.ID == id {
			s.items = append(s.items[:i], s.items[i+1:]...)
			return nil
		}
	}
	return items.ErrNotFound
}

type item struct {
	ID          string
	Name        string
	Kind        string
	Priority    string
	Status      string
	ProcessedAt *string
}

func TestItemsCRUD(t *testing.T) {
	c := client.New(NewHandler(&memStore{}))

	var created struct{ CreateItem item }
	c.MustPost(`mutation { createItem(input: {name: "  latex  ", kind: IMPORT}) { id name kind priority status processedAt } }`, &created)
	want := item{ID: "1", Name: "latex", Kind: "IMPORT", Priority: "NORMAL", Status: "PENDING"}
	if created.CreateItem != want {
		t.Fatalf("createItem: got %+v, want %+v", created.CreateItem, want)
	}

	var list struct{ Items []item }
	c.MustPost(`{ items { id name } }`, &list)
	if len(list.Items) != 1 || list.Items[0].ID != "1" {
		t.Fatalf("items: got %+v", list.Items)
	}

	// IDs may be sent as strings or numbers.
	var one struct{ Item *item }
	c.MustPost(`query($id: ID!) { item(id: $id) { name } }`, &one, client.Var("id", 1))
	if one.Item == nil || one.Item.Name != "latex" {
		t.Fatalf("item: got %+v", one.Item)
	}

	var deleted struct{ DeleteItem bool }
	c.MustPost(`mutation { deleteItem(id: "1") }`, &deleted)
	if !deleted.DeleteItem {
		t.Fatal("deleteItem: got false for an existing item")
	}
	c.MustPost(`mutation { deleteItem(id: "1") }`, &deleted)
	if deleted.DeleteItem {
		t.Fatal("deleteItem: got true for a deleted item")
	}

	// A missing item is null, not an error.
	var gone struct{ Item *item }
	c.MustPost(`{ item(id: "1") { name } }`, &gone)
	if gone.Item != nil {
		t.Fatalf("item after delete: got %+v, want null", gone.Item)
	}
}

// Priority is optional: NORMAL when left out or null.
func TestCreatePriority(t *testing.T) {
	c := client.New(NewHandler(&memStore{}))
	for input, want := range map[string]string{
		`{name: "x", kind: IMPORT}`:                 "NORMAL",
		`{name: "x", kind: IMPORT, priority: null}`: "NORMAL",
		`{name: "x", kind: IMPORT, priority: LOW}`:  "LOW",
		`{name: "x", kind: IMPORT, priority: HIGH}`: "HIGH",
	} {
		var created struct{ CreateItem item }
		c.MustPost(`mutation { createItem(input: `+input+`) { priority } }`, &created)
		if created.CreateItem.Priority != want {
			t.Errorf("%s: priority = %q, want %q", input, created.CreateItem.Priority, want)
		}
	}
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name  string
		store *memStore
		query string
		want  string // in the error message
	}{
		{"blank name", &memStore{}, `mutation { createItem(input: {name: " ", kind: EXPORT}) { id } }`, "name is required"},
		{"unknown kind", &memStore{}, `mutation { createItem(input: {name: "x", kind: TRADE}) { id } }`, "TRADE"},
		{"unknown priority", &memStore{}, `mutation { createItem(input: {name: "x", kind: IMPORT, priority: URGENT}) { id } }`, "URGENT"},
		{"bad id", &memStore{}, `{ item(id: "abc") { id } }`, "is not a valid item id"},
		{"unknown field", &memStore{}, `{ items { price } }`, `Cannot query field`},
		// Database errors are hidden from clients, like the REST API's 500s.
		{"store error", &memStore{err: errors.New("connection refused")}, `{ items { id } }`, "internal error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp any
			err := client.New(NewHandler(tt.store)).Post(tt.query, &resp)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got error %v, want one containing %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "connection refused") {
				t.Fatalf("store error leaked to the client: %v", err)
			}
		})
	}
}

func TestComplexityLimit(t *testing.T) {
	// Aliases let one query ask for the same list many times over.
	var q strings.Builder
	q.WriteString("{")
	for i := range 30 {
		q.WriteString(" a" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ": items { id name kind status createdAt processedAt }")
	}
	q.WriteString(" }")
	var resp any
	err := client.New(NewHandler(&memStore{})).Post(q.String(), &resp)
	if err == nil || !strings.Contains(err.Error(), "complexity") {
		t.Fatalf("got %v, want a complexity error", err)
	}
}

func TestContentTypes(t *testing.T) {
	srv := httptest.NewServer(NewRouter(&memStore{}, "test", http.NotFoundHandler(), nil))
	t.Cleanup(srv.Close)

	tests := []struct {
		name, path, contentType, body string
		wantStatus                    int
		wantBody                      string
	}{
		{"json", "/graphql", "application/json", `{"query":"{ items { id } }"}`, http.StatusOK, `{"data":{"items":[]}}`},
		{"raw query", "/graphql", "application/graphql", `{ items { id } }`, http.StatusOK, `{"data":{"items":[]}}`},
		// Postman and others default to the bare host.
		{"json on /", "/", "application/json", `{"query":"{ items { id } }"}`, http.StatusOK, `{"data":{"items":[]}}`},
		// What curl -d sends without -H: refused, with a hint (and against CSRF).
		{"form", "/graphql", "application/x-www-form-urlencoded", `{"query":"{ items { id } }"}`, http.StatusUnsupportedMediaType, "Content-Type: application/json"},
		{"form on /", "/", "application/x-www-form-urlencoded", `{"query":"{ items { id } }"}`, http.StatusUnsupportedMediaType, "Content-Type: application/json"},
		{"none", "/graphql", "", `{"query":"{ items { id } }"}`, http.StatusUnsupportedMediaType, "Content-Type: application/json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+tt.path, strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)
			if res.StatusCode != tt.wantStatus || !strings.Contains(string(body), tt.wantBody) {
				t.Fatalf("got %d %s, want %d containing %q", res.StatusCode, body, tt.wantStatus, tt.wantBody)
			}
		})
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
	srv := httptest.NewServer(NewRouter(&memStore{}, "test", http.NotFoundHandler(), RateLimit(&allowN{n: 2})))
	t.Cleanup(srv.Close)
	post := func() *http.Response {
		t.Helper()
		res, err := http.Post(srv.URL+"/graphql", "application/json", strings.NewReader(`{"query":"{ items { id } }"}`))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	for i := range 2 {
		if res := post(); res.StatusCode != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200", i+1, res.StatusCode)
		}
	}
	res := post()
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), `"code":"RATE_LIMITED"`) {
		t.Fatalf("over the limit: got %d %s, want 429 with RATE_LIMITED", res.StatusCode, body)
	}
	if res.Header.Get("Retry-After") != "60" {
		t.Errorf("Retry-After: got %q, want 60", res.Header.Get("Retry-After"))
	}

	// GraphiQL and the probes aren't limited.
	for _, path := range []string{"/", "/healthz"} {
		if res, _ := http.Get(srv.URL + path); res.StatusCode != http.StatusOK {
			t.Errorf("%s: got %d, want 200", path, res.StatusCode)
		}
	}
}
