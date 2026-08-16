package retryboundary

import (
	"errors"
	"testing"
)

func TestRetryHonorsMaximumAttempts(t *testing.T) {
	calls := 0
	want := errors.New("still failing")
	err := Retry(3, func() error { calls++; return want })
	if !errors.Is(err, want) || calls != 3 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestRetryStopsAfterSuccess(t *testing.T) {
	calls := 0
	err := Retry(4, func() error {
		calls++
		if calls == 2 {
			return nil
		}
		return errors.New("again")
	})
	if err != nil || calls != 2 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestRetryWithNoAttemptsDoesNotInvoke(t *testing.T) {
	calls := 0
	err := Retry(0, func() error { calls++; return nil })
	if !errors.Is(err, ErrNoAttempts) || calls != 0 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}
