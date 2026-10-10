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
