package handler

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"benchmark/paged-items/store"
)

type page struct {
	Items      []store.Item `json:"items"`
	NextCursor string       `json:"next_cursor"`
}

func get(t *testing.T, s *store.Store, target string) (int, string, page) {
	t.Helper()
	rec := httptest.NewRecorder()
	New(s).ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
	var p page
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("bad JSON %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, rec.Body.String(), p
}

func TestHiddenListEmptyIsArray(t *testing.T) {
	code, body, p := get(t, store.New(), "/items")
	if code != 200 || len(p.Items) != 0 || p.NextCursor != "" {
		t.Fatalf("code %d body %s", code, body)
	}
	if !strings.Contains(body, `"items":[]`) {
		t.Fatalf("empty page must encode items as [], got %s", body)
	}
}

func TestHiddenListPagesAndDefaults(t *testing.T) {
	s := store.New()
	for i := 0; i < 150; i++ {
		s.Add(store.Item{ID: fmt.Sprintf("k%03d", i)})
	}
	_, _, p := get(t, s, "/items")
	if len(p.Items) != 20 || p.NextCursor == "" {
		t.Fatalf("default limit: %d items, cursor %q", len(p.Items), p.NextCursor)
	}
	_, _, p = get(t, s, "/items?limit=1000")
	if len(p.Items) != 100 || p.NextCursor == "" {
		t.Fatalf("limit clamp: %d items, cursor %q", len(p.Items), p.NextCursor)
	}
	_, _, p2 := get(t, s, "/items?limit=100&cursor="+p.NextCursor)
	if len(p2.Items) != 50 || p2.NextCursor != "" || p2.Items[0].ID != "k100" {
		t.Fatalf("last page: %d items, first %q, cursor %q", len(p2.Items), p2.Items[0].ID, p2.NextCursor)
	}
}

func TestHiddenListBadInput(t *testing.T) {
	s := store.New()
	s.Add(store.Item{ID: "a"})
	for _, q := range []string{"cursor=%25%25%25", "cursor=not+a+cursor", "limit=0", "limit=-3", "limit=abc"} {
		if code, _, _ := get(t, s, "/items?"+q); code != 400 {
			t.Fatalf("%s: code = %d, want 400", q, code)
		}
	}
}

func TestHiddenSingleItemStillWorks(t *testing.T) {
	s := store.New()
	s.Add(store.Item{ID: "a", Name: "A"})
	if code, _, _ := get(t, s, "/items/a"); code != 200 {
		t.Fatalf("code = %d", code)
	}
	if code, _, _ := get(t, s, "/items/zzz"); code != 404 {
		t.Fatalf("code = %d", code)
	}
}
