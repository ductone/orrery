package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const memoryBaseSchema = `CREATE TABLE IF NOT EXISTS workspaces(workspace_id TEXT PRIMARY KEY, identity_key TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL, updated_at TEXT NOT NULL); CREATE TABLE IF NOT EXISTS memory_records(memory_id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL REFERENCES workspaces(workspace_id), scope TEXT NOT NULL CHECK(scope IN ('workspace','project','user')), kind TEXT NOT NULL, text TEXT NOT NULL, provenance TEXT NOT NULL, confidence REAL NOT NULL CHECK(confidence>=0 AND confidence<=1), status TEXT NOT NULL CHECK(status IN ('pending','active','superseded','expired','deleted')), evidence_refs TEXT NOT NULL DEFAULT '[]', created_at TEXT NOT NULL, updated_at TEXT NOT NULL, expires_at TEXT, superseded_by TEXT REFERENCES memory_records(memory_id)); CREATE INDEX IF NOT EXISTS memory_records_lookup ON memory_records(workspace_id,scope,status,updated_at);`

var (
	ErrMemoryNotFound        = errors.New("memory record not found")
	ErrMemoryNotSupersedable = errors.New("memory record cannot be superseded")
)

type Workspace struct {
	ID, IdentityKey      string
	CreatedAt, UpdatedAt time.Time
}

type MemoryRecord struct {
	ID, WorkspaceID, Scope, Kind, Text, Provenance string
	Confidence                                     float64
	Status                                         string
	EvidenceRefs                                   string
	CreatedAt, UpdatedAt                           time.Time
	ExpiresAt                                      *time.Time
	SupersededBy                                   string
}

type MemoryFilter struct {
	WorkspaceID    string
	Scope          string
	Status         string
	IncludeExpired bool
	// IncludeDeleted also returns forget tombstones, so extraction can avoid
	// resurrecting something the person explicitly forgot.
	IncludeDeleted bool
}

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
	applyMemoryDefaults(&x)
	if x.ID == "" {
		x.ID = uuid.NewString()
	}
	if err := validateMemoryRecord(x); err != nil {
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO memory_records(memory_id,workspace_id,scope,kind,text,provenance,confidence,status,evidence_refs,created_at,updated_at,expires_at,superseded_by) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, x.ID, x.WorkspaceID, x.Scope, x.Kind, x.Text, x.Provenance, x.Confidence, x.Status, x.EvidenceRefs, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), expiry, nullIfEmpty(x.SupersededBy))
	if err != nil {
		return MemoryRecord{}, err
	}
	return x, nil
}

func (s *Store) UpdateMemory(ctx context.Context, x MemoryRecord) error {
	applyMemoryDefaults(&x)
	if x.ID == "" {
		return errors.New("memory id is required")
	}
	if err := validateMemoryRecord(x); err != nil {
		return err
	}
	if err := s.validateMemoryReferences(ctx, x); err != nil {
		return err
	}
	var expiry any
	if x.ExpiresAt != nil {
		expiry = x.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE memory_records SET scope=?,kind=?,text=?,provenance=?,confidence=?,status=?,evidence_refs=?,updated_at=?,expires_at=?,superseded_by=? WHERE memory_id=? AND workspace_id=? AND status!='deleted' AND status!='superseded'`, x.Scope, x.Kind, x.Text, x.Provenance, x.Confidence, x.Status, x.EvidenceRefs, time.Now().UTC().Format(time.RFC3339Nano), expiry, nullIfEmpty(x.SupersededBy), x.ID, x.WorkspaceID)
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

// MemoryMutation describes one lifecycle change committed with its audit event.
type MemoryMutation struct {
	Operation   string
	Record      MemoryRecord
	WorkspaceID string
	OldID       string
	MemoryID    string
}

// ApplyMemoryMutationEvent commits a memory lifecycle change and its event in
// one transaction. If either write fails, neither is durable.
func (s *Store) ApplyMemoryMutationEvent(ctx context.Context, sid, turnID, eventType string, eventData any, m MemoryMutation) (MemoryRecord, error) {
	x := m.Record
	if m.Operation == "commit" || m.Operation == "correct" {
		applyMemoryDefaults(&x)
		if x.ID == "" {
			x.ID = uuid.NewString()
		}
		if err := validateMemoryRecord(x); err != nil {
			return MemoryRecord{}, err
		}
		if err := s.validateMemoryReferences(ctx, x); err != nil {
			return MemoryRecord{}, err
		}
	} else if m.Operation == "update" {
		applyMemoryDefaults(&x)
		if x.ID == "" {
			return MemoryRecord{}, errors.New("memory id is required")
		}
		if err := validateMemoryRecord(x); err != nil {
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
		_, err = tx.ExecContext(ctx, `INSERT INTO memory_records(memory_id,workspace_id,scope,kind,text,provenance,confidence,status,evidence_refs,created_at,updated_at,expires_at,superseded_by) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, x.ID, x.WorkspaceID, x.Scope, x.Kind, x.Text, x.Provenance, x.Confidence, x.Status, x.EvidenceRefs, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), expiry, nullIfEmpty(x.SupersededBy))
	case "update":
		var res sql.Result
		res, err = tx.ExecContext(ctx, `UPDATE memory_records SET scope=?,kind=?,text=?,provenance=?,confidence=?,status=?,evidence_refs=?,updated_at=?,expires_at=?,superseded_by=? WHERE memory_id=? AND workspace_id=? AND status!='deleted' AND status!='superseded'`, x.Scope, x.Kind, x.Text, x.Provenance, x.Confidence, x.Status, x.EvidenceRefs, now.Format(time.RFC3339Nano), expiry, nullIfEmpty(x.SupersededBy), x.ID, x.WorkspaceID)
		if err == nil {
			var n int64
			n, err = res.RowsAffected()
			if err == nil && n == 0 {
				err = sql.ErrNoRows
			}
		}
	case "correct":
		x.CreatedAt, x.UpdatedAt = now, now
		_, err = tx.ExecContext(ctx, `INSERT INTO memory_records(memory_id,workspace_id,scope,kind,text,provenance,confidence,status,evidence_refs,created_at,updated_at,expires_at,superseded_by) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, x.ID, x.WorkspaceID, x.Scope, x.Kind, x.Text, x.Provenance, x.Confidence, x.Status, x.EvidenceRefs, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), expiry, nil)
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
	if m.Operation == "forget" {
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
	row := tx.QueryRowContext(ctx, `SELECT memory_id,workspace_id,scope,kind,text,provenance,confidence,status,evidence_refs,created_at,updated_at,expires_at,COALESCE(superseded_by,'') FROM memory_records WHERE memory_id=?`, id)
	if err := row.Scan(&x.ID, &x.WorkspaceID, &x.Scope, &x.Kind, &x.Text, &x.Provenance, &x.Confidence, &x.Status, &x.EvidenceRefs, &created, &updated, &expires, &x.SupersededBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MemoryRecord{}, fmt.Errorf("%w: %s", ErrMemoryNotFound, id)
		}
		return MemoryRecord{}, err
	}
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
	query := `SELECT memory_id,workspace_id,scope,kind,text,provenance,confidence,status,evidence_refs,created_at,updated_at,expires_at,COALESCE(superseded_by,'') FROM memory_records WHERE workspace_id=?`
	if !f.IncludeDeleted {
		query += ` AND status!='deleted'`
	}
	args := []any{f.WorkspaceID}
	if f.Scope != "" {
		query += ` AND scope=?`
		args = append(args, f.Scope)
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
		if err := rows.Scan(&x.ID, &x.WorkspaceID, &x.Scope, &x.Kind, &x.Text, &x.Provenance, &x.Confidence, &x.Status, &x.EvidenceRefs, &created, &updated, &expires, &x.SupersededBy); err != nil {
			return nil, err
		}
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

func applyMemoryDefaults(x *MemoryRecord) {
	if x.Scope == "" {
		x.Scope = "workspace"
	}
	if x.Status == "" {
		x.Status = "active"
	}
	if x.EvidenceRefs == "" {
		x.EvidenceRefs = "[]"
	}
}

func validateMemoryRecord(x MemoryRecord) error {
	if x.WorkspaceID == "" {
		return errors.New("workspace id is required")
	}
	if x.Scope != "workspace" && x.Scope != "project" {
		return errors.New("memory scope must be workspace or project")
	}
	switch x.Status {
	case "pending", "active", "superseded", "expired":
	default:
		return errors.New("invalid memory status")
	}
	if x.Kind == "" {
		return errors.New("memory kind is required")
	}
	if x.Text == "" {
		return errors.New("memory text is required")
	}
	if x.Provenance == "" {
		return errors.New("memory provenance is required")
	}
	if !validMemoryKind(x.Kind) {
		return fmt.Errorf("memory kind must be one of %s", strings.Join(MemoryKinds, ", "))
	}
	if !validMemoryProvenance(x.Provenance) {
		return fmt.Errorf("memory provenance must be one of %s", strings.Join(MemoryProvenances, ", "))
	}
	if x.Confidence < 0 || x.Confidence > 1 {
		return errors.New("memory confidence must be between 0 and 1")
	}
	var evidence any
	if err := json.Unmarshal([]byte(x.EvidenceRefs), &evidence); err != nil {
		return errors.New("memory evidence refs must be a JSON array")
	}
	if _, ok := evidence.([]any); !ok {
		return errors.New("memory evidence refs must be a JSON array")
	}
	if x.Status == "superseded" && x.SupersededBy == "" {
		return errors.New("superseded memory requires a replacement")
	}
	if x.Status != "superseded" && x.SupersededBy != "" {
		return errors.New("only superseded memory may name a replacement")
	}
	return nil
}

func (s *Store) validateMemoryReferences(ctx context.Context, x MemoryRecord) error {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM workspaces WHERE workspace_id=?`, x.WorkspaceID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("workspace does not exist")
		}
		return err
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

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
