package chunker

import (
	"math"
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

func TestChunkBadSize(t *testing.T) {
	for _, size := range []int{0, -1} {
		if got := Chunk([]int{1, 2}, size); got != nil {
			t.Fatalf("size %d: got %v", size, got)
		}
	}
}

func TestChunkHugeSize(t *testing.T) {
	got := Chunk([]int{1, 2, 3}, math.MaxInt)
	if !reflect.DeepEqual(got, [][]int{{1, 2, 3}}) {
		t.Fatalf("got %v", got)
	}
}

func TestChunkAppendDoesNotClobber(t *testing.T) {
	items := []int{1, 2, 3, 4}
	got := Chunk(items, 2)
	_ = append(got[0], 99)
	if items[2] != 3 {
		t.Fatalf("append clobbered input: %v", items)
	}
}
