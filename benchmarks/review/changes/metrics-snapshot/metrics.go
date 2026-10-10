// Package metrics holds named counters safe for concurrent use.
package metrics

import "sync"

// Counters is a set of named counters.
type Counters struct {
	mu sync.Mutex
	m  map[string]int
}

// New returns an empty Counters.
func New() *Counters { return &Counters{m: map[string]int{}} }

// Inc adds one to the named counter.
func (c *Counters) Inc(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[name]++
}

// Get returns the current value of the named counter.
func (c *Counters) Get(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[name]
}

// Snapshot returns a copy of all counters at a point in time.
func (c *Counters) Snapshot() map[string]int {
	out := make(map[string]int, len(c.m))
	for k, v := range c.m {
		out[k] = v
	}
	return out
}
