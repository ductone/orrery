package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY, spec TEXT NOT NULL, durable_summary TEXT NOT NULL DEFAULT '', phase TEXT NOT NULL DEFAULT 'plan', model TEXT NOT NULL DEFAULT '', turn INTEGER NOT NULL DEFAULT 0, spent_usd REAL NOT NULL DEFAULT 0, budget_usd REAL NOT NULL, status TEXT NOT NULL DEFAULT 'running', integration TEXT NOT NULL DEFAULT '', external_id TEXT NOT NULL DEFAULT '', external_incarnation TEXT NOT NULL DEFAULT '', workspace_path TEXT NOT NULL DEFAULT '', workspace_ownership TEXT NOT NULL DEFAULT 'orrery', integration_context_json TEXT NOT NULL DEFAULT '{}', request_json TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id), seq INTEGER NOT NULL, turn_id TEXT NOT NULL DEFAULT '', type TEXT NOT NULL, data_json TEXT NOT NULL, created_at TEXT NOT NULL, UNIQUE(session_id,seq));
CREATE TABLE IF NOT EXISTS messages(id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id), role TEXT NOT NULL, content_json TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS request_receipts(session_id TEXT NOT NULL REFERENCES sessions(id), request_id TEXT NOT NULL, kind TEXT NOT NULL, turn_id TEXT NOT NULL, payload_hash TEXT NOT NULL, accepted_at TEXT NOT NULL, PRIMARY KEY(session_id,request_id));
CREATE TABLE IF NOT EXISTS todos(id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id), position INTEGER NOT NULL, text TEXT NOT NULL, phase TEXT NOT NULL, status TEXT NOT NULL, UNIQUE(session_id,position));
CREATE TABLE IF NOT EXISTS cache_ledger(session_id TEXT NOT NULL, model TEXT NOT NULL, warm_prefix_tokens INTEGER NOT NULL DEFAULT 0, last_hit TEXT, ttl_seconds INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(session_id,model));
CREATE TABLE IF NOT EXISTS routing_records(id TEXT PRIMARY KEY, session_id TEXT NOT NULL, turn INTEGER NOT NULL, decision_point TEXT NOT NULL, state_json TEXT NOT NULL, candidates_json TEXT NOT NULL, chosen_model TEXT NOT NULL, chosen_effort TEXT NOT NULL, was_switch INTEGER NOT NULL, cache_est_json TEXT NOT NULL, explanation TEXT NOT NULL, turn_outcome_json TEXT, job_outcome_json TEXT, session_outcome_json TEXT, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS jobs(id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id), parent_job_id TEXT, spec TEXT NOT NULL, result_schema_json TEXT NOT NULL, budget_json TEXT NOT NULL, workspace_json TEXT NOT NULL, hints_json TEXT NOT NULL, depth INTEGER NOT NULL, model TEXT NOT NULL, status TEXT NOT NULL, result_json TEXT, outcome_json TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS idx_events_session_seq ON events(session_id,seq); CREATE INDEX IF NOT EXISTS idx_routing_session_turn ON routing_records(session_id,turn); CREATE INDEX IF NOT EXISTS idx_jobs_session ON jobs(session_id);`)
	if err != nil {
		return err
	}
	// Existing v0 databases are upgraded in place. SQLite has no portable
	// ADD COLUMN IF NOT EXISTS, so inspect the schema before each addition.
	for name, definition := range map[string]string{
		"integration":              "TEXT NOT NULL DEFAULT ''",
		"external_id":              "TEXT NOT NULL DEFAULT ''",
		"external_incarnation":     "TEXT NOT NULL DEFAULT ''",
		"workspace_path":           "TEXT NOT NULL DEFAULT ''",
		"workspace_ownership":      "TEXT NOT NULL DEFAULT 'orrery'",
		"integration_context_json": "TEXT NOT NULL DEFAULT '{}'",
		"request_json":             "TEXT NOT NULL DEFAULT '{}'",
	} {
		if err := s.ensureColumn("sessions", name, definition); err != nil {
			return err
		}
	}
	if err := s.ensureColumn("events", "turn_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	_, err = s.db.Exec(`CREATE TABLE IF NOT EXISTS request_receipts(session_id TEXT NOT NULL REFERENCES sessions(id), request_id TEXT NOT NULL, kind TEXT NOT NULL, turn_id TEXT NOT NULL, payload_hash TEXT NOT NULL, accepted_at TEXT NOT NULL, PRIMARY KEY(session_id,request_id)); CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_external ON sessions(integration,external_id,external_incarnation) WHERE external_id!='';`)
	return err
}

func (s *Store) ensureColumn(table, name, definition string) error {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var column, typ string
		var notNull, pk int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &column, &typ, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		found = found || column == name
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + name + ` ` + definition)
	return err
}

type Session struct {
	ID, Spec, DurableSummary, Phase, Model, Status string
	Integration, ExternalID, ExternalIncarnation   string
	WorkspacePath, WorkspaceOwnership              string
	IntegrationContextJSON, RequestJSON            string
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
	if x.WorkspaceOwnership == "" {
		x.WorkspaceOwnership = "orrery"
	}
	if x.IntegrationContextJSON == "" {
		x.IntegrationContextJSON = "{}"
	}
	if x.RequestJSON == "" {
		x.RequestJSON = "{}"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(id,spec,durable_summary,phase,model,turn,spent_usd,budget_usd,status,integration,external_id,external_incarnation,workspace_path,workspace_ownership,integration_context_json,request_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, x.ID, x.Spec, x.DurableSummary, x.Phase, x.Model, x.Turn, x.SpentUSD, x.BudgetUSD, x.Status, x.Integration, x.ExternalID, x.ExternalIncarnation, x.WorkspacePath, x.WorkspaceOwnership, x.IntegrationContextJSON, x.RequestJSON, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	return err
}

// CreateSessionAccepted atomically creates an integration-owned session and
// records durable acceptance of its initial prompt. It returns the existing
// session for the same external identity, making create safe to retry.
func (s *Store) CreateSessionAccepted(ctx context.Context, x Session, requestID, turnID, payloadHash string) (Session, bool, error) {
	if requestID == "" {
		return Session{}, false, errors.New("request_id required")
	}
	if x.ExternalID == "" || x.Integration == "" {
		return Session{}, false, errors.New("integration and external_id required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, false, err
	}
	defer tx.Rollback()
	var existingID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM sessions WHERE integration=? AND external_id=? AND external_incarnation=?`, x.Integration, x.ExternalID, x.ExternalIncarnation).Scan(&existingID)
	if err == nil {
		if err = tx.Commit(); err != nil {
			return Session{}, false, err
		}
		existing, err := s.Session(ctx, existingID)
		return existing, false, err
	}
	if err != sql.ErrNoRows {
		return Session{}, false, err
	}
	now := time.Now().UTC()
	if x.Phase == "" {
		x.Phase = "plan"
	}
	if x.Status == "" {
		x.Status = "running"
	}
	if x.WorkspaceOwnership == "" {
		x.WorkspaceOwnership = "external"
	}
	if x.IntegrationContextJSON == "" {
		x.IntegrationContextJSON = "{}"
	}
	if x.RequestJSON == "" {
		x.RequestJSON = "{}"
	}
	stamp := now.Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions(id,spec,durable_summary,phase,model,turn,spent_usd,budget_usd,status,integration,external_id,external_incarnation,workspace_path,workspace_ownership,integration_context_json,request_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, x.ID, x.Spec, x.DurableSummary, x.Phase, x.Model, x.Turn, x.SpentUSD, x.BudgetUSD, x.Status, x.Integration, x.ExternalID, x.ExternalIncarnation, x.WorkspacePath, x.WorkspaceOwnership, x.IntegrationContextJSON, x.RequestJSON, stamp, stamp)
	if err != nil {
		return Session{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO request_receipts(session_id,request_id,kind,turn_id,payload_hash,accepted_at) VALUES(?,?,?,?,?,?)`, x.ID, requestID, "integration", turnID, payloadHash, stamp); err != nil {
		return Session{}, false, err
	}
	createdData := JSON(map[string]any{"external_id": x.ExternalID, "integration": x.Integration, "workspace_ownership": x.WorkspaceOwnership})
	acceptedData := JSON(map[string]any{"request_id": requestID, "source": x.Integration})
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(session_id,seq,turn_id,type,data_json,created_at) VALUES(?,?,?,?,?,?),(?,?,?,?,?,?)`, x.ID, 1, turnID, "session.created", createdData, stamp, x.ID, 2, turnID, "turn.accepted", acceptedData, stamp); err != nil {
		return Session{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return Session{}, false, err
	}
	x.CreatedAt, x.UpdatedAt = now, now
	return x, true, nil
}
func (s *Store) Session(ctx context.Context, id string) (Session, error) {
	var x Session
	var ca, ua string
	err := s.db.QueryRowContext(ctx, `SELECT id,spec,durable_summary,phase,model,turn,spent_usd,budget_usd,status,integration,external_id,external_incarnation,workspace_path,workspace_ownership,integration_context_json,request_json,created_at,updated_at FROM sessions WHERE id=?`, id).Scan(&x.ID, &x.Spec, &x.DurableSummary, &x.Phase, &x.Model, &x.Turn, &x.SpentUSD, &x.BudgetUSD, &x.Status, &x.Integration, &x.ExternalID, &x.ExternalIncarnation, &x.WorkspacePath, &x.WorkspaceOwnership, &x.IntegrationContextJSON, &x.RequestJSON, &ca, &ua)
	x.CreatedAt, _ = time.Parse(time.RFC3339Nano, ca)
	x.UpdatedAt, _ = time.Parse(time.RFC3339Nano, ua)
	return x, err
}

func (s *Store) SessionByExternalID(ctx context.Context, integration, externalID, incarnation string) (Session, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM sessions WHERE integration=? AND external_id=? AND external_incarnation=?`, integration, externalID, incarnation).Scan(&id)
	if err != nil {
		return Session{}, err
	}
	return s.Session(ctx, id)
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
func (s *Store) SetSessionStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET status=?,updated_at=? WHERE id=?`, status, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}
func (s *Store) MarkRunningInterrupted(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET status='interrupted',updated_at=? WHERE status='running'`, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"request_receipts", "events", "messages", "todos", "cache_ledger", "jobs", "routing_records"} {
		if _, err = tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE session_id=?`, id); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
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
	SchemaVersion int             `json:"schema_version"`
	EventID       string          `json:"event_id"`
	Seq           int             `json:"sequence"`
	SessionID     string          `json:"session_id"`
	TurnID        string          `json:"turn_id,omitempty"`
	Type          string          `json:"type"`
	Data          json.RawMessage `json:"data"`
	CreatedAt     time.Time       `json:"timestamp"`
}

func (s *Store) AddEvent(ctx context.Context, sid, typ string, data any) (Event, error) {
	return s.AddEventForTurn(ctx, sid, "", typ, data)
}

func (s *Store) AddEventForTurn(ctx context.Context, sid, turnID, typ string, data any) (Event, error) {
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(session_id,seq,turn_id,type,data_json,created_at)VALUES(?,?,?,?,?,?)`, sid, seq, turnID, typ, string(b), now.Format(time.RFC3339Nano)); err != nil {
		return Event{}, err
	}
	if err = tx.Commit(); err != nil {
		return Event{}, err
	}
	return Event{SchemaVersion: 1, EventID: fmt.Sprintf("%s:%d", sid, seq), Seq: seq, SessionID: sid, TurnID: turnID, Type: typ, Data: b, CreatedAt: now}, nil
}
func (s *Store) EventsAfter(ctx context.Context, sid string, after int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq,turn_id,type,data_json,created_at FROM events WHERE session_id=? AND seq>? ORDER BY seq`, sid, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var b, ts string
		if err := rows.Scan(&e.Seq, &e.TurnID, &e.Type, &b, &ts); err != nil {
			return nil, err
		}
		e.SchemaVersion = 1
		e.SessionID = sid
		e.EventID = fmt.Sprintf("%s:%d", sid, e.Seq)
		e.Data = json.RawMessage(b)
		e.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

type RequestReceipt struct {
	SessionID, RequestID, Kind, TurnID, PayloadHash string
	AcceptedAt                                      time.Time
	Duplicate                                       bool
}

func (s *Store) RequestReceipt(ctx context.Context, sid, requestID string) (RequestReceipt, error) {
	var x RequestReceipt
	var accepted string
	err := s.db.QueryRowContext(ctx, `SELECT session_id,request_id,kind,turn_id,payload_hash,accepted_at FROM request_receipts WHERE session_id=? AND request_id=?`, sid, requestID).Scan(&x.SessionID, &x.RequestID, &x.Kind, &x.TurnID, &x.PayloadHash, &accepted)
	x.AcceptedAt, _ = time.Parse(time.RFC3339Nano, accepted)
	return x, err
}

// AcceptMessage atomically records an idempotency receipt, the user message,
// and the acceptance event. A retry with the same request ID and payload is a
// successful no-op; reusing an ID for different content is rejected.
func (s *Store) AcceptMessage(ctx context.Context, sid, requestID, turnID, kind, payloadHash string, content any) (RequestReceipt, error) {
	if requestID == "" {
		return RequestReceipt{}, errors.New("request_id required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RequestReceipt{}, err
	}
	defer tx.Rollback()
	var existingHash, existingTurn, existingKind, accepted string
	err = tx.QueryRowContext(ctx, `SELECT payload_hash,turn_id,kind,accepted_at FROM request_receipts WHERE session_id=? AND request_id=?`, sid, requestID).Scan(&existingHash, &existingTurn, &existingKind, &accepted)
	if err == nil {
		if existingHash != payloadHash {
			return RequestReceipt{}, fmt.Errorf("request_id %q was already used with a different payload", requestID)
		}
		ts, _ := time.Parse(time.RFC3339Nano, accepted)
		return RequestReceipt{SessionID: sid, RequestID: requestID, Kind: existingKind, TurnID: existingTurn, PayloadHash: existingHash, AcceptedAt: ts, Duplicate: true}, nil
	}
	if err != sql.ErrNoRows {
		return RequestReceipt{}, err
	}
	now := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `INSERT INTO request_receipts(session_id,request_id,kind,turn_id,payload_hash,accepted_at) VALUES(?,?,?,?,?,?)`, sid, requestID, kind, turnID, payloadHash, now.Format(time.RFC3339Nano)); err != nil {
		return RequestReceipt{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO messages(session_id,role,content_json,created_at) VALUES(?,?,?,?)`, sid, "user", JSON(content), now.Format(time.RFC3339Nano)); err != nil {
		return RequestReceipt{}, err
	}
	var seq int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE session_id=?`, sid).Scan(&seq); err != nil {
		return RequestReceipt{}, err
	}
	data := JSON(map[string]any{"request_id": requestID, "source": kind})
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(session_id,seq,turn_id,type,data_json,created_at) VALUES(?,?,?,?,?,?)`, sid, seq, turnID, "turn.accepted", data, now.Format(time.RFC3339Nano)); err != nil {
		return RequestReceipt{}, err
	}
	userData := JSON(map[string]any{"request_id": requestID, "content": content})
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(session_id,seq,turn_id,type,data_json,created_at) VALUES(?,?,?,?,?,?)`, sid, seq+1, turnID, "user.message", userData, now.Format(time.RFC3339Nano)); err != nil {
		return RequestReceipt{}, err
	}
	if err = tx.Commit(); err != nil {
		return RequestReceipt{}, err
	}
	return RequestReceipt{SessionID: sid, RequestID: requestID, Kind: kind, TurnID: turnID, PayloadHash: payloadHash, AcceptedAt: now}, nil
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
