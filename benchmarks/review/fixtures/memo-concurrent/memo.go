// Package memo caches the results of an expensive function by key.
package memo

type result struct {
	v   int
	err error
}

// Memo caches fn's results, including errors.
type Memo struct {
	fn func(string) (int, error)
	m  map[string]result
}

// New returns a Memo for fn.
func New(fn func(string) (int, error)) *Memo {
	return &Memo{fn: fn, m: map[string]result{}}
}

// Get returns fn(key), calling fn at most once per key.
func (m *Memo) Get(key string) (int, error) {
	if r, ok := m.m[key]; ok {
		return r.v, r.err
	}
	v, err := m.fn(key)
	m.m[key] = result{v, err}
	return v, err
}
