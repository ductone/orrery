package pager

import (
	"reflect"
	"testing"
)

func TestPage(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}
	if got := Page(items, 1, 2); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("page 1: %v", got)
	}
	if got := Page(items, 3, 2); !reflect.DeepEqual(got, []string{"e"}) {
		t.Fatalf("page 3: %v", got)
	}
	if got := Page(items, 4, 2); len(got) != 0 {
		t.Fatalf("page 4: %v", got)
	}
	if got := Page(items, 1, 0); got != nil {
		t.Fatalf("size 0: %v", got)
	}
}
