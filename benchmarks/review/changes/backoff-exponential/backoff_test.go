package backoff

import (
	"testing"
	"time"
)

func TestConstant(t *testing.T) {
	f := Constant(time.Second)
	if f(0) != time.Second || f(9) != time.Second {
		t.Fatal("constant schedule changed")
	}
}

func TestExponential(t *testing.T) {
	f := Exponential(100*time.Millisecond, 5*time.Second)
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	for i, w := range want {
		if got := f(i); got != w {
			t.Fatalf("attempt %d: got %v want %v", i, got, w)
		}
	}
	if got := f(10); got != 5*time.Second {
		t.Fatalf("cap: got %v", got)
	}
}
