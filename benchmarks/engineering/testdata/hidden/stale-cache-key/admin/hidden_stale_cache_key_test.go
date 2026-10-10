package admin_test

import (
	"errors"
	"testing"

	"benchmark/stale-cache-key/admin"
	"benchmark/stale-cache-key/cache"
	"benchmark/stale-cache-key/users"
)

func setup(t *testing.T) (*cache.Cache, *users.MemSource, *users.Service, *admin.Admin) {
	t.Helper()
	c := cache.New()
	src := users.NewMemSource()
	src.Put(users.User{Email: "alice@example.com", Name: "Alice"})
	src.Put(users.User{Email: "dave@example.com", Name: "Dave"})
	return c, src, users.NewService(c, src), admin.New(c, src)
}

func TestHiddenRenameSpellings(t *testing.T) {
	spellings := []string{
		"alice@example.com",
		"Alice@Example.COM",
		"  alice@example.com\t",
		"alice+news@example.com",
		" Alice+News@Example.COM ",
	}
	for _, sp := range spellings {
		t.Run(sp, func(t *testing.T) {
			_, _, svc, adm := setup(t)
			if u, _ := svc.Get("alice@example.com"); u.Name != "Alice" {
				t.Fatalf("warm Get = %v", u)
			}
			if err := adm.Rename(sp, "Alicia"); err != nil {
				t.Fatal(err)
			}
			for _, reader := range []string{"alice@example.com", "ALICE@example.com ", "alice+x@example.com"} {
				u, err := svc.Get(reader)
				if err != nil || u.Name != "Alicia" {
					t.Fatalf("Get(%q) = %v, %v; want Alicia", reader, u, err)
				}
			}
		})
	}
}

func TestHiddenCacheStillUsed(t *testing.T) {
	c, src, svc, adm := setup(t)
	svc.Get("alice@example.com")
	svc.Get("dave@example.com")
	base := src.Loads()
	if err := adm.Rename(" ALICE+a@example.com", "Alicia"); err != nil {
		t.Fatal(err)
	}
	// Other users stay cached.
	if u, _ := svc.Get("dave@example.com"); u.Name != "Dave" {
		t.Fatalf("dave = %v", u)
	}
	if src.Loads() != base {
		t.Fatalf("renaming alice evicted other entries: loads %d -> %d", base, src.Loads())
	}
	// Alice is reloaded exactly once, then served from cache.
	for i := 0; i < 3; i++ {
		if u, _ := svc.Get("alice@example.com"); u.Name != "Alicia" {
			t.Fatalf("alice = %v", u)
		}
	}
	if src.Loads() != base+1 {
		t.Fatalf("loads = %d, want %d", src.Loads(), base+1)
	}
	if c.Len() != 2 {
		t.Fatalf("cache len = %d, want 2", c.Len())
	}
}

func TestHiddenRenameMissing(t *testing.T) {
	_, _, svc, adm := setup(t)
	svc.Get("alice@example.com")
	if err := adm.Rename("ghost@example.com", "Ghost"); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if u, _ := svc.Get("alice@example.com"); u.Name != "Alice" {
		t.Fatalf("alice = %v", u)
	}
}
