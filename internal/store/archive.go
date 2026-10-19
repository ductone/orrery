package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// ArchiveSession replaces everything recorded for a session except its row
// with one gzip-compressed JSON SessionArchive. Jobs, which other code reads
// for finished sessions, stay in the jobs table.
func (s *Store) ArchiveSession(ctx context.Context, sid string) error {
	sess, err := s.Session(ctx, sid)
	if err != nil {
		return err
	}
	if !sess.ArchivedAt.IsZero() {
		return nil
	}
	archive := SessionArchive{}
	if archive.Events, err = s.EventsAfter(ctx, sid, 0); err != nil {
		return err
	}
	if archive.Messages, err = s.Messages(ctx, sid); err != nil {
		return err
	}
	if archive.Todos, err = s.Todos(ctx, sid); err != nil {
		return err
	}
	if archive.WorkItems, err = s.WorkItems(ctx, sid); err != nil {
		return err
	}
	if archive.Continuation, err = s.Continuation(ctx, sid); err != nil {
		return err
	}
	if archive.PendingInputs, err = s.PendingInputs(ctx, sid); err != nil {
		return err
	}
	if archive.Checkpoints, err = s.Checkpoints(ctx, sid); err != nil {
		return err
	}
	now := time.Now().UTC()
	archive.ArchivedAt = now
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err = json.NewEncoder(zw).Encode(archive); err != nil {
		return err
	}
	if err = zw.Close(); err != nil {
		return err
	}
	stamp := now.Format(time.RFC3339Nano)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The session is finished, so nothing should have been written since the
	// reads above. Check anyway: deleting rows the archive lacks would lose
	// them.
	last := 0
	if n := len(archive.Events); n > 0 {
		last = archive.Events[n-1].Seq
	}
	var archived sql.NullString
	var seq int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM events WHERE session_id=?`, sid).Scan(&seq); err != nil {
		return err
	}
	if seq != last {
		return fmt.Errorf("archive session %s: events changed while archiving", sid)
	}
	if err = tx.QueryRowContext(ctx, `SELECT archived_at FROM sessions WHERE id=?`, sid).Scan(&archived); err != nil {
		return err
	}
	if archived.String != "" {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO session_archives(session_id,archived_at,data) VALUES(?,?,?)`, sid, stamp, buf.Bytes()); err != nil {
		return err
	}
	for _, table := range []string{"request_receipts", "events", "messages", "todos", "work_items", "continuation", "cache_ledger", "routing_records", "checkpoints", "pending_inputs", "queued_messages", "memory_watermarks"} {
		if _, err = tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE session_id=?`, sid); err != nil {
			return err
		}
	}
	// updated_at is left alone: it is when the session finished, which
	// retention measures age from.
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET archived_at=? WHERE id=?`, stamp, sid); err != nil {
		return err
	}
	return tx.Commit()
}

// ReadArchive returns the session's archive, or sql.ErrNoRows (storage.ErrNotFound).
func (s *Store) ReadArchive(ctx context.Context, sid string) (SessionArchive, error) {
	var data []byte
	if err := s.db.QueryRowContext(ctx, `SELECT data FROM session_archives WHERE session_id=?`, sid).Scan(&data); err != nil {
		return SessionArchive{}, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return SessionArchive{}, err
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return SessionArchive{}, err
	}
	var archive SessionArchive
	if err := json.Unmarshal(raw, &archive); err != nil {
		return SessionArchive{}, err
	}
	return archive, nil
}

// UsageBytes returns the size of the database's live pages,
// (page_count-freelist_count)*page_size. Freed pages are excluded because
// SQLite keeps them in the file for reuse instead of shrinking it, so the
// file size would not fall when sessions are archived or deleted (run VACUUM
// to give the space back to the filesystem). The write-ahead log is not
// counted; it stays small once checkpointed.
func (s *Store) UsageBytes(ctx context.Context) (int64, error) {
	var pages, free, size int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&size); err != nil {
		return 0, err
	}
	return (pages - free) * size, nil
}
