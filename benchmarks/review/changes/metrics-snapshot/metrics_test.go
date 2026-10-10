package metrics

import (
	"sync"
	"testing"
)

func TestInc(t *testing.T) {
	c := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.Inc("a") }()
	}
	wg.Wait()
	if got := c.Get("a"); got != 50 {
		t.Fatalf("got %d", got)
	}
	if got := c.Get("b"); got != 0 {
		t.Fatalf("got %d", got)
	}
}

func TestSnapshot(t *testing.T) {
	c := New()
	c.Inc("a")
	c.Inc("a")
	c.Inc("b")
	s := c.Snapshot()
	if s["a"] != 2 || s["b"] != 1 || len(s) != 2 {
		t.Fatalf("snapshot %v", s)
	}
	c.Inc("a")
	if s["a"] != 2 {
		t.Fatal("snapshot must be a copy")
	}
}
