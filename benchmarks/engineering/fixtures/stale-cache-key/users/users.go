// Package users reads user records through a cache.
package users

import (
	"errors"
	"strings"

	"benchmark/stale-cache-key/cache"
)

// ErrNotFound is returned when no user has the given email address.
var ErrNotFound = errors.New("users: not found")

// User is a directory entry.
type User struct {
	Email string
	Name  string
}

// Source loads users from the system of record.
type Source interface {
	Load(email string) (User, error)
}

// Service serves reads, caching successful loads.
type Service struct {
	cache *cache.Cache
	src   Source
}

// NewService returns a Service reading through c.
func NewService(c *cache.Cache, src Source) *Service {
	return &Service{cache: c, src: src}
}

// canonical identifies a mailbox: surrounding space and case are ignored, and
// a "+tag" suffix on the local part does not change who the user is.
func canonical(email string) string {
	e := strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndex(e, "@")
	if at < 0 {
		return e
	}
	local, domain := e[:at], e[at:]
	if plus := strings.Index(local, "+"); plus >= 0 {
		local = local[:plus]
	}
	return local + domain
}

func cacheKey(email string) string { return "user:" + canonical(email) }

// Get returns the user for email, consulting the cache first.
func (s *Service) Get(email string) (User, error) {
	key := cacheKey(email)
	if v, ok := s.cache.Get(key); ok {
		return v.(User), nil
	}
	u, err := s.src.Load(email)
	if err != nil {
		return User{}, err
	}
	s.cache.Set(key, u)
	return u, nil
}
