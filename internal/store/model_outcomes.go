package store

import (
	"context"

	"github.com/ductone/orrery/internal/outcomes"
)

const modelOutcomesSchema = `CREATE TABLE IF NOT EXISTS model_outcomes(route TEXT PRIMARY KEY, authored_pass REAL NOT NULL DEFAULT 0, authored_fail REAL NOT NULL DEFAULT 0, reviews INTEGER NOT NULL DEFAULT 0, overturned INTEGER NOT NULL DEFAULT 0, answer_rejected INTEGER NOT NULL DEFAULT 0, run_pass INTEGER NOT NULL DEFAULT 0, run_fail INTEGER NOT NULL DEFAULT 0, run_input_required INTEGER NOT NULL DEFAULT 0, run_cancelled INTEGER NOT NULL DEFAULT 0);`

// RecordModelOutcomes adds each delta's counts to its route's totals.
func (s *Store) RecordModelOutcomes(ctx context.Context, deltas []ModelOutcome) error {
	if len(deltas) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := recordOutcomes(ctx, tx, deltas); err != nil {
		return err
	}
	return tx.Commit()
}

func recordOutcomes(ctx context.Context, db statsExecer, deltas []ModelOutcome) error {
	for _, d := range deltas {
		if _, err := db.ExecContext(ctx, `INSERT INTO model_outcomes(route,authored_pass,authored_fail,reviews,overturned,answer_rejected,run_pass,run_fail,run_input_required,run_cancelled) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(route) DO UPDATE SET authored_pass=authored_pass+excluded.authored_pass, authored_fail=authored_fail+excluded.authored_fail, reviews=reviews+excluded.reviews, overturned=overturned+excluded.overturned, answer_rejected=answer_rejected+excluded.answer_rejected, run_pass=run_pass+excluded.run_pass, run_fail=run_fail+excluded.run_fail, run_input_required=run_input_required+excluded.run_input_required, run_cancelled=run_cancelled+excluded.run_cancelled`,
			d.Route, d.AuthoredPass, d.AuthoredFail, d.Reviews, d.Overturned, d.AnswerRejected, d.RunPass, d.RunFail, d.RunInputRequired, d.RunCancelled); err != nil {
			return err
		}
	}
	return nil
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
	p := outcomes.New()
	for _, ev := range events {
		if err := recordOutcomes(ctx, tx, p.Apply(ev.session, ev.kind, []byte(ev.data))); err != nil {
			return err
		}
	}
	return tx.Commit()
}
