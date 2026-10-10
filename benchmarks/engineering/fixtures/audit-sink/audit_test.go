package audit

import (
	"errors"
	"testing"
)

func TestLogOrder(t *testing.T) {
	s := &MemSink{}
	l := NewLogger(s)
	if err := l.Log("a", "b"); err != nil {
		t.Fatal(err)
	}
	if len(s.Events) != 2 || s.Events[1] != (Event{Seq: 2, Msg: "b"}) {
		t.Fatalf("events = %v", s.Events)
	}
}

func TestLogPartial(t *testing.T) {
	s := &MemSink{Cap: 2}
	l := NewLogger(s)
	err := l.Log("a", "b", "c")
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Written != 2 || !errors.Is(err, ErrFull) {
		t.Fatalf("err = %v", err)
	}
	if l.Seq() != 2 {
		t.Fatalf("seq = %d", l.Seq())
	}
}
