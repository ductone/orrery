package chunker

import (
	"reflect"
	"testing"
)

func TestChunk(t *testing.T) {
	got := Chunk([]int{1, 2, 3, 4, 5}, 2)
	want := [][]int{{1, 2}, {3, 4}, {5}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if got := Chunk(nil, 3); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}
