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
	// Orrery is a single process. One shared SQLite connection gives parent and
	// worker goroutines deterministic transaction ordering without SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
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
	_, err := s.db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;
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
func (s *Store) Sessions(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM sessions ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var out []Session
	for _, id := range ids {
		x, err := s.Session(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, nil
}
func (s *Store) UpdateSession(ctx context.Context, x Session) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET durable_summary=?,phase=?,model=?,turn=?,status=?,updated_at=? WHERE id=?`, x.DurableSummary, x.Phase, x.Model, x.Turn, x.Status, time.Now().UTC().Format(time.RFC3339Nano), x.ID)
	return err
}
func (s *Store) AddSpend(ctx context.Context, id string, usd float64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET spent_usd=spent_usd+?,updated_at=? WHERE id=?`, usd, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}
func (s *Store) ReservedJobUSD(ctx context.Context, sid string) (float64, error) {
	var n float64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(CAST(json_extract(budget_json,'$.max_usd') AS REAL)),0) FROM jobs WHERE session_id=? AND status='running'`, sid).Scan(&n)
	return n, err
}
func (s *Store) UpdateLatestJobRoutingOutcome(ctx context.Context, sid string, v any) error {
	_, err := s.db.ExecContext(ctx, `UPDATE routing_records SET job_outcome_json=? WHERE id=(SELECT id FROM routing_records WHERE session_id=? AND decision_point IN ('spawn','review') AND job_outcome_json IS NULL ORDER BY created_at DESC LIMIT 1)`, JSON(v), sid)
	return err
}
func (s *Store) UpdateLatestTurnRoutingOutcome(ctx context.Context, sid string, turn int, v any) error {
	_, err := s.db.ExecContext(ctx, `UPDATE routing_records SET turn_outcome_json=? WHERE id=(SELECT id FROM routing_records WHERE session_id=? AND turn=? AND decision_point IN ('turn','escalation') ORDER BY created_at DESC LIMIT 1)`, JSON(v), sid, turn)
	return err
}
func (s *Store) UpdateSessionRoutingOutcome(ctx context.Context, sid string, v any) error {
	_, err := s.db.ExecContext(ctx, `UPDATE routing_records SET session_outcome_json=? WHERE session_id=?`, JSON(v), sid)
	return err
}

type Message struct {
	Role, ContentJSON string
	CreatedAt         time.Time
}

func (s *Store) AddMessage(ctx context.Context, sid, role string, content any) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO messages(session_id,role,content_json,created_at)VALUES(?,?,?,?)`, sid, role, JSON(content), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
func (s *Store) Messages(ctx context.Context, sid string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT role,content_json,created_at FROM messages WHERE session_id=? ORDER BY id`, sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var ts string
		if err := rows.Scan(&m.Role, &m.ContentJSON, &ts); err != nil {
			return nil, err
		}
		m.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) CompactMessages(ctx context.Context, sid string, keep int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE session_id=? AND id NOT IN (SELECT id FROM messages WHERE session_id=? ORDER BY id DESC LIMIT ?)`, sid, sid, keep)
	return err
}
func (s *Store) InvalidateCaches(ctx context.Context, sid string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE cache_ledger SET warm_prefix_tokens=0,last_hit=NULL WHERE session_id=?`, sid)
	return err
}

type Todo struct {
	Position int    `json:"position"`
	Text     string `json:"text"`
	Phase    string `json:"phase"`
	Status   string `json:"status"`
}

func (s *Store) SetTodos(ctx context.Context, sid string, todos []Todo) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM todos WHERE session_id=?`, sid); err != nil {
		return err
	}
	for i, t := range todos {
		if _, err = tx.ExecContext(ctx, `INSERT INTO todos(session_id,position,text,phase,status)VALUES(?,?,?,?,?)`, sid, i, t.Text, t.Phase, t.Status); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) Todos(ctx context.Context, sid string) ([]Todo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT position,text,phase,status FROM todos WHERE session_id=? ORDER BY position`, sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Todo
	for rows.Next() {
		var t Todo
		if err := rows.Scan(&t.Position, &t.Text, &t.Phase, &t.Status); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type Job struct {
	ID, SessionID, ParentJobID, Spec, ResultSchemaJSON, BudgetJSON, WorkspaceJSON, HintsJSON, Model, Status, ResultJSON, OutcomeJSON string
	Depth                                                                                                                            int
	CreatedAt, UpdatedAt                                                                                                             time.Time
}

func (s *Store) CreateJob(ctx context.Context, j Job) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs(id,session_id,parent_job_id,spec,result_schema_json,budget_json,workspace_json,hints_json,depth,model,status,created_at,updated_at)VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, j.ID, j.SessionID, nilIfEmpty(j.ParentJobID), j.Spec, j.ResultSchemaJSON, j.BudgetJSON, j.WorkspaceJSON, j.HintsJSON, j.Depth, j.Model, j.Status, now, now)
	return err
}
func (s *Store) FinishJob(ctx context.Context, id, status string, result, outcome any) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status=?,result_json=?,outcome_json=?,updated_at=? WHERE id=?`, status, JSON(result), JSON(outcome), time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}
func (s *Store) Job(ctx context.Context, id string) (Job, error) {
	var j Job
	var parent, result, outcome sql.NullString
	var ca, ua string
	err := s.db.QueryRowContext(ctx, `SELECT id,session_id,parent_job_id,spec,result_schema_json,budget_json,workspace_json,hints_json,depth,model,status,result_json,outcome_json,created_at,updated_at FROM jobs WHERE id=?`, id).Scan(&j.ID, &j.SessionID, &parent, &j.Spec, &j.ResultSchemaJSON, &j.BudgetJSON, &j.WorkspaceJSON, &j.HintsJSON, &j.Depth, &j.Model, &j.Status, &result, &outcome, &ca, &ua)
	j.ParentJobID = parent.String
	j.ResultJSON = result.String
	j.OutcomeJSON = outcome.String
	j.CreatedAt, _ = time.Parse(time.RFC3339Nano, ca)
	j.UpdatedAt, _ = time.Parse(time.RFC3339Nano, ua)
	return j, err
}
func (s *Store) Jobs(ctx context.Context, sid string) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs WHERE session_id=? ORDER BY created_at`, sid)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var out []Job
	for _, id := range ids {
		j, err := s.Job(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
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

type CacheEntry struct {
	SessionID, Model string
	WarmPrefixTokens int
	LastHit          time.Time
	TTL              time.Duration
}

func (e CacheEntry) Valid(now time.Time) bool {
	return e.WarmPrefixTokens > 0 && !e.LastHit.IsZero() && now.Before(e.LastHit.Add(e.TTL))
}
func (s *Store) Cache(ctx context.Context, sid, model string) (CacheEntry, error) {
	var e CacheEntry
	var hit sql.NullString
	var ttl int
	err := s.db.QueryRowContext(ctx, `SELECT session_id,model,warm_prefix_tokens,last_hit,ttl_seconds FROM cache_ledger WHERE session_id=? AND model=?`, sid, model).Scan(&e.SessionID, &e.Model, &e.WarmPrefixTokens, &hit, &ttl)
	if err == sql.ErrNoRows {
		return CacheEntry{SessionID: sid, Model: model}, nil
	}
	if hit.Valid {
		e.LastHit, _ = time.Parse(time.RFC3339Nano, hit.String)
	}
	e.TTL = time.Duration(ttl) * time.Second
	return e, err
}
func (s *Store) WarmCache(ctx context.Context, sid, model string, tokens int, ttl time.Duration) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO cache_ledger(session_id,model,warm_prefix_tokens,last_hit,ttl_seconds)VALUES(?,?,?,?,?) ON CONFLICT(session_id,model) DO UPDATE SET warm_prefix_tokens=excluded.warm_prefix_tokens,last_hit=excluded.last_hit,ttl_seconds=excluded.ttl_seconds`, sid, model, tokens, time.Now().UTC().Format(time.RFC3339Nano), int(ttl.Seconds()))
	return err
}

type RoutingRecord struct {
	ID, SessionID                                                       string
	Turn                                                                int
	DecisionPoint, StateJSON, CandidatesJSON, ChosenModel, ChosenEffort string
	WasSwitch                                                           bool
	CacheEstJSON, Explanation                                           string
}

func (s *Store) WriteRouting(ctx context.Context, r RoutingRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO routing_records(id,session_id,turn,decision_point,state_json,candidates_json,chosen_model,chosen_effort,was_switch,cache_est_json,explanation,created_at)VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, r.ID, r.SessionID, r.Turn, r.DecisionPoint, r.StateJSON, r.CandidatesJSON, r.ChosenModel, r.ChosenEffort, r.WasSwitch, r.CacheEstJSON, r.Explanation, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
func (s *Store) UpdateRoutingOutcome(ctx context.Context, id, field string, v any) error {
	allowed := map[string]bool{"turn_outcome_json": true, "job_outcome_json": true, "session_outcome_json": true}
	if !allowed[field] {
		return fmt.Errorf("invalid outcome field %q", field)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE routing_records SET `+field+`=? WHERE id=?`, JSON(v), id)
	return err
}
