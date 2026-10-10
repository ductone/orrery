package tagindex

import (
	"reflect"
	"testing"
)

func TestAddAndIDs(t *testing.T) {
	ix := New()
	ix.Add("go", "1")
	ix.Add("go", "2")
	if got := ix.IDs("go"); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Fatalf("got %v", got)
	}
	if got := ix.IDs("rust"); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestZeroValue(t *testing.T) {
	var ix Index
	ix.Add("a", "x")
	if got := ix.IDs("a"); !reflect.DeepEqual(got, []string{"x"}) {
		t.Fatalf("got %v", got)
	}
}

func TestMerge(t *testing.T) {
	a, b := New(), New()
	a.Add("go", "1")
	b.Add("go", "2")
	b.Add("rust", "3")
	a.Merge(b)
	if got := a.IDs("go"); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Fatalf("go: %v", got)
	}
	if got := a.IDs("rust"); !reflect.DeepEqual(got, []string{"3"}) {
		t.Fatalf("rust: %v", got)
	}
	var zero Index
	zero.Merge(b)
	if got := zero.IDs("rust"); !reflect.DeepEqual(got, []string{"3"}) {
		t.Fatalf("zero: %v", got)
	}
}
