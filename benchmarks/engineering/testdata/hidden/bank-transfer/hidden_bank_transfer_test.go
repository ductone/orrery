package bank

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func within(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("timed out (deadlock?)")
	}
}

func TestHiddenTotalNeverDips(t *testing.T) {
	b := New()
	names := []string{"a", "b", "c", "d"}
	for _, n := range names {
		b.Open(n, 1000)
	}
	var stop atomic.Bool
	var bad atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			if tot := b.Total(); tot != 4000 {
				bad.Store(int64(tot))
			}
		}
	}()
	within(t, 60*time.Second, func() {
		var tw sync.WaitGroup
		for g := 0; g < 8; g++ {
			tw.Add(1)
			go func(g int) {
				defer tw.Done()
				for i := 0; i < 3000; i++ {
					b.Transfer(names[(g+i)%4], names[(g+i+1)%4], 1+i%7)
				}
			}(g)
		}
		tw.Wait()
	})
	stop.Store(true)
	wg.Wait()
	if v := bad.Load(); v != 0 {
		t.Fatalf("Total observed %d mid-transfer, want constant 4000", v)
	}
	if b.Total() != 4000 {
		t.Fatalf("final total %d", b.Total())
	}
}

func TestHiddenNoDeadlockOpposingTransfers(t *testing.T) {
	b := New()
	b.Open("x", 1_000_000)
	b.Open("y", 1_000_000)
	within(t, 30*time.Second, func() {
		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				for i := 0; i < 5000; i++ {
					b.Transfer("x", "y", 1)
				}
			}()
			go func() {
				defer wg.Done()
				for i := 0; i < 5000; i++ {
					b.Transfer("y", "x", 1)
				}
			}()
		}
		wg.Wait()
	})
	if b.Total() != 2_000_000 {
		t.Fatalf("total = %d", b.Total())
	}
}

func TestHiddenNeverOverdrawn(t *testing.T) {
	b := New()
	b.Open("src", 100)
	b.Open("dst", 0)
	var ok atomic.Int64
	within(t, 30*time.Second, func() {
		var wg sync.WaitGroup
		for g := 0; g < 50; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if b.Transfer("src", "dst", 10) == nil {
					ok.Add(1)
				}
			}()
		}
		wg.Wait()
	})
	if ok.Load() != 10 {
		t.Fatalf("%d transfers succeeded, want exactly 10", ok.Load())
	}
	if s, _ := b.Balance("src"); s != 0 {
		t.Fatalf("src = %d", s)
	}
	if d, _ := b.Balance("dst"); d != 100 {
		t.Fatalf("dst = %d", d)
	}
}

func TestHiddenFailedTransferInvisible(t *testing.T) {
	b := New()
	b.Open("a", 500)
	var stop atomic.Bool
	var bad atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			if v, _ := b.Balance("a"); v != 500 {
				bad.Store(int64(v))
			}
			if tot := b.Total(); tot != 500 {
				bad.Store(int64(tot))
			}
		}
	}()
	within(t, 30*time.Second, func() {
		for i := 0; i < 20000; i++ {
			if err := b.Transfer("a", "ghost", 5); !errors.Is(err, ErrNoAccount) {
				t.Errorf("err = %v", err)
				return
			}
		}
	})
	stop.Store(true)
	wg.Wait()
	if v := bad.Load(); v != 0 {
		t.Fatalf("observer saw %d while a transfer to a missing account was failing", v)
	}
}

func TestHiddenSelfTransfer(t *testing.T) {
	b := New()
	b.Open("a", 50)
	within(t, 5*time.Second, func() {
		if err := b.Transfer("a", "a", 20); err != nil {
			t.Errorf("err = %v", err)
		}
		if err := b.Transfer("a", "a", 80); !errors.Is(err, ErrInsufficient) {
			t.Errorf("err = %v, want ErrInsufficient", err)
		}
	})
	if a, _ := b.Balance("a"); a != 50 {
		t.Fatalf("a = %d, want 50", a)
	}
}
