package dedupe

import (
	"testing"
	"time"
)

func TestIntsSmall(t *testing.T) {
	got := Ints([]int{3, 1, 3, 2, 1})
	want := []int{3, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("Ints = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Ints = %v, want %v", got, want)
		}
	}
}

func TestIntsLarge(t *testing.T) {
	const n = 200000
	xs := make([]int, n)
	for i := range xs {
		xs[i] = i / 2
	}
	done := make(chan int, 1)
	go func() { done <- len(Ints(xs)) }()
	select {
	case got := <-done:
		if got != n/2 {
			t.Fatalf("len = %d, want %d", got, n/2)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ints did not finish within 2s on large input")
	}
}
