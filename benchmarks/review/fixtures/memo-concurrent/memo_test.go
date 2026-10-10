package memo

import (
	"errors"
	"testing"
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
