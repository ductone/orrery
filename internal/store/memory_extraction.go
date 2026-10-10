package store

import (
	"context"
	"database/sql"
	"slices"
)

// Normalize legacy free-text categories before enforcing the enumerations on writes.
const memorySchema = memoryBaseSchema + `CREATE TABLE IF NOT EXISTS memory_watermarks(session_id TEXT PRIMARY KEY REFERENCES sessions(id), sequence INTEGER NOT NULL DEFAULT 0); CREATE TABLE IF NOT EXISTS memory_sightings(memory_id TEXT NOT NULL REFERENCES memory_records(memory_id), session_id TEXT NOT NULL REFERENCES sessions(id), sightings INTEGER NOT NULL DEFAULT 1, PRIMARY KEY(memory_id,session_id)); UPDATE memory_records SET kind='fact' WHERE kind NOT IN ('fact','command','decision','preference','lesson'); UPDATE memory_records SET provenance='observed' WHERE provenance NOT IN ('user','instruction','observed','extracted');`

var MemoryKinds = []string{"fact", "command", "decision", "preference", "lesson"}
var MemoryProvenances = []string{"user", "instruction", "observed", "extracted"}

func validMemoryKind(kind string) bool { return slices.Contains(MemoryKinds, kind) }
func validMemoryProvenance(provenance string) bool {
	return slices.Contains(MemoryProvenances, provenance)
}

func (s *Store) MemoryWatermark(ctx context.Context, sid string) (int, error) {
	var seq int
	err := s.db.QueryRowContext(ctx, `SELECT sequence FROM memory_watermarks WHERE session_id=?`, sid).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return seq, err
}

func (s *Store) AdvanceMemoryWatermark(ctx context.Context, sid string, seq int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO memory_watermarks(session_id,sequence) VALUES(?,?) ON CONFLICT(session_id) DO UPDATE SET sequence=MAX(sequence,excluded.sequence)`, sid, seq)
	return err
}

func (s *Store) ObserveMemory(ctx context.Context, id, sid string) (int, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO memory_sightings(memory_id,session_id) VALUES(?,?) ON CONFLICT(memory_id,session_id) DO UPDATE SET sightings=sightings+1`, id, sid)
	if err != nil {
		return 0, err
	}
	var sessions int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_sightings WHERE memory_id=?`, id).Scan(&sessions)
	return sessions, err
}

// MemoryWorkspaces lists every workspace that has memory records.
func (s *Store) MemoryWorkspaces(ctx context.Context) ([]Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT w.workspace_id,w.identity_key FROM workspaces w JOIN memory_records m ON m.workspace_id=w.workspace_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var w Workspace
		if err := rows.Scan(&w.ID, &w.IdentityKey); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
