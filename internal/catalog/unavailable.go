package catalog

import (
	"encoding/json"

	"github.com/ductone/orrey/internal/config"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// refusalTTL is how long a refused model stays out of discovery. Refusals
// are usually about the account (a provider key it lacks), which can change,
// so they expire rather than accumulate.
const refusalTTL = 7 * 24 * time.Hour

type refusal struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

var refusalMu sync.Mutex

// Dir is where discovery caches listings and records refusals.
func Dir() string { return filepath.Join(config.Home(), "catalog") }

func refusalPath(dir string) string { return filepath.Join(dir, "unavailable.json") }

// MarkUnavailable records that a provider refused a model for this account,
// so discovery leaves it out until the refusal expires.
func MarkUnavailable(dir, id, reason string) error {
	if dir == "" {
		return nil
	}
	refusalMu.Lock()
	defer refusalMu.Unlock()
	all := readRefusals(dir)
	all[id] = refusal{Reason: reason, At: time.Now().UTC()}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(all)
	if err != nil {
		return err
	}
	tmp := refusalPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, refusalPath(dir))
}

// Unavailable returns the models refused within refusalTTL of now.
func Unavailable(dir string, now time.Time) map[string]string {
	refusalMu.Lock()
	defer refusalMu.Unlock()
	out := map[string]string{}
	for id, r := range readRefusals(dir) {
		if now.Sub(r.At) < refusalTTL {
			out[id] = r.Reason
		}
	}
	return out
}

func readRefusals(dir string) map[string]refusal {
	all := map[string]refusal{}
	if dir == "" {
		return all
	}
	if b, err := os.ReadFile(refusalPath(dir)); err == nil {
		_ = json.Unmarshal(b, &all)
	}
	return all
}
