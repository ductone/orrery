package wordset

import (
	"reflect"
	"testing"
)

func TestUnique(t *testing.T) {
	in := []string{"b", "a", "b", "A", "a"}
	got := Unique(in)
	if want := []string{"b", "a", "A"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if !reflect.DeepEqual(in, []string{"b", "a", "b", "A", "a"}) {
		t.Fatal("input modified")
	}
	if Unique(nil) != nil {
		t.Fatal("want nil for empty input")
	}
}
