package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ductone/orrery/storage"
	"github.com/google/uuid"
)

const memoryBaseSchema = `CREATE TABLE IF NOT EXISTS workspaces(workspace_id TEXT PRIMARY KEY, identity_key TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL, updated_at TEXT NOT NULL); CREATE TABLE IF NOT EXISTS memory_records(memory_id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL REFERENCES workspaces(workspace_id), scope TEXT NOT NULL CHECK(scope IN ('workspace','project','user')), kind TEXT NOT NULL, text TEXT NOT NULL, provenance TEXT NOT NULL, confidence REAL NOT NULL CHECK(confidence>=0 AND confidence<=1), status TEXT NOT NULL CHECK(status IN ('pending','active','superseded','expired','deleted')), evidence_refs TEXT NOT NULL DEFAULT '[]', created_at TEXT NOT NULL, updated_at TEXT NOT NULL, expires_at TEXT, superseded_by TEXT REFERENCES memory_records(memory_id)); CREATE INDEX IF NOT EXISTS memory_records_lookup ON memory_records(workspace_id,scope,status,updated_at);`

// EnsureWorkspace returns the stable workspace row for an application-resolved identity key.
func (s *Store) EnsureWorkspace(ctx context.Context, identityKey string) (Workspace, error) {
	if identityKey == "" {
		return Workspace{}, errors.New("workspace identity key is required")
	}
	now := time.Now().UTC()
	id := uuid.NewString()
	stamp := now.Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `INSERT INTO workspaces(workspace_id,identity_key,created_at,updated_at) VALUES(?,?,?,?) ON CONFLICT(identity_key) DO NOTHING`, id, identityKey, stamp, stamp)
	if err != nil {
		return Workspace{}, err
	}
	var w Workspace
	var created, updated string
	err = s.db.QueryRowContext(ctx, `SELECT workspace_id,identity_key,created_at,updated_at FROM workspaces WHERE identity_key=?`, identityKey).Scan(&w.ID, &w.IdentityKey, &created, &updated)
	if err != nil {
		return Workspace{}, err
	}
	w.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	w.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return w, nil
}

// CommitMemory inserts a durable record. New records are active unless status is explicitly pending.
func (s *Store) CommitMemory(ctx context.Context, x MemoryRecord) (MemoryRecord, error) {
	storage.ApplyMemoryDefaults(&x)
	if x.ID == "" {
		x.ID = uuid.NewString()
	}
	if err := storage.ValidateMemoryRecord(x); err != nil {
		return MemoryRecord{}, err
	}
	if err := s.validateMemoryReferences(ctx, x); err != nil {
		return MemoryRecord{}, err
	}
	now := time.Now().UTC()
	x.CreatedAt, x.UpdatedAt = now, now
	var expiry any
	if x.ExpiresAt != nil {
		expires := x.ExpiresAt.UTC()
		x.ExpiresAt = &expires
		expiry = expires.Format(time.RFC3339Nano)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO memory_records(memory_id,workspace_id,scope,kind,text,provenance,confidence,status,evidence_refs,created_at,updated_at,expires_at,superseded_by,session_id,origin,shadows) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, x.ID, x.WorkspaceID, storedMemoryScope(x.Scope), x.Kind, x.Text, x.Provenance, x.Confidence, x.Status, x.EvidenceRefs, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), expiry, nullIfEmpty(x.SupersededBy), x.SessionID, x.Origin, x.Shadows)
	if err != nil {
		return MemoryRecord{}, err
	}
	return x, nil
}

func (s *Store) UpdateMemory(ctx context.Context, x MemoryRecord) error {
	storage.ApplyMemoryDefaults(&x)
	if x.ID == "" {
		return errors.New("memory id is required")
	}
	if err := storage.ValidateMemoryRecord(x); err != nil {
		return err
	}
	if err := s.validateMemoryReferences(ctx, x); err != nil {
		return err
	}
	var expiry any
	if x.ExpiresAt != nil {
		expiry = x.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE memory_records SET scope=?,kind=?,text=?,provenance=?,confidence=?,status=?,evidence_refs=?,updated_at=?,expires_at=?,superseded_by=?,shadows=? WHERE memory_id=? AND workspace_id=? AND status!='deleted' AND status!='superseded'`, storedMemoryScope(x.Scope), x.Kind, x.Text, x.Provenance, x.Confidence, x.Status, x.EvidenceRefs, time.Now().UTC().Format(time.RFC3339Nano), expiry, nullIfEmpty(x.SupersededBy), x.Shadows, x.ID, x.WorkspaceID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SupersedeMemory marks an old record superseded only when both records belong to the same workspace.
func (s *Store) SupersedeMemory(ctx context.Context, workspaceID, oldID, newID string) error {
	if oldID == newID {
		return errors.New("a memory record cannot supersede itself")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldWorkspace, oldStatus, newWorkspace, newStatus string
	if err = tx.QueryRowContext(ctx, `SELECT workspace_id,status FROM memory_records WHERE memory_id=?`, oldID).Scan(&oldWorkspace, &oldStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrMemoryNotFound, oldID)
		}
		return err
	}
	if err = tx.QueryRowContext(ctx, `SELECT workspace_id,status FROM memory_records WHERE memory_id=?`, newID).Scan(&newWorkspace, &newStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrMemoryNotFound, newID)
		}
		return err
	}
	if oldWorkspace != workspaceID || newWorkspace != workspaceID {
		return errors.New("memory supersession must stay within one workspace")
	}
	if oldStatus == "deleted" || oldStatus == "superseded" {
		return fmt.Errorf("%w: source status is %s", ErrMemoryNotSupersedable, oldStatus)
	}
	if newStatus != "active" {
		return fmt.Errorf("%w: replacement status is %s", ErrMemoryNotSupersedable, newStatus)
	}
	res, err := tx.ExecContext(ctx, `UPDATE memory_records SET status='superseded',superseded_by=?,updated_at=? WHERE memory_id=? AND workspace_id=? AND status!='deleted' AND status!='superseded'`, newID, time.Now().UTC().Format(time.RFC3339Nano), oldID, workspaceID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrMemoryNotSupersedable
	}
	return tx.Commit()
}

// ForgetMemory creates a tombstone; forgotten records remain stored but are never listed as active.
func (s *Store) ForgetMemory(ctx context.Context, workspaceID, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE memory_records SET status='deleted',updated_at=? WHERE memory_id=? AND workspace_id=? AND status!='deleted'`, time.Now().UTC().Format(time.RFC3339Nano), id, workspaceID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ApplyMemoryMutationEvent commits a memory lifecycle change and its event in
// one transaction. If either write fails, neither is durable.
func (s *Store) ApplyMemoryMutationEvent(ctx context.Context, sid, turnID, eventType string, eventData any, m MemoryMutation) (MemoryRecord, error) {
	x := m.Record
	if m.Operation == "commit" || m.Operation == "correct" {
		storage.ApplyMemoryDefaults(&x)
		if x.ID == "" {
			x.ID = uuid.NewString()
		}
		if err := storage.ValidateMemoryRecord(x); err != nil {
			return MemoryRecord{}, err
		}
		if err := s.validateMemoryReferences(ctx, x); err != nil {
			return MemoryRecord{}, err
		}
	} else if m.Operation == "update" {
		storage.ApplyMemoryDefaults(&x)
		if x.ID == "" {
			return MemoryRecord{}, errors.New("memory id is required")
		}
		if err := storage.ValidateMemoryRecord(x); err != nil {
			return MemoryRecord{}, err
		}
		if err := s.validateMemoryReferences(ctx, x); err != nil {
			return MemoryRecord{}, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MemoryRecord{}, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	expiry := any(nil)
	if x.ExpiresAt != nil {
		expiry = x.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	switch m.Operation {
	case "commit":
		x.CreatedAt, x.UpdatedAt = now, now
		_, err = tx.ExecContext(ctx, `INSERT INTO memory_records(memory_id,workspace_id,scope,kind,text,provenance,confidence,status,evidence_refs,created_at,updated_at,expires_at,superseded_by,session_id,origin,shadows) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, x.ID, x.WorkspaceID, storedMemoryScope(x.Scope), x.Kind, x.Text, x.Provenance, x.Confidence, x.Status, x.EvidenceRefs, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), expiry, nullIfEmpty(x.SupersededBy), x.SessionID, x.Origin, x.Shadows)
	case "update":
		var res sql.Result
		res, err = tx.ExecContext(ctx, `UPDATE memory_records SET scope=?,kind=?,text=?,provenance=?,confidence=?,status=?,evidence_refs=?,updated_at=?,expires_at=?,superseded_by=?,shadows=? WHERE memory_id=? AND workspace_id=? AND status!='deleted' AND status!='superseded'`, storedMemoryScope(x.Scope), x.Kind, x.Text, x.Provenance, x.Confidence, x.Status, x.EvidenceRefs, now.Format(time.RFC3339Nano), expiry, nullIfEmpty(x.SupersededBy), x.Shadows, x.ID, x.WorkspaceID)
		if err == nil {
			var n int64
			n, err = res.RowsAffected()
			if err == nil && n == 0 {
				err = sql.ErrNoRows
			}
		}
	case "correct":
		x.CreatedAt, x.UpdatedAt = now, now
		_, err = tx.ExecContext(ctx, `INSERT INTO memory_records(memory_id,workspace_id,scope,kind,text,provenance,confidence,status,evidence_refs,created_at,updated_at,expires_at,superseded_by,session_id,origin,shadows) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, x.ID, x.WorkspaceID, storedMemoryScope(x.Scope), x.Kind, x.Text, x.Provenance, x.Confidence, x.Status, x.EvidenceRefs, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), expiry, nil, x.SessionID, x.Origin, x.Shadows)
		if err == nil {
			var res sql.Result
			res, err = tx.ExecContext(ctx, `UPDATE memory_records SET status='superseded',superseded_by=?,updated_at=? WHERE memory_id=? AND workspace_id=? AND status!='deleted' AND status!='superseded'`, x.ID, now.Format(time.RFC3339Nano), m.OldID, m.WorkspaceID)
			if err == nil {
				var n int64
				n, err = res.RowsAffected()
				if err == nil && n == 0 {
					err = ErrMemoryNotSupersedable
				}
			}
		}
	case "forget":
		var res sql.Result
		res, err = tx.ExecContext(ctx, `UPDATE memory_records SET status='deleted',updated_at=? WHERE memory_id=? AND workspace_id=? AND status!='deleted'`, now.Format(time.RFC3339Nano), m.MemoryID, m.WorkspaceID)
		if err == nil {
			var n int64
			n, err = res.RowsAffected()
			if err == nil && n == 0 {
				err = sql.ErrNoRows
			}
		}
	case "restore":
		var res sql.Result
		res, err = tx.ExecContext(ctx, `UPDATE memory_records SET status='active',superseded_by=NULL,expires_at=?,updated_at=? WHERE memory_id=? AND workspace_id=? AND status!='active'`, expiry, now.Format(time.RFC3339Nano), m.MemoryID, m.WorkspaceID)
		if err == nil {
			var n int64
			n, err = res.RowsAffected()
			if err == nil && n == 0 {
				err = sql.ErrNoRows
			}
		}
	default:
		err = errors.New("unknown memory mutation")
	}
	if data, ok := eventData.(map[string]any); ok {
		switch m.Operation {
		case "commit":
			data["id"] = x.ID
		case "correct":
			data["replacement_id"] = x.ID
		}
	}
	if err != nil {
		return MemoryRecord{}, err
	}
	if _, err = s.addEventTx(ctx, tx, sid, turnID, eventType, eventData); err != nil {
		return MemoryRecord{}, err
	}
	// Return the row as persisted rather than the caller's input, so every
	// operation (including forget, which is addressed only by ID) describes the
	// mutated record.
	targetID := x.ID
	if m.Operation == "forget" || m.Operation == "restore" {
		targetID = m.MemoryID
	}
	persisted, err := readMemoryTx(ctx, tx, targetID)
	if err != nil {
		return MemoryRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return MemoryRecord{}, err
	}
	return persisted, nil
}

// readMemoryTx loads one memory row inside an open transaction.
func readMemoryTx(ctx context.Context, tx *sql.Tx, id string) (MemoryRecord, error) {
	var x MemoryRecord
	var created, updated string
	var expires sql.NullString
	row := tx.QueryRowContext(ctx, `SELECT memory_id,workspace_id,scope,kind,text,provenance,confidence,status,evidence_refs,created_at,updated_at,expires_at,COALESCE(superseded_by,''),COALESCE(session_id,''),COALESCE(origin,''),COALESCE(shadows,'') FROM memory_records WHERE memory_id=?`, id)
	if err := row.Scan(&x.ID, &x.WorkspaceID, &x.Scope, &x.Kind, &x.Text, &x.Provenance, &x.Confidence, &x.Status, &x.EvidenceRefs, &created, &updated, &expires, &x.SupersededBy, &x.SessionID, &x.Origin, &x.Shadows); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MemoryRecord{}, fmt.Errorf("%w: %s", ErrMemoryNotFound, id)
		}
		return MemoryRecord{}, err
	}
	x.Scope = storage.NormalizeMemoryScope(x.Scope)
	var err error
	if x.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return MemoryRecord{}, fmt.Errorf("parse memory created_at: %w", err)
	}
	if x.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return MemoryRecord{}, fmt.Errorf("parse memory updated_at: %w", err)
	}
	if expires.Valid && expires.String != "" {
		expiry, parseErr := time.Parse(time.RFC3339Nano, expires.String)
		if parseErr != nil {
			return MemoryRecord{}, fmt.Errorf("parse memory expires_at: %w", parseErr)
		}
		x.ExpiresAt = &expiry
	}
	return x, nil
}

func (s *Store) ListMemory(ctx context.Context, f MemoryFilter) ([]MemoryRecord, error) {
	if f.WorkspaceID == "" {
		return nil, errors.New("workspace id is required")
	}
	// Persist expiry before filtering, including pending records. Terminal user
	// corrections and forget tombstones keep their status.
	if _, err := s.db.ExecContext(ctx, `UPDATE memory_records SET status='expired',updated_at=? WHERE workspace_id=? AND status IN ('active','pending') AND expires_at IS NOT NULL AND julianday(expires_at)<=julianday(?)`, time.Now().UTC().Format(time.RFC3339Nano), f.WorkspaceID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, err
	}
	query := `SELECT memory_id,workspace_id,scope,kind,text,provenance,confidence,status,evidence_refs,created_at,updated_at,expires_at,COALESCE(superseded_by,''),COALESCE(session_id,''),COALESCE(origin,''),COALESCE(shadows,'') FROM memory_records WHERE workspace_id=?`
	if !f.IncludeDeleted {
		query += ` AND status!='deleted'`
	}
	args := []any{f.WorkspaceID}
	if scopes := storage.MemoryFilterScopes(f); len(scopes) > 0 {
		query += ` AND scope IN (?` + strings.Repeat(`,?`, len(scopes)-1) + `)`
		for _, scope := range scopes {
			args = append(args, storedMemoryScope(scope))
		}
	}
	if f.Status != "" {
		query += ` AND status=?`
		args = append(args, f.Status)
	} else if !f.IncludeExpired {
		query += ` AND status!='expired'`
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now()
	out := []MemoryRecord{}
	for rows.Next() {
		var x MemoryRecord
		var created, updated string
		var expires sql.NullString
		if err := rows.Scan(&x.ID, &x.WorkspaceID, &x.Scope, &x.Kind, &x.Text, &x.Provenance, &x.Confidence, &x.Status, &x.EvidenceRefs, &created, &updated, &expires, &x.SupersededBy, &x.SessionID, &x.Origin, &x.Shadows); err != nil {
			return nil, err
		}
		x.Scope = storage.NormalizeMemoryScope(x.Scope)
		x.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, fmt.Errorf("parse memory created_at: %w", err)
		}
		x.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, fmt.Errorf("parse memory updated_at: %w", err)
		}
		if expires.Valid && expires.String != "" {
			expiry, parseErr := time.Parse(time.RFC3339Nano, expires.String)
			if parseErr != nil {
				return nil, fmt.Errorf("parse memory expires_at: %w", parseErr)
			}
			x.ExpiresAt = &expiry
			if !f.IncludeExpired && f.Status != "expired" && !expiry.After(now) {
				continue
			}
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

func (s *Store) validateMemoryReferences(ctx context.Context, x MemoryRecord) error {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM workspaces WHERE workspace_id=?`, x.WorkspaceID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("workspace does not exist")
		}
		return err
	}
	if x.Shadows != "" {
		var workspaceID, scope string
		err := s.db.QueryRowContext(ctx, `SELECT workspace_id,scope FROM memory_records WHERE memory_id=?`, x.Shadows).Scan(&workspaceID, &scope)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrMemoryNotFound, x.Shadows)
		}
		if err != nil {
			return err
		}
		if workspaceID != x.WorkspaceID {
			return errors.New("a shadowed memory record must be in the same workspace")
		}
		if storage.NormalizeMemoryScope(scope) == storage.ScopeAgent {
			return errors.New("a memory record may only shadow a group or global record")
		}
	}
	if x.SupersededBy == "" {
		return nil
	}
	var workspaceID, status string
	err := s.db.QueryRowContext(ctx, `SELECT workspace_id,status FROM memory_records WHERE memory_id=?`, x.SupersededBy).Scan(&workspaceID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrMemoryNotFound, x.SupersededBy)
	}
	if err != nil {
		return err
	}
	if workspaceID != x.WorkspaceID {
		return errors.New("memory supersession must stay within one workspace")
	}
	if status != "active" {
		return fmt.Errorf("%w: replacement status is %s", ErrMemoryNotSupersedable, status)
	}
	return nil
}

// The memory_records CHECK constraint predates the canonical scope names and
// SQLite cannot alter a CHECK without rebuilding the table, so the table keeps
// the original names and the store translates at its boundary: reads
// normalize through storage.NormalizeMemoryScope and writes and filters map
// back with storedMemoryScope. Existing databases need no migration.
func storedMemoryScope(scope string) string {
	switch scope {
	case storage.ScopeAgent:
		return "workspace"
	case storage.ScopeGroup:
		return "project"
	case storage.ScopeGlobal:
		return "user"
	}
	return scope
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
