package users

import "sync"

// MemSource is an in-memory Source for tests and demos.
type MemSource struct {
	mu    sync.Mutex
	rows  map[string]User
	loads int
}

// NewMemSource returns an empty MemSource.
func NewMemSource() *MemSource { return &MemSource{rows: map[string]User{}} }

// Put inserts or replaces a user.
func (m *MemSource) Put(u User) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[canonical(u.Email)] = u
}

// Load implements Source and counts calls.
func (m *MemSource) Load(email string) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loads++
	u, ok := m.rows[canonical(email)]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

// Rename changes the display name of the user with the given email.
func (m *MemSource) Rename(email, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := canonical(email)
	u, ok := m.rows[k]
	if !ok {
		return ErrNotFound
	}
	u.Name = name
	m.rows[k] = u
	return nil
}

// Loads reports how many times Load has been called.
func (m *MemSource) Loads() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loads
}
