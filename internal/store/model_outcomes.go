package store

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

const modelOutcomesSchema = `CREATE TABLE IF NOT EXISTS model_outcomes(route TEXT PRIMARY KEY, authored_pass REAL NOT NULL DEFAULT 0, authored_fail REAL NOT NULL DEFAULT 0, reviews INTEGER NOT NULL DEFAULT 0, overturned INTEGER NOT NULL DEFAULT 0, answer_rejected INTEGER NOT NULL DEFAULT 0, run_pass INTEGER NOT NULL DEFAULT 0, run_fail INTEGER NOT NULL DEFAULT 0, run_input_required INTEGER NOT NULL DEFAULT 0, run_cancelled INTEGER NOT NULL DEFAULT 0);`

// ModelOutcome is what happened to the work a route did. Authored counts are
// fractional: a review verdict is shared among the routes that made the edit
// and exec-write calls since the previous review, by share of calls.
type ModelOutcome struct {
	Route                      string
	AuthoredPass, AuthoredFail float64
	// Reviews counts reviews the route performed; Overturned those whose
	// findings were later overturned by adjudication or a later review.
	Reviews, Overturned                              int
	AnswerRejected                                   int
	RunPass, RunFail, RunInputRequired, RunCancelled int
}

// outcomeSession is the per-session state needed to attribute outcomes.
type outcomeSession struct {
	route         string             // active route from the latest routing decision
	answerRoute   string             // route that wrote the latest assistant message
	weights       map[string]float64 // edit and exec-write calls since the last review
	reviewers     []string           // review job routes since the last review outcome
	lastShares    map[string]float64 // authors of the last reviewed diff
	lastReviewers []string
	lastFailed    bool // last review failed
}

// outcomeProjector folds events into model_outcomes. The live store and the
// backfill share it so both attribute identically.
type outcomeProjector struct {
	mu       sync.Mutex
	sessions map[string]*outcomeSession
}

func newOutcomeProjector() *outcomeProjector {
	return &outcomeProjector{sessions: map[string]*outcomeSession{}}
}

var execWrite = regexp.MustCompile(`(^|[\s;&|(])(sed\s+-[a-z]*i|tee|mv|cp|rm|touch|patch|git\s+(apply|checkout|restore|mv|rm)|gofmt\s+-w|goimports\s+-w|go\s+mod\s+tidy)(\s|$)|[^0-9&>=-]>>?\s*[^&\s]`)

var execNoise = strings.NewReplacer("2>&1", "", ">/dev/null", "", "> /dev/null", "", "2>/dev/null", "")

func execWrites(command string) bool {
	return execWrite.MatchString(" " + execNoise.Replace(command))
}

func (o *outcomeProjector) apply(ctx context.Context, db statsExecer, sid, typ string, raw []byte) error {
	switch typ {
	case "routing.decision", "routing.selected", "job.started", "tool.finished", "assistant.message", "review.outcome", "completion.answer_check", "session.terminal":
	default:
		return nil
	}
	var d struct {
		Model    string `json:"model"`
		Review   bool   `json:"review"`
		Pass     bool   `json:"pass"`
		Inconcl  bool   `json:"inconclusive"`
		Disputed bool   `json:"disputed"`
		OffTopic bool   `json:"off_topic"`
		Announce bool   `json:"announces_work"`
		Status   string `json:"status"`
		Decision struct {
			Model struct {
				ID string `json:"id"`
			} `json:"model"`
		} `json:"decision"`
		Call struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"call"`
		Result       any  `json:"result"`
		Deduplicated bool `json:"deduplicated"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	s := o.sessions[sid]
	if s == nil {
		s = &outcomeSession{weights: map[string]float64{}}
		o.sessions[sid] = s
	}
	switch typ {
	case "routing.decision", "routing.selected":
		if d.Decision.Model.ID != "" {
			s.route = d.Decision.Model.ID
		}
	case "assistant.message":
		s.answerRoute = d.Model
		if s.answerRoute == "" {
			s.answerRoute = s.route
		}
	case "job.started":
		if d.Review && d.Model != "" {
			s.reviewers = append(s.reviewers, d.Model)
		}
	case "tool.finished":
		if s.route == "" || d.Deduplicated {
			return nil
		}
		if m, ok := d.Result.(map[string]any); ok && m["ok"] == false {
			return nil
		}
		switch d.Call.Name {
		case "edit":
			s.weights[s.route]++
		case "exec":
			if c, _ := d.Call.Arguments["command"].(string); execWrites(c) {
				s.weights[s.route]++
			}
		}
	case "completion.answer_check":
		if (d.OffTopic || d.Announce) && s.answerRoute != "" {
			return bump(ctx, db, s.answerRoute, "answer_rejected", 1)
		}
	case "session.terminal":
		col := map[string]string{"pass": "run_pass", "fail": "run_fail", "input_required": "run_input_required", "cancelled": "run_cancelled"}[d.Status]
		if col != "" && s.answerRoute != "" {
			return bump(ctx, db, s.answerRoute, col, 1)
		}
	case "review.outcome":
		return o.review(ctx, db, s, d.Pass, d.Inconcl, d.Disputed)
	}
	return nil
}

func (o *outcomeProjector) review(ctx context.Context, db statsExecer, s *outcomeSession, pass, inconclusive, disputed bool) error {
	if inconclusive {
		return nil
	}
	overturn := func() error {
		for _, r := range s.lastReviewers {
			if err := bump(ctx, db, r, "overturned", 1); err != nil {
				return err
			}
		}
		for r, share := range s.lastShares {
			if err := bumpAuthored(ctx, db, r, share, -share); err != nil {
				return err
			}
		}
		s.lastFailed = false
		return nil
	}
	if disputed {
		// Adjudication re-judges the diff the last review failed. Passing it
		// converts that failure and overturns the original reviewers.
		s.reviewers = nil
		if pass && s.lastFailed {
			return overturn()
		}
		return nil
	}
	for _, r := range s.reviewers {
		if err := bump(ctx, db, r, "reviews", 1); err != nil {
			return err
		}
	}
	total := 0.0
	for _, w := range s.weights {
		total += w
	}
	if total == 0 {
		// No edits since the last review: the same diff was judged again, so
		// a pass overturns the failed review before it.
		if pass && s.lastFailed {
			err := overturn()
			s.reviewers = nil
			return err
		}
		if !pass {
			s.lastFailed, s.lastReviewers = true, s.reviewers
		}
		s.reviewers = nil
		return nil
	}
	shares := map[string]float64{}
	for r, w := range s.weights {
		shares[r] = w / total
		p, f := 0.0, shares[r]
		if pass {
			p, f = f, 0
		}
		if err := bumpAuthored(ctx, db, r, p, f); err != nil {
			return err
		}
	}
	s.lastShares, s.lastReviewers, s.lastFailed = shares, s.reviewers, !pass
	s.weights, s.reviewers = map[string]float64{}, nil
	return nil
}

func bump(ctx context.Context, db statsExecer, route, column string, n int) error {
	_, err := db.ExecContext(ctx, `INSERT INTO model_outcomes(route,`+column+`) VALUES(?,?) ON CONFLICT(route) DO UPDATE SET `+column+`=`+column+`+excluded.`+column, route, n)
	return err
}

func bumpAuthored(ctx context.Context, db statsExecer, route string, pass, fail float64) error {
	_, err := db.ExecContext(ctx, `INSERT INTO model_outcomes(route,authored_pass,authored_fail) VALUES(?,?,?) ON CONFLICT(route) DO UPDATE SET authored_pass=authored_pass+excluded.authored_pass, authored_fail=authored_fail+excluded.authored_fail`, route, pass, fail)
	return err
}

// ModelOutcomes returns the per-route outcome stats ordered by route.
func (s *Store) ModelOutcomes(ctx context.Context) ([]ModelOutcome, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT route,authored_pass,authored_fail,reviews,overturned,answer_rejected,run_pass,run_fail,run_input_required,run_cancelled FROM model_outcomes ORDER BY route`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelOutcome
	for rows.Next() {
		var x ModelOutcome
		if err := rows.Scan(&x.Route, &x.AuthoredPass, &x.AuthoredFail, &x.Reviews, &x.Overturned, &x.AnswerRejected, &x.RunPass, &x.RunFail, &x.RunInputRequired, &x.RunCancelled); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// backfillModelOutcomes replays historical events once, when no outcomes exist.
func (s *Store) backfillModelOutcomes() error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM model_outcomes`).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return tx.Commit()
	}
	type historical struct{ session, kind, data string }
	rows, err := tx.Query(`SELECT session_id,type,data_json FROM events ORDER BY id`)
	if err != nil {
		return err
	}
	var events []historical
	for rows.Next() {
		var ev historical
		if err := rows.Scan(&ev.session, &ev.kind, &ev.data); err != nil {
			rows.Close()
			return err
		}
		events = append(events, ev)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	p := newOutcomeProjector()
	for _, ev := range events {
		if err := p.apply(ctx, tx, ev.session, ev.kind, []byte(ev.data)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
