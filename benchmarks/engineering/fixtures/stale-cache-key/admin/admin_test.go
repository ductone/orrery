package admin

import (
	"testing"

	"benchmark/stale-cache-key/cache"
	"benchmark/stale-cache-key/users"
)

func TestRenameInvalidates(t *testing.T) {
	c := cache.New()
	src := users.NewMemSource()
	src.Put(users.User{Email: "carol@example.com", Name: "Carol"})
	svc := users.NewService(c, src)
	if _, err := svc.Get("carol@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := New(c, src).Rename("carol@example.com", "Caroline"); err != nil {
		t.Fatal(err)
	}
	u, err := svc.Get("carol@example.com")
	if err != nil || u.Name != "Caroline" {
		t.Fatalf("Get = %v, %v; want Caroline", u, err)
	}
}
