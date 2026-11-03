package main

import (
	"context"
	"sort"
	"sync"
	"time"
)

// memStore is an in-memory itemStore for handler tests, so they run without
// a database. pgStore's own tests cover the SQL (see store_test.go).
type memStore struct {
	mu     sync.RWMutex
	items  map[int64]Item
	nextID int64
}

func newMemStore() *memStore {
	return &memStore{items: make(map[int64]Item), nextID: 1}
}

func (s *memStore) List(ctx context.Context) ([]Item, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Item, 0, len(s.items))
	for _, it := range s.items {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *memStore) Get(ctx context.Context, id int64) (Item, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, ok := s.items[id]
	if !ok {
		return Item{}, errNotFound
	}
	return it, nil
}

func (s *memStore) Create(ctx context.Context, name, kind string) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := Item{ID: s.nextID, Name: name, Kind: kind, CreatedAt: time.Now().UTC(), Status: "pending"}
	s.items[it.ID] = it
	s.nextID++
	return it, nil
}

func (s *memStore) Delete(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[id]; !ok {
		return errNotFound
	}
	delete(s.items, id)
	return nil
}
