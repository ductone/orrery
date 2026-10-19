package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ductone/orrery/storage"
)

// RetentionReport describes one MaintainStorage pass.
type RetentionReport struct {
	// Archived and Deleted list the sessions the pass archived and deleted,
	// in order. Deleting a top-level session also deletes its children,
	// which are listed too.
	Archived []string
	Deleted  []string
	// BytesBefore and BytesAfter are the store's usage around the pass. They
	// are zero when the store cannot measure itself (storage.Archiver).
	BytesBefore, BytesAfter int64
	// OverBudget is true when a budget is configured and BytesAfter is still
	// above it: what remains is protected or not yet extracted into memory.
	OverBudget bool
}

func (e *Engine) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

// MaintainStorage runs one retention pass under cfg.Retention and returns what
// it did. In order it: deletes worker and review sessions that finished more
// than WorkerAfter ago; archives top-level sessions that finished more than
// ArchiveAfter ago; deletes sessions archived more than DeleteAfter ago (when
// set); then, if MaxBytes is set and the store is over it, archives the
// oldest finished sessions regardless of age and then deletes the oldest
// archived ones until usage is under HighWaterFraction of MaxBytes.
//
// A session's age is measured from its UpdatedAt. Sessions that are running,
// active in this engine, or waiting for a person's answer (input_required)
// are never touched, nor is a top-level session with such a child, nor one
// whose memory extraction has not caught up. Without storage.Archiver only
// worker sessions are deleted. Failing to process one session does not stop
// the pass; the errors are joined and returned with the report.
func (e *Engine) MaintainStorage(ctx context.Context) (RetentionReport, error) {
	var rep RetentionReport
	var errs []error
	cfg, _, _, _, _ := e.runtimeSnapshot()
	rc := cfg.Retention
	now := e.clock()
	arch, canArchive := e.store.(storage.Archiver)
	if canArchive {
		n, err := arch.UsageBytes(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("measure store: %w", err))
			canArchive = false
		}
		rep.BytesBefore = n
	}
	remove := func(s storage.Session, children []storage.Session) {
		// Children first: a failure then leaves a parent that a later pass
		// retries, never an orphan nobody can find from its parent.
		for _, c := range children {
			if err := e.store.DeleteSession(ctx, c.ID); err != nil {
				errs = append(errs, fmt.Errorf("delete session %s: %w", c.ID, err))
				return
			}
			rep.Deleted = append(rep.Deleted, c.ID)
		}
		if err := e.store.DeleteSession(ctx, s.ID); err != nil {
			errs = append(errs, fmt.Errorf("delete session %s: %w", s.ID, err))
			return
		}
		rep.Deleted = append(rep.Deleted, s.ID)
		if s.ParentSessionID == "" {
			e.removeScratch(s.ID)
		}
	}

	// 1. Expired worker and review sessions. Orphans (parent already gone)
	// are included.
	children, err := e.store.ChildSessions(ctx)
	if err != nil {
		return rep, fmt.Errorf("list child sessions: %w", err)
	}
	for _, c := range children {
		if e.sessionFinished(c) && now.Sub(c.UpdatedAt) > rc.WorkerWindow() {
			remove(c, nil)
		}
	}

	if !canArchive {
		return rep, errors.Join(errs...)
	}
	archive := func(s storage.Session) bool {
		if err := arch.ArchiveSession(ctx, s.ID); err != nil {
			errs = append(errs, fmt.Errorf("archive session %s: %w", s.ID, err))
			return false
		}
		rep.Archived = append(rep.Archived, s.ID)
		return true
	}
	// sessions returns the top-level sessions, oldest first, each with its
	// surviving children, and whether it may be touched.
	type candidate struct {
		session  storage.Session
		children []storage.Session
		ok       bool
	}
	candidates := func() ([]candidate, error) {
		top, err := e.store.Sessions(ctx)
		if err != nil {
			return nil, err
		}
		kids, err := e.store.ChildSessions(ctx)
		if err != nil {
			return nil, err
		}
		var out []candidate
		for _, s := range top {
			c := candidate{session: s, ok: e.sessionFinished(s)}
			for _, k := range kids {
				if k.ParentSessionID == s.ID {
					c.children = append(c.children, k)
					c.ok = c.ok && e.sessionFinished(k)
				}
			}
			if c.ok && s.ArchivedAt.IsZero() {
				pending, err := e.memoryPending(ctx, s.ID)
				if err != nil {
					errs = append(errs, fmt.Errorf("check memory for %s: %w", s.ID, err))
				}
				c.ok = err == nil && !pending
			}
			out = append(out, c)
		}
		slices.SortStableFunc(out, func(a, b candidate) int { return a.session.UpdatedAt.Compare(b.session.UpdatedAt) })
		return out, nil
	}

	// 2 and 3. Age-based archiving and deletion.
	all, err := candidates()
	if err != nil {
		return rep, errors.Join(append(errs, fmt.Errorf("list sessions: %w", err))...)
	}
	for _, c := range all {
		switch {
		case !c.ok:
		case c.session.ArchivedAt.IsZero():
			if now.Sub(c.session.UpdatedAt) > rc.ArchiveWindow() {
				archive(c.session)
			}
		case rc.DeleteAfter > 0 && now.Sub(c.session.ArchivedAt) > rc.DeleteAfter:
			remove(c.session, c.children)
		}
	}

	// 4. Size budget.
	if rc.MaxBytes > 0 {
		target := int64(float64(rc.MaxBytes) * rc.HighWater())
		usage := func() int64 {
			n, err := arch.UsageBytes(ctx)
			if err != nil {
				errs = append(errs, fmt.Errorf("measure store: %w", err))
				return 0
			}
			return n
		}
		if n := usage(); n > rc.MaxBytes {
			all, err = candidates()
			if err != nil {
				return rep, errors.Join(append(errs, fmt.Errorf("list sessions: %w", err))...)
			}
			// Archive oldest first; if that is not enough, delete the
			// oldest archived sessions (all is ordered oldest first).
			for _, c := range all {
				if n <= target {
					break
				}
				if c.ok && c.session.ArchivedAt.IsZero() && archive(c.session) {
					n = usage()
				}
			}
			for _, c := range all {
				if n <= target {
					break
				}
				// Re-read: the loop above may have just archived it.
				if !c.ok || !e.archived(ctx, c.session.ID) {
					continue
				}
				remove(c.session, c.children)
				n = usage()
			}
		}
		rep.BytesAfter = usage()
		rep.OverBudget = rep.BytesAfter > rc.MaxBytes
		return rep, errors.Join(errs...)
	}
	if n, err := arch.UsageBytes(ctx); err == nil {
		rep.BytesAfter = n
	}
	return rep, errors.Join(errs...)
}

func (e *Engine) archived(ctx context.Context, sid string) bool {
	s, err := e.store.Session(ctx, sid)
	return err == nil && !s.ArchivedAt.IsZero()
}

// sessionFinished reports whether retention may touch the session: it is not
// running, not active in this engine, and not waiting for a person to answer
// (input_required stays protected however old it is, since the answer would
// resume it).
func (e *Engine) sessionFinished(s storage.Session) bool {
	if s.Status == "running" || s.Status == "input_required" || !e.sessionIdle(s.ID) {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[s.ID] == 0
}

// memoryPending reports whether memory extraction still has work on the
// session. Extraction only ever consumes events up to a passing
// "session.terminal", and leaves the watermark short of anything after it, or
// of a session that did not pass, so "the watermark reached the last event"
// would hold back every such session forever. Pending therefore means what
// extractMemory itself would still act on: a passing terminal event beyond
// the watermark. Child sessions are never extracted (only top-level runs
// schedule extraction), so this is only asked of top-level sessions.
func (e *Engine) memoryPending(ctx context.Context, sid string) (bool, error) {
	watermark, err := e.store.MemoryWatermark(ctx, sid)
	if err != nil {
		return false, err
	}
	events, err := e.store.EventsAfter(ctx, sid, watermark)
	if err != nil {
		return false, err
	}
	for _, ev := range events {
		if ev.Type != "session.terminal" {
			continue
		}
		var terminal struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(ev.Data, &terminal) == nil && terminal.Status == "pass" {
			return true, nil
		}
	}
	return false, nil
}
