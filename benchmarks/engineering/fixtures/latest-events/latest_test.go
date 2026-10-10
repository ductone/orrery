package events

import (
	"fmt"
	"testing"
	"time"
)

func TestLatestBasic(t *testing.T) {
	got := Latest([]Event{{"a", 1, "x"}, {"b", 1, "y"}, {"a", 2, "z"}, {"", 9, "skip"}})
	want := []Event{{"a", 2, "z"}, {"b", 1, "y"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestLatestEmpty(t *testing.T) {
	if got := Latest(nil); got != nil {
		t.Fatalf("got %v", got)
	}
}

func TestLatestLarge(t *testing.T) {
	const n = 100000
	evs := make([]Event, n)
	for i := range evs {
		evs[i] = Event{Key: fmt.Sprint("k", i), Version: 1}
	}
	done := make(chan int, 1)
	go func() { done <- len(Latest(evs)) }()
	select {
	case got := <-done:
		if got != n {
			t.Fatalf("len = %d, want %d", got, n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Latest did not finish within 2s")
	}
}
