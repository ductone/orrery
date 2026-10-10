// Package memo caches the results of an expensive function by key.
package memo

import "sync"

type entry struct {
	once sync.Once
	v    int
	err  error
}

// Memo caches fn's results, including errors. It is safe for concurrent use.
type Memo struct {
	fn func(string) (int, error)
	mu sync.Mutex
	m  map[string]*entry
}

// New returns a Memo for fn.
func New(fn func(string) (int, error)) *Memo {
	return &Memo{fn: fn, m: map[string]*entry{}}
}

// Get returns fn(key), calling fn at most once per key even when many
// goroutines ask for the same key at the same time. Callers for different
// keys do not wait for each other's fn.
func (m *Memo) Get(key string) (int, error) {
	m.mu.Lock()
	e, ok := m.m[key]
	if !ok {
		e = &entry{}
		m.m[key] = e
	}
	m.mu.Unlock()

	e.once.Do(func() { e.v, e.err = m.fn(key) })
	return e.v, e.err
}
