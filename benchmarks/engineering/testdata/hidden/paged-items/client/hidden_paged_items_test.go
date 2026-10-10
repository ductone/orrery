package client

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"benchmark/paged-items/handler"
	"benchmark/paged-items/store"
)

func server(n int) (*Client, func()) {
	s := store.New()
	for i := 0; i < n; i++ {
		s.Add(store.Item{ID: fmt.Sprintf("k%03d", i), Name: "n"})
	}
	srv := httptest.NewServer(handler.New(s))
	return New(srv.URL, nil), srv.Close
}

func TestHiddenListAll(t *testing.T) {
	for _, tc := range []struct{ n, size int }{{0, 5}, {1, 5}, {10, 5}, {11, 5}, {7, 1}, {250, 100}, {30, 0}} {
		c, done := server(tc.n)
		all, err := c.ListAll(tc.size)
		done()
		if err != nil || len(all) != tc.n {
			t.Fatalf("n=%d size=%d: got %d items, err %v", tc.n, tc.size, len(all), err)
		}
		for i, it := range all {
			if it.ID != fmt.Sprintf("k%03d", i) {
				t.Fatalf("n=%d size=%d: item %d = %q", tc.n, tc.size, i, it.ID)
			}
		}
	}
}

func TestHiddenPage(t *testing.T) {
	c, done := server(5)
	defer done()
	items, next, err := c.Page("", 2)
	if err != nil || len(items) != 2 || next == "" {
		t.Fatalf("Page = %v %q %v", items, next, err)
	}
	items, next, err = c.Page(next, 3)
	if err != nil || len(items) != 3 || next != "" || items[0].ID != "k002" {
		t.Fatalf("Page = %v %q %v", items, next, err)
	}
}

func TestHiddenPageInvalidCursor(t *testing.T) {
	c, done := server(3)
	defer done()
	if _, _, err := c.Page("%%%", 2); err == nil {
		t.Fatal("expected error for invalid cursor")
	}
}
