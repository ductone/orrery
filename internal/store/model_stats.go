package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

const modelStatsSchema = `CREATE TABLE IF NOT EXISTS model_stats(route TEXT PRIMARY KEY, calls INTEGER NOT NULL DEFAULT 0, latency_seconds REAL NOT NULL DEFAULT 0, output_tokens_per_second REAL NOT NULL DEFAULT 0, truncated INTEGER NOT NULL DEFAULT 0, empty INTEGER NOT NULL DEFAULT 0, malformed INTEGER NOT NULL DEFAULT 0, provider_errors INTEGER NOT NULL DEFAULT 0, input_tokens INTEGER NOT NULL DEFAULT 0, cache_read_tokens INTEGER NOT NULL DEFAULT 0, last_call TEXT NOT NULL DEFAULT '', last_slow_call TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL);`

type ModelStat struct {
	Route                                       string
	Calls                                       int
	LatencySeconds, OutputTokensPerSecond       float64
	InputTokens, CacheReadTokens                int
	Truncated, Empty, Malformed, ProviderErrors int
	LastCall, LastSlowCall, UpdatedAt           time.Time
}

type statsExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func recordModelCall(ctx context.Context, db statsExecer, route string, latency time.Duration, outputTokens int, truncated bool, inputTokens, cacheReadTokens int, at time.Time) error {
	seconds := latency.Seconds()
	rate := 0.0
	if seconds > 0 {
		rate = float64(outputTokens) / seconds
	}
	stamp := at.UTC().Format(time.RFC3339Nano)
	slow := ""
	if latency > 120*time.Second {
		slow = stamp
	}
	_, err := db.ExecContext(ctx, `INSERT INTO model_stats(route,calls,latency_seconds,output_tokens_per_second,truncated,input_tokens,cache_read_tokens,last_call,last_slow_call,updated_at) VALUES(?,1,?,?,?,?,?,?,?,?) ON CONFLICT(route) DO UPDATE SET calls=calls+1, latency_seconds=CASE WHEN calls=0 THEN excluded.latency_seconds ELSE 0.9*latency_seconds+0.1*excluded.latency_seconds END, output_tokens_per_second=CASE WHEN calls=0 THEN excluded.output_tokens_per_second ELSE 0.9*output_tokens_per_second+0.1*excluded.output_tokens_per_second END, truncated=truncated+excluded.truncated, input_tokens=input_tokens+excluded.input_tokens, cache_read_tokens=cache_read_tokens+excluded.cache_read_tokens, last_call=excluded.last_call, last_slow_call=CASE WHEN excluded.last_slow_call='' THEN last_slow_call ELSE excluded.last_slow_call END, updated_at=excluded.updated_at`, route, seconds, rate, truncated, inputTokens, cacheReadTokens, stamp, slow, stamp)
	return err
}

func (s *Store) RecordModelCall(ctx context.Context, route string, latency time.Duration, outputTokens int, truncated bool, inputTokens, cacheReadTokens int) error {
	return recordModelCall(ctx, s.db, route, latency, outputTokens, truncated, inputTokens, cacheReadTokens, time.Now())
}

func recordModelFailure(ctx context.Context, db statsExecer, route, kind string, at time.Time) error {
	column := ""
	switch kind {
	case "empty":
		column = "empty"
	case "malformed":
		column = "malformed"
	case "provider_error":
		column = "provider_errors"
	default:
		return fmt.Errorf("unknown model failure kind %q", kind)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO model_stats(route,`+column+`,updated_at) VALUES(?,1,?) ON CONFLICT(route) DO UPDATE SET `+column+`=`+column+`+1,updated_at=excluded.updated_at`, route, at.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) RecordModelFailure(ctx context.Context, route, kind string) error {
	return recordModelFailure(ctx, s.db, route, kind, time.Now())
}

func (s *Store) ModelStats(ctx context.Context) ([]ModelStat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT route,calls,latency_seconds,output_tokens_per_second,truncated,empty,malformed,provider_errors,input_tokens,cache_read_tokens,last_call,last_slow_call,updated_at FROM model_stats ORDER BY route`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelStat
	for rows.Next() {
		var x ModelStat
		var last, slow, updated string
		if err := rows.Scan(&x.Route, &x.Calls, &x.LatencySeconds, &x.OutputTokensPerSecond, &x.Truncated, &x.Empty, &x.Malformed, &x.ProviderErrors, &x.InputTokens, &x.CacheReadTokens, &last, &slow, &updated); err != nil {
			return nil, err
		}
		x.LastCall, _ = time.Parse(time.RFC3339Nano, last)
		x.LastSlowCall, _ = time.Parse(time.RFC3339Nano, slow)
		x.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, x)
	}
	return out, rows.Err()
}

// Backfill in event order, retaining the active route per session for older
// completion.rejected events that did not include a model.
func (s *Store) backfillModelStats() error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM model_stats`).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return tx.Commit()
	}
	rows, err := tx.Query(`SELECT session_id,type,data_json,created_at FROM events ORDER BY id`)
	if err != nil {
		return err
	}
	type historical struct{ session, kind, data, stamp string }
	var events []historical
	for rows.Next() {
		var ev historical
		if err := rows.Scan(&ev.session, &ev.kind, &ev.data, &ev.stamp); err != nil {
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
	active := map[string]string{}
	for _, ev := range events {
		var data struct {
			Model           string         `json:"model"`
			Reason          string         `json:"reason"`
			Latency         *time.Duration `json:"latency"`
			OutputTokens    int            `json:"output_tokens"`
			InputTokens     int            `json:"input_tokens"`
			CacheReadTokens int            `json:"cache_read_tokens"`
			Truncated       bool           `json:"truncated"`
			Decision        struct {
				Model struct {
					ID string `json:"id"`
				} `json:"model"`
			} `json:"decision"`
		}
		if json.Unmarshal([]byte(ev.data), &data) != nil {
			continue
		}
		if data.Decision.Model.ID != "" {
			active[ev.session] = data.Decision.Model.ID
		}
		if data.Model != "" {
			active[ev.session] = data.Model
		}
		route := data.Model
		if route == "" && ev.kind == "completion.rejected" {
			route = active[ev.session]
		}
		if route == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, ev.stamp)
		if err != nil {
			return err
		}
		switch ev.kind {
		case "usage.reported":
			// Compaction events have no latency; they cannot inform call stats.
			if data.Latency != nil {
				err = recordModelCall(ctx, tx, route, *data.Latency, data.OutputTokens, data.Truncated, data.InputTokens, data.CacheReadTokens, at)
			}
		case "provider.error":
			err = recordModelFailure(ctx, tx, route, "provider_error", at)
		case "completion.rejected":
			switch data.Reason {
			case "empty assistant response":
				err = recordModelFailure(ctx, tx, route, "empty", at)
			case "malformed tool-call arguments":
				err = recordModelFailure(ctx, tx, route, "malformed", at)
			}
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
