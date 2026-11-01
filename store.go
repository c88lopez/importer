package main

import (
	"sort"
	"strconv"
	"sync"
	"time"
)

type Item struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"` // "import" or "export"
	CreatedAt time.Time `json:"createdAt"`
}

// store is in-memory on purpose: each pod has its own copy, which makes it
// easy to see that replicas don't share state (a good k8s lesson).
type store struct {
	mu     sync.RWMutex
	items  map[string]Item
	nextID int
}

func newStore() *store {
	return &store{items: make(map[string]Item), nextID: 1}
}

func (s *store) list() []Item {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Item, 0, len(s.items))
	for _, it := range s.items {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (s *store) get(id string) (Item, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, ok := s.items[id]
	return it, ok
}

func (s *store) create(name, kind string) Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := Item{ID: strconv.Itoa(s.nextID), Name: name, Kind: kind, CreatedAt: time.Now().UTC()}
	s.items[it.ID] = it
	s.nextID++
	return it
}

func (s *store) delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[id]; !ok {
		return false
	}
	delete(s.items, id)
	return true
}
