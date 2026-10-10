package audit

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

var errBoom = errors.New("boom")

// scriptedSink records every event it is offered and fails on chosen calls.
type scriptedSink struct {
	failOn map[int]bool // 1-based call numbers that fail
	calls  int
	got    []Event
}

func (s *scriptedSink) Write(e Event) error {
	s.calls++
	if s.failOn[s.calls] {
		return errBoom
	}
	s.got = append(s.got, e)
	return nil
}

func TestHiddenPartialCountsSkipsAndSeq(t *testing.T) {
	s := &scriptedSink{failOn: map[int]bool{3: true}}
	l := NewLogger(s)
	err := l.Log("a", "", "b", "", "c", "d")
	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *PartialError", err)
	}
	if pe.Written != 2 || !errors.Is(err, errBoom) {
		t.Fatalf("Written = %d, err = %v; want 2 (empties are not counted) wrapping boom", pe.Written, err)
	}
	if l.Seq() != 2 {
		t.Fatalf("Seq = %d, want 2", l.Seq())
	}
	if s.calls != 3 {
		t.Fatalf("sink called %d times; Log must stop at the first error and skip empties", s.calls)
	}
	// Retry: the failed message gets the next number, no gap.
	if err := l.Log("c", "d"); err != nil {
		t.Fatal(err)
	}
	want := []Event{{1, "a"}, {2, "b"}, {3, "c"}, {4, "d"}}
	if len(s.got) != 4 {
		t.Fatalf("got %v", s.got)
	}
	for i := range want {
		if s.got[i] != want[i] {
			t.Fatalf("got %v, want %v", s.got, want)
		}
	}
}

func TestHiddenFirstEventFails(t *testing.T) {
	s := &scriptedSink{failOn: map[int]bool{1: true}}
	l := NewLogger(s)
	err := l.Log("x")
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Written != 0 || l.Seq() != 0 {
		t.Fatalf("err = %v seq = %d", err, l.Seq())
	}
}

func TestHiddenNoMessagesNoSinkCalls(t *testing.T) {
	s := &scriptedSink{}
	l := NewLogger(s)
	if err := l.Log(); err != nil {
		t.Fatal(err)
	}
	if err := l.Log("", ""); err != nil {
		t.Fatal(err)
	}
	if s.calls != 0 || l.Seq() != 0 {
		t.Fatalf("calls = %d seq = %d", s.calls, l.Seq())
	}
}

func TestHiddenMemSinkStillWorks(t *testing.T) {
	var sink Sink = &MemSink{Cap: 1}
	l := NewLogger(sink)
	err := l.Log("a", "b")
	if !errors.Is(err, ErrFull) {
		t.Fatalf("err = %v", err)
	}
}

func TestHiddenWriterSink(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(NewWriterSink(&buf))
	if err := l.Log("one", "", "two"); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "1\tone\n2\ttwo\n" {
		t.Fatalf("output = %q", got)
	}
}

type flakyWriter struct {
	writes int
	failOn int
	buf    strings.Builder
}

func (f *flakyWriter) Write(p []byte) (int, error) {
	f.writes++
	if f.writes == f.failOn {
		return 0, errBoom
	}
	return f.buf.Write(p)
}

func TestHiddenWriterSinkErrorSemantics(t *testing.T) {
	fw := &flakyWriter{failOn: 3}
	l := NewLogger(NewWriterSink(fw))
	err := l.Log("a", "b", "c", "d")
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Written != 2 || !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if fw.writes != 3 {
		t.Fatalf("writer called %d times, want one Write per event and stop at first error", fw.writes)
	}
	if l.Seq() != 2 {
		t.Fatalf("seq = %d", l.Seq())
	}
	if err := l.Log("c"); err != nil {
		t.Fatal(err)
	}
	if got := fw.buf.String(); got != "1\ta\n2\tb\n3\tc\n" {
		t.Fatalf("output = %q", got)
	}
}
