package store

import (
	"errors"
	"fmt"
	"testing"
)

func fill(n int) *Store {
	s := New()
	for i := 1; i <= n; i++ {
		s.Add(Item{ID: fmt.Sprintf("id%03d", i), Name: "n"})
	}
	return s
}

func ids(items []Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func TestHiddenPageEmpty(t *testing.T) {
	items, next, err := New().Page("", 10)
	if err != nil || len(items) != 0 || next != "" {
		t.Fatalf("Page = %v, %q, %v", items, next, err)
	}
}

func TestHiddenPageWalk(t *testing.T) {
	s := fill(5)
	items, next, err := s.Page("", 2)
	if err != nil || fmt.Sprint(ids(items)) != "[id001 id002]" || next == "" {
		t.Fatalf("p1 = %v %q %v", ids(items), next, err)
	}
	items, next, err = s.Page(next, 2)
	if err != nil || fmt.Sprint(ids(items)) != "[id003 id004]" || next == "" {
		t.Fatalf("p2 = %v %q %v", ids(items), next, err)
	}
	items, next, err = s.Page(next, 2)
	if err != nil || fmt.Sprint(ids(items)) != "[id005]" || next != "" {
		t.Fatalf("p3 = %v %q %v", ids(items), next, err)
	}
}

func TestHiddenPageExactFitHasNoCursor(t *testing.T) {
	s := fill(4)
	items, next, err := s.Page("", 4)
	if err != nil || len(items) != 4 || next != "" {
		t.Fatalf("Page = %v %q %v; a full final page must not return a cursor", ids(items), next, err)
	}
	_, next, _ = s.Page("", 2)
	items, next, err = s.Page(next, 2)
	if err != nil || len(items) != 2 || next != "" {
		t.Fatalf("second page = %v %q %v", ids(items), next, err)
	}
}

func TestHiddenPageInvalid(t *testing.T) {
	s := fill(3)
	for _, c := range []string{"!!!", "%%%", "not a cursor", "a b"} {
		if _, _, err := s.Page(c, 2); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("Page(%q) err = %v, want ErrInvalidCursor", c, err)
		}
	}
	for _, l := range []int{0, -1} {
		if _, _, err := s.Page("", l); !errors.Is(err, ErrInvalidLimit) {
			t.Fatalf("Page limit %d err = %v, want ErrInvalidLimit", l, err)
		}
	}
}

func TestHiddenPageStableUnderChange(t *testing.T) {
	s := fill(6)
	_, next, _ := s.Page("", 3) // after id003
	s.Delete("id003")            // the item the cursor points at disappears
	s.Add(Item{ID: "id000"})     // an earlier item appears
	s.Add(Item{ID: "id003a"})    // a later one appears
	items, next2, err := s.Page(next, 10)
	if err != nil || fmt.Sprint(ids(items)) != "[id003a id004 id005 id006]" || next2 != "" {
		t.Fatalf("Page = %v %q %v", ids(items), next2, err)
	}
}

func TestHiddenPageCursorOpaqueAfterRoundTrip(t *testing.T) {
	s := fill(3)
	_, next, _ := s.Page("", 1)
	if next == "id001" {
		t.Fatalf("cursor %q should be an opaque token, not the raw ID", next)
	}
}
