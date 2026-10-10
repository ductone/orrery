package users

import (
	"testing"

	"benchmark/stale-cache-key/cache"
)

func TestGetCaches(t *testing.T) {
	src := NewMemSource()
	src.Put(User{Email: "bob@example.com", Name: "Bob"})
	svc := NewService(cache.New(), src)
	for i := 0; i < 3; i++ {
		u, err := svc.Get("Bob@Example.com ")
		if err != nil || u.Name != "Bob" {
			t.Fatalf("Get = %v, %v", u, err)
		}
	}
	if src.Loads() != 1 {
		t.Fatalf("loads = %d, want 1", src.Loads())
	}
}

func TestGetNotFound(t *testing.T) {
	svc := NewService(cache.New(), NewMemSource())
	if _, err := svc.Get("nobody@example.com"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
