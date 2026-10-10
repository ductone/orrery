// Package cache is a tiny concurrency-safe in-memory cache.
package cache

import "sync"

// Cache maps string keys to arbitrary values.
type Cache struct {
	mu sync.Mutex
	m  map[string]any
}

// New returns an empty cache.
func New() *Cache { return &Cache{m: map[string]any{}} }

// Get returns the value stored under key.
func (c *Cache) Get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[key]
	return v, ok
}

// Set stores v under key.
func (c *Cache) Set(key string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = v
}

// Delete removes key.
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}

// Clear removes every entry.
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = map[string]any{}
}

// Len returns the number of cached entries.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
