package events

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// reference is the straightforward quadratic specification.
func reference(evs []Event) []Event {
	var out []Event
	for _, e := range evs {
		if e.Key == "" {
			continue
		}
		found := false
		for i := range out {
			if out[i].Key == e.Key {
				if e.Version >= out[i].Version {
					out[i] = e
				}
				found = true
				break
			}
		}
		if !found {
			out = append(out, e)
		}
	}
	return out
}

func TestHiddenTiesAndOrder(t *testing.T) {
	in := []Event{
		{"b", 1, "b1"},
		{"a", 5, "a5-first"},
		{"c", 2, "c2"},
		{"a", 5, "a5-last"}, // tie: later wins, slot stays after b
		{"b", 0, "b0"},      // lower version loses
		{"c", 3, "c3"},
		{"", 100, "no key"},
	}
	want := []Event{{"b", 1, "b1"}, {"a", 5, "a5-last"}, {"c", 3, "c3"}}
	got := Latest(in)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestHiddenNegativeVersionsAndNil(t *testing.T) {
	if got := Latest([]Event{{"", 1, "x"}}); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
	in := []Event{{"a", -5, "1"}, {"a", -7, "2"}, {"a", -5, "3"}}
	got := Latest(in)
	if len(got) != 1 || got[0].Value != "3" {
		t.Fatalf("got %v", got)
	}
}

func TestHiddenInputNotModified(t *testing.T) {
	in := []Event{{"a", 1, "x"}, {"a", 2, "y"}, {"b", 1, "z"}}
	cp := append([]Event(nil), in...)
	Latest(in)
	if fmt.Sprint(in) != fmt.Sprint(cp) {
		t.Fatalf("input modified: %v", in)
	}
}

func TestHiddenMatchesReference(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for round := 0; round < 200; round++ {
		n := r.Intn(60)
		in := make([]Event, n)
		for i := range in {
			k := ""
			if r.Intn(10) > 0 {
				k = fmt.Sprint("k", r.Intn(8))
			}
			in[i] = Event{k, r.Intn(4), fmt.Sprint(i)}
		}
		got, want := Latest(in), reference(in)
		if len(got) == 0 && len(want) == 0 {
			if got != nil {
				t.Fatalf("empty result must be nil")
			}
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("input %v\ngot  %v\nwant %v", in, got, want)
		}
	}
}

func TestHiddenLargeFast(t *testing.T) {
	const n = 300000
	r := rand.New(rand.NewSource(2))
	in := make([]Event, n)
	for i := range in {
		in[i] = Event{fmt.Sprint("k", r.Intn(150000)), r.Intn(3), fmt.Sprint(i)}
	}
	done := make(chan []Event, 1)
	go func() { done <- Latest(in) }()
	select {
	case got := <-done:
		// Spot-check ordering: first-appearance order of keys.
		seen := map[string]bool{}
		var order []string
		for _, e := range in {
			if !seen[e.Key] {
				seen[e.Key] = true
				order = append(order, e.Key)
			}
		}
		if len(got) != len(order) {
			t.Fatalf("len = %d, want %d", len(got), len(order))
		}
		for i := range order {
			if got[i].Key != order[i] {
				t.Fatalf("position %d: key %q, want %q", i, got[i].Key, order[i])
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Latest did not finish within 3s on 300k events")
	}
}
