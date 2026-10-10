// Package admin applies changes to users and keeps the read cache fresh.
package admin

import (
	"strings"

	"benchmark/stale-cache-key/cache"
)

// Store is the system of record that admin writes to.
type Store interface {
	Rename(email, name string) error
}

// Admin changes user records.
type Admin struct {
	cache *cache.Cache
	store Store
}

// New returns an Admin that invalidates entries in c after writing to store.
func New(c *cache.Cache, store Store) *Admin {
	return &Admin{cache: c, store: store}
}

// Rename sets the display name of the user with the given email.
func (a *Admin) Rename(email, name string) error {
	if err := a.store.Rename(email, name); err != nil {
		return err
	}
	a.cache.Delete("user:" + strings.ToLower(email))
	return nil
}
