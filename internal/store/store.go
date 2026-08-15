package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	s := &Store{db}
	if err = s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }
func (s *Store) migrate() error {
	_, err := s.db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY, spec TEXT NOT NULL, durable_summary TEXT NOT NULL DEFAULT '', phase TEXT NOT NULL DEFAULT 'plan', model TEXT NOT NULL DEFAULT '', turn INTEGER NOT NULL DEFAULT 0, spent_usd REAL NOT NULL DEFAULT 0, budget_usd REAL NOT NULL, status TEXT NOT NULL DEFAULT 'running', created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id), seq INTEGER NOT NULL, type TEXT NOT NULL, data_json TEXT NOT NULL, created_at TEXT NOT NULL, UNIQUE(session_id,seq));
CREATE TABLE IF NOT EXISTS messages(id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id), role TEXT NOT NULL, content_json TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS todos(id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id), position INTEGER NOT NULL, text TEXT NOT NULL, phase TEXT NOT NULL, status TEXT NOT NULL, UNIQUE(session_id,position));
CREATE TABLE IF NOT EXISTS cache_ledger(session_id TEXT NOT NULL, model TEXT NOT NULL, warm_prefix_tokens INTEGER NOT NULL DEFAULT 0, last_hit TEXT, ttl_seconds INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(session_id,model));
CREATE TABLE IF NOT EXISTS routing_records(id TEXT PRIMARY KEY, session_id TEXT NOT NULL, turn INTEGER NOT NULL, decision_point TEXT NOT NULL, state_json TEXT NOT NULL, candidates_json TEXT NOT NULL, chosen_model TEXT NOT NULL, chosen_effort TEXT NOT NULL, was_switch INTEGER NOT NULL, cache_est_json TEXT NOT NULL, explanation TEXT NOT NULL, turn_outcome_json TEXT, job_outcome_json TEXT, session_outcome_json TEXT, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS jobs(id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id), parent_job_id TEXT, spec TEXT NOT NULL, result_schema_json TEXT NOT NULL, budget_json TEXT NOT NULL, workspace_json TEXT NOT NULL, hints_json TEXT NOT NULL, depth INTEGER NOT NULL, model TEXT NOT NULL, status TEXT NOT NULL, result_json TEXT, outcome_json TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS idx_events_session_seq ON events(session_id,seq); CREATE INDEX IF NOT EXISTS idx_routing_session_turn ON routing_records(session_id,turn); CREATE INDEX IF NOT EXISTS idx_jobs_session ON jobs(session_id);`)
	return err
}

type Session struct {
	ID, Spec, DurableSummary, Phase, Model, Status string
	Turn                                           int
	SpentUSD, BudgetUSD                            float64
	CreatedAt, UpdatedAt                           time.Time
}

func (s *Store) CreateSession(ctx context.Context, x Session) error {
	now := time.Now().UTC()
	if x.Phase == "" {
		x.Phase = "plan"
	}
	if x.Status == "" {
		x.Status = "running"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(id,spec,durable_summary,phase,model,turn,spent_usd,budget_usd,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, x.ID, x.Spec, x.DurableSummary, x.Phase, x.Model, x.Turn, x.SpentUSD, x.BudgetUSD, x.Status, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	return err
}
func (s *Store) Session(ctx context.Context, id string) (Session, error) {
	var x Session
	var ca, ua string
	err := s.db.QueryRowContext(ctx, `SELECT id,spec,durable_summary,phase,model,turn,spent_usd,budget_usd,status,created_at,updated_at FROM sessions WHERE id=?`, id).Scan(&x.ID, &x.Spec, &x.DurableSummary, &x.Phase, &x.Model, &x.Turn, &x.SpentUSD, &x.BudgetUSD, &x.Status, &ca, &ua)
	x.CreatedAt, _ = time.Parse(time.RFC3339Nano, ca)
	x.UpdatedAt, _ = time.Parse(time.RFC3339Nano, ua)
	return x, err
}
func (s *Store) UpdateSession(ctx context.Context, x Session) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET durable_summary=?,phase=?,model=?,turn=?,spent_usd=?,status=?,updated_at=? WHERE id=?`, x.DurableSummary, x.Phase, x.Model, x.Turn, x.SpentUSD, x.Status, time.Now().UTC().Format(time.RFC3339Nano), x.ID)
	return err
}

type Event struct {
	Seq       int             `json:"seq"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"created_at"`
}

func (s *Store) AddEvent(ctx context.Context, sid, typ string, data any) (Event, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return Event{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, err
	}
	defer tx.Rollback()
	var seq int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE session_id=?`, sid).Scan(&seq); err != nil {
		return Event{}, err
	}
	now := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(session_id,seq,type,data_json,created_at)VALUES(?,?,?,?,?)`, sid, seq, typ, string(b), now.Format(time.RFC3339Nano)); err != nil {
		return Event{}, err
	}
	if err = tx.Commit(); err != nil {
		return Event{}, err
	}
	return Event{seq, typ, b, now}, nil
}
func (s *Store) EventsAfter(ctx context.Context, sid string, after int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq,type,data_json,created_at FROM events WHERE session_id=? AND seq>? ORDER BY seq`, sid, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var b, ts string
		if err := rows.Scan(&e.Seq, &e.Type, &b, &ts); err != nil {
			return nil, err
		}
		e.Data = json.RawMessage(b)
		e.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, e)
	}
	return out, rows.Err()
}
func JSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func (s *Store) ExportRouting(ctx context.Context, since time.Time, emit func([]byte) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT json_object('id',id,'session_id',session_id,'turn',turn,'decision_point',decision_point,'state',json(state_json),'candidates',json(candidates_json),'chosen_model',chosen_model,'chosen_effort',chosen_effort,'was_switch',json(was_switch),'cache_est',json(cache_est_json),'explanation',explanation,'turn_outcome',json(turn_outcome_json),'job_outcome',json(job_outcome_json),'session_outcome',json(session_outcome_json),'created_at',created_at) FROM routing_records WHERE created_at>=? ORDER BY created_at`, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		if err := emit([]byte(line)); err != nil {
			return err
		}
	}
	return rows.Err()
}
func (s *Store) String() string { return fmt.Sprintf("sqlite(%p)", s.db) }
