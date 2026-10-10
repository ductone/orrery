package bank

import (
	"errors"
	"testing"
)

func TestTransfer(t *testing.T) {
	b := New()
	b.Open("a", 100)
	b.Open("b", 0)
	if err := b.Transfer("a", "b", 30); err != nil {
		t.Fatal(err)
	}
	if a, _ := b.Balance("a"); a != 70 {
		t.Fatalf("a = %d", a)
	}
	if err := b.Transfer("a", "b", 500); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("err = %v", err)
	}
	if err := b.Transfer("a", "zzz", 10); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("err = %v", err)
	}
	if b.Total() != 100 {
		t.Fatalf("total = %d, want 100", b.Total())
	}
}

func TestParallelDeposits(t *testing.T) {
	b := New()
	b.Open("a", 0)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				b.Deposit("a", 1)
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if a, _ := b.Balance("a"); a != 800 {
		t.Fatalf("a = %d, want 800", a)
	}
}
