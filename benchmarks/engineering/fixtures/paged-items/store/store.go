// Package store keeps items in memory.
package store

import (
	"sort"
	"sync"
)

// Item is a stored record.
type Item struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Store is a concurrency-safe item store.
type Store struct {
	mu    sync.RWMutex
	items map[string]Item
}

// New returns an empty Store.
func New() *Store { return &Store{items: map[string]Item{}} }

// Add inserts or replaces an item.
func (s *Store) Add(it Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[it.ID] = it
}

// Delete removes the item with the given id and reports whether it existed.
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.items[id]
	delete(s.items, id)
	return ok
}

// Get returns the item with the given id.
func (s *Store) Get(id string) (Item, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, ok := s.items[id]
	return it, ok
}

// All returns every item ordered by ID.
func (s *Store) All() []Item {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Item, 0, len(s.items))
	for _, it := range s.items {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
