package memo

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetCaches(t *testing.T) {
	calls := 0
	boom := errors.New("boom")
	m := New(func(k string) (int, error) {
		calls++
		if k == "bad" {
			return 0, boom
		}
		return len(k), nil
	})
	for i := 0; i < 3; i++ {
		if v, err := m.Get("abc"); v != 3 || err != nil {
			t.Fatal(v, err)
		}
		if _, err := m.Get("bad"); err != boom {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestGetConcurrent(t *testing.T) {
	var calls atomic.Int32
	m := New(func(k string) (int, error) {
		calls.Add(1)
		time.Sleep(5 * time.Millisecond)
		return len(k), nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, err := m.Get("key"); v != 3 || err != nil {
				t.Error(v, err)
			}
		}()
	}
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
}
