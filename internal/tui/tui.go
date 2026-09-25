package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ductone/orrey/internal/store"
)

// Options configures one TUI process. Exactly one session is in scope: an
// explicit SessionID, the session bound to Create's external identity, or a
// session created from the first message.
type Options struct {
	Backend   Backend
	SessionID string
	// Create supplies the workspace, budget, routing pin, and optional
	// external identity used for lookup and for creating the session.
	Create CreateRequest
	// Prompt is submitted as soon as the TUI starts, like a positional
	// message to other coding-agent CLIs.
	Prompt  string
	Version string
	// SquireDir and SquireTaskID enable the Squire harness contract (pid
	// sidecar, control socket, event journal). Both must be set.
	SquireDir    string
	SquireTaskID string
	Input        io.Reader
	Output       io.Writer
}

// Resolve returns the session the TUI is scoped to, or a zero Session when
// the session will be created from the first message.
func Resolve(ctx context.Context, b Backend, opts Options) (store.Session, error) {
	if opts.SessionID != "" {
		return b.Session(ctx, opts.SessionID)
	}
	if opts.Create.ExternalID != "" {
		s, err := b.Lookup(ctx, opts.Create.Integration, opts.Create.ExternalID, opts.Create.ExternalIncarnation)
		if err == nil || !errors.Is(err, ErrNotFound) {
			return s, err
		}
	}
	return store.Session{}, nil
}

func Run(ctx context.Context, opts Options) error {
	if opts.Backend == nil {
		return errors.New("tui: backend required")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lookup, stop := context.WithTimeout(ctx, 30*time.Second)
	session, err := Resolve(lookup, opts.Backend, opts)
	stop()
	if err != nil {
		return fmt.Errorf("resolve session: %w", err)
	}
	sessionID := session.ID
	m := newModel(ctx, opts, sessionID)
	m.sessionSnapshot = session
	progOpts := []tea.ProgramOption{tea.WithContext(ctx)}
	if opts.Input != nil {
		progOpts = append(progOpts, tea.WithInput(opts.Input))
	}
	if opts.Output != nil {
		progOpts = append(progOpts, tea.WithOutput(opts.Output))
	}
	p := tea.NewProgram(m, progOpts...)
	if opts.SquireDir != "" && opts.SquireTaskID != "" {
		sq, err := StartSquire(opts.SquireDir, opts.SquireTaskID, func(text string) error {
			reply := make(chan error, 1)
			p.Send(controlMsg{text: text, reply: reply})
			select {
			case err := <-reply:
				return err
			case <-time.After(2 * time.Minute):
				return errors.New("timed out waiting for the session to accept the prompt")
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err != nil {
			return err
		}
		defer sq.Close()
		if sessionID != "" {
			if err := sq.Bind(sessionID); err != nil {
				return err
			}
		}
		m.squire = sq
	}
	_, err = p.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}
