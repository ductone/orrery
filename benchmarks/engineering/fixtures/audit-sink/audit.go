// Package audit numbers and records audit messages.
package audit

import (
	"errors"
	"fmt"
)

// ErrFull is returned by a MemSink that has reached its capacity.
var ErrFull = errors.New("audit: sink full")

// Event is one numbered audit message.
type Event struct {
	Seq int
	Msg string
}

// MemSink stores events in memory. A Cap of 0 means unlimited.
type MemSink struct {
	Cap    int
	Events []Event
}

// Write appends e, or returns ErrFull.
func (m *MemSink) Write(e Event) error {
	if m.Cap > 0 && len(m.Events) >= m.Cap {
		return ErrFull
	}
	m.Events = append(m.Events, e)
	return nil
}

// PartialError reports that Log stopped early. Written is the number of
// messages that were recorded before the failure.
type PartialError struct {
	Written int
	Err     error
}

func (p *PartialError) Error() string {
	return fmt.Sprintf("audit: wrote %d events before failure: %v", p.Written, p.Err)
}

func (p *PartialError) Unwrap() error { return p.Err }

// Logger assigns sequence numbers, starting at 1, and writes events to a sink.
type Logger struct {
	sink *MemSink
	seq  int
}

// NewLogger returns a Logger writing to sink.
func NewLogger(sink *MemSink) *Logger { return &Logger{sink: sink} }

// Seq returns the sequence number of the last event written, or 0.
func (l *Logger) Seq() int { return l.seq }

// Log records msgs in order. Empty messages are skipped: they are neither
// written nor numbered. Log stops at the first sink error and returns a
// *PartialError; events written before it are kept. A failed event does not
// consume a sequence number, so a retry continues without gaps.
func (l *Logger) Log(msgs ...string) error {
	written := 0
	for _, m := range msgs {
		if m == "" {
			continue
		}
		e := Event{Seq: l.seq + 1, Msg: m}
		if err := l.sink.Write(e); err != nil {
			return &PartialError{Written: written, Err: err}
		}
		l.seq++
		written++
	}
	return nil
}
