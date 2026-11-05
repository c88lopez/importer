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

func (s *memStore) Create(ctx context.Context, name, kind string) (items.Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return items.Item{}, s.err
	}
	s.nextID++
	it := items.Item{ID: s.nextID, Name: name, Kind: kind, Status: "pending", CreatedAt: time.Now()}
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
	Status      string
	ProcessedAt *string
}

func TestItemsCRUD(t *testing.T) {
	c := client.New(NewHandler(&memStore{}))

	var created struct{ CreateItem item }
	c.MustPost(`mutation { createItem(input: {name: "  latex  ", kind: IMPORT}) { id name kind status processedAt } }`, &created)
	want := item{ID: "1", Name: "latex", Kind: "IMPORT", Status: "PENDING"}
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

func TestErrors(t *testing.T) {
	tests := []struct {
		name  string
		store *memStore
		query string
		want  string // in the error message
	}{
		{"blank name", &memStore{}, `mutation { createItem(input: {name: " ", kind: EXPORT}) { id } }`, "name is required"},
		{"unknown kind", &memStore{}, `mutation { createItem(input: {name: "x", kind: TRADE}) { id } }`, "TRADE"},
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
	srv := httptest.NewServer(NewRouter(&memStore{}, "test", http.NotFoundHandler()))
	t.Cleanup(srv.Close)

	tests := []struct {
		name, contentType, body string
		wantStatus              int
		wantBody                string
	}{
		{"json", "application/json", `{"query":"{ items { id } }"}`, http.StatusOK, `{"data":{"items":[]}}`},
		{"raw query", "application/graphql", `{ items { id } }`, http.StatusOK, `{"data":{"items":[]}}`},
		// What curl -d sends without -H: refused, with a hint (and against CSRF).
		{"form", "application/x-www-form-urlencoded", `{"query":"{ items { id } }"}`, http.StatusUnsupportedMediaType, "Content-Type: application/json"},
		{"none", "", `{"query":"{ items { id } }"}`, http.StatusUnsupportedMediaType, "Content-Type: application/json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/graphql", strings.NewReader(tt.body))
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
