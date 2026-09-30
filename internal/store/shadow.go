package store

import (
	"context"
	"database/sql"
	"time"
)

// shadowSchema holds observations from a shadow classifier. A row is written
// synchronously when the question is asked, so the answer (written by the
// asynchronous caller) and the harness's own baseline and eventual outcome can
// land in any order.
const shadowSchema = `CREATE TABLE IF NOT EXISTS shadow_observations(id TEXT PRIMARY KEY, session_id TEXT NOT NULL, turn_id TEXT NOT NULL DEFAULT '', turn INTEGER NOT NULL DEFAULT 0, site TEXT NOT NULL, question_version TEXT NOT NULL, questions_json TEXT NOT NULL, state_json TEXT NOT NULL, state_chars INTEGER NOT NULL DEFAULT 0, classifier_model TEXT NOT NULL DEFAULT '', answers_json TEXT, usage_json TEXT, latency_ms INTEGER, error TEXT NOT NULL DEFAULT '', baseline_json TEXT, outcome_json TEXT, created_at TEXT NOT NULL, answered_at TEXT); CREATE INDEX IF NOT EXISTS idx_shadow_site_created ON shadow_observations(site,created_at); CREATE INDEX IF NOT EXISTS idx_shadow_session ON shadow_observations(session_id);`

type ShadowObservation struct {
	ID, SessionID, TurnID, Site, QuestionVersion string
	Turn                                         int
	Questions, State, Baseline                   any
}

func (s *Store) CreateShadow(ctx context.Context, o ShadowObservation) error {
	state := JSON(o.State)
	var baseline any
	if o.Baseline != nil {
		baseline = JSON(o.Baseline)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO shadow_observations(id,session_id,turn_id,turn,site,question_version,questions_json,state_json,state_chars,baseline_json,created_at)VALUES(?,?,?,?,?,?,?,?,?,?,?)`, o.ID, o.SessionID, o.TurnID, o.Turn, o.Site, o.QuestionVersion, JSON(o.Questions), state, len(state), baseline, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// CompleteShadow records the classifier's answer, or the error that replaced it.
func (s *Store) CompleteShadow(ctx context.Context, id, classifierModel string, answers, usage any, latency time.Duration, callErr error) error {
	var answersJSON, usageJSON any
	errText := ""
	if callErr != nil {
		errText = callErr.Error()
	} else {
		answersJSON, usageJSON = JSON(answers), JSON(usage)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE shadow_observations SET classifier_model=?,answers_json=?,usage_json=?,latency_ms=?,error=?,answered_at=? WHERE id=?`, classifierModel, answersJSON, usageJSON, latency.Milliseconds(), errText, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

// SetShadowBaseline records what the harness itself decided.
func (s *Store) SetShadowBaseline(ctx context.Context, id string, v any) error {
	_, err := s.db.ExecContext(ctx, `UPDATE shadow_observations SET baseline_json=? WHERE id=?`, JSON(v), id)
	return err
}

// SetShadowOutcome records ground truth that arrives after the decision.
func (s *Store) SetShadowOutcome(ctx context.Context, id string, v any) error {
	_, err := s.db.ExecContext(ctx, `UPDATE shadow_observations SET outcome_json=? WHERE id=?`, JSON(v), id)
	return err
}

// ShadowRecord is one exported observation. State is empty unless requested,
// because it carries source content.
type ShadowRecord struct {
	ID              string  `json:"id"`
	SessionID       string  `json:"session_id"`
	TurnID          string  `json:"turn_id"`
	Turn            int     `json:"turn"`
	Site            string  `json:"site"`
	QuestionVersion string  `json:"question_version"`
	ClassifierModel string  `json:"classifier_model,omitempty"`
	Questions       RawJSON `json:"questions"`
	State           RawJSON `json:"state,omitempty"`
	StateChars      int     `json:"state_chars"`
	Answers         RawJSON `json:"answers,omitempty"`
	Usage           RawJSON `json:"usage,omitempty"`
	LatencyMS       *int64  `json:"latency_ms,omitempty"`
	Error           string  `json:"error,omitempty"`
	Baseline        RawJSON `json:"baseline,omitempty"`
	Outcome         RawJSON `json:"outcome,omitempty"`
	CreatedAt       string  `json:"created_at"`
	AnsweredAt      string  `json:"answered_at,omitempty"`
}

// RawJSON is stored JSON passed through to an encoder unchanged.
type RawJSON string

func (r RawJSON) MarshalJSON() ([]byte, error) {
	if r == "" {
		return []byte("null"), nil
	}
	return []byte(r), nil
}

func (s *Store) ShadowRecords(ctx context.Context, since time.Time, site string, includeState bool) ([]ShadowRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,session_id,turn_id,turn,site,question_version,classifier_model,questions_json,state_json,state_chars,answers_json,usage_json,latency_ms,error,baseline_json,outcome_json,created_at,answered_at FROM shadow_observations WHERE created_at>=? AND (?='' OR site=?) ORDER BY created_at`, since.UTC().Format(time.RFC3339Nano), site, site)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShadowRecord
	for rows.Next() {
		var r ShadowRecord
		var state string
		var answers, usage, baseline, outcome, answeredAt sql.NullString
		var latency sql.NullInt64
		if err := rows.Scan(&r.ID, &r.SessionID, &r.TurnID, &r.Turn, &r.Site, &r.QuestionVersion, &r.ClassifierModel, &r.Questions, &state, &r.StateChars, &answers, &usage, &latency, &r.Error, &baseline, &outcome, &r.CreatedAt, &answeredAt); err != nil {
			return nil, err
		}
		if includeState {
			r.State = RawJSON(state)
		}
		r.Answers, r.Usage, r.Baseline, r.Outcome = RawJSON(answers.String), RawJSON(usage.String), RawJSON(baseline.String), RawJSON(outcome.String)
		r.AnsweredAt = answeredAt.String
		if latency.Valid {
			r.LatencyMS = &latency.Int64
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestChildSession returns the most recent worker session a parent created
// for a spec. Jobs do not record their session, but a spec is specific enough
// (a review spec embeds its diff) to find the attempt that just finished.
func (s *Store) LatestChildSession(ctx context.Context, parentID, spec string) (Session, error) {
	var id string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM sessions WHERE parent_session_id=? AND spec=? ORDER BY created_at DESC LIMIT 1`, parentID, spec).Scan(&id); err != nil {
		return Session{}, err
	}
	return s.Session(ctx, id)
}
