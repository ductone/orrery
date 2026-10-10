package client

import (
	"net/http/httptest"
	"testing"

	"benchmark/paged-items/handler"
	"benchmark/paged-items/store"
)

func TestGet(t *testing.T) {
	s := store.New()
	s.Add(store.Item{ID: "x", Name: "X"})
	srv := httptest.NewServer(handler.New(s))
	defer srv.Close()
	c := New(srv.URL, nil)
	it, err := c.Get("x")
	if err != nil || it.Name != "X" {
		t.Fatalf("Get = %v, %v", it, err)
	}
	if _, err := c.Get("nope"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
