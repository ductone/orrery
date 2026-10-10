package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadStrictAndSecrets(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	t.Setenv("ORRERY_TEST_API_KEY", "secret")
	if err := os.WriteFile(p, []byte("listen: '127.0.0.1:1'\ndatabase: '"+filepath.Join(dir, "x.db")+"'\nworkspace_root: '"+dir+"'\nproviders:\n  openai:\n    api_key: '!env ORRERY_TEST_API_KEY'\nbudget: {session_usd: 1, job_default_fraction: 0.2}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Providers["openai"].APIKey != "secret" {
		t.Fatal("secret not resolved")
	}
	if err = os.WriteFile(p, []byte("unknown: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(p); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestSessionTokenLimitOptional(t *testing.T) {
	// Unset/zero falls back to the default.
	if got := (BudgetConfig{}).SessionTokenLimit(); got != defaultSessionTokens {
		t.Fatalf("default = %d, want %d", got, defaultSessionTokens)
	}
	if got := (BudgetConfig{SessionTokens: 250000}).SessionTokenLimit(); got != 250000 {
		t.Fatalf("explicit = %d, want 250000", got)
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	// session_tokens is optional and accepted when present.
	if err := os.WriteFile(p, []byte("budget: {session_usd: 1, job_default_fraction: 0.2, session_tokens: 250000}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil || c.Budget.SessionTokens != 250000 {
		t.Fatalf("load=%+v err=%v", c.Budget, err)
	}
	// A negative value is rejected.
	if err := os.WriteFile(p, []byte("budget: {session_usd: 1, job_default_fraction: 0.2, session_tokens: -5}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(p); err == nil {
		t.Fatal("negative session_tokens accepted")
	}
}

func TestLoadWithEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orrery.yaml")
	content := "database: '" + filepath.Join(dir, "db.sqlite") + "'\nproviders:\n  openai:\n    api_key: '!env OPENAI_API_KEY'\nbudget: {session_usd: 1, job_default_fraction: 0.2}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "old")
	cfg, err := LoadWithEnv(path, map[string]string{"OPENAI_API_KEY": "rotated"})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Providers["openai"].APIKey; got != "rotated" {
		t.Fatalf("API key = %q, want override", got)
	}
}

func TestBudgetDefaultsAndReviewFloor(t *testing.T) {
	d := Default()
	if d.Budget.ReviewFloorUSD() != 2 {
		t.Fatalf("default review floor = %v, want 2", d.Budget.ReviewFloorUSD())
	}
	if d.Budget.SessionUSD != 25 {
		t.Fatalf("default session budget = %v, want 25", d.Budget.SessionUSD)
	}

	// A config that sets other budget fields must not silently reset the
	// review floor: it is the shape our own orrery.yaml uses.
	dir := t.TempDir()
	path := filepath.Join(dir, "orrery.yaml")
	if err := os.WriteFile(path, []byte("listen: \"127.0.0.1:7433\"\ndatabase: \".orrery/orrery.db\"\nbudget:\n  session_usd: 5\n  job_default_fraction: 0.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Budget.ReviewFloorUSD() != 2 {
		t.Fatalf("review floor after partial budget override = %v, want 2", cfg.Budget.ReviewFloorUSD())
	}
	if len(cfg.Router.FrontierFloorPhases) != 3 {
		t.Fatalf("frontier floor phases = %v, want the 3 defaults", cfg.Router.FrontierFloorPhases)
	}
	if cfg.Budget.SessionUSD != 5 {
		t.Fatalf("session budget override lost: %v", cfg.Budget.SessionUSD)
	}
}

func TestRemovedInterventionsBlockStillLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	body := "listen: '127.0.0.1:1'\ndatabase: 'x.db'\ninterventions:\n  judge_enabled: false\n  judge_backoff: 2.5\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}

func TestJevConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) (Config, error) {
		path := filepath.Join(dir, "c.yaml")
		if err := os.WriteFile(path, []byte("listen: '127.0.0.1:1'\ndatabase: 'x.db'\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	if _, err := write("jev:\n  api_key: k\n  shadow: [phase]\n"); err == nil {
		t.Fatal("the removed jev.shadow field must be rejected")
	}
	t.Setenv("ORRERY_TEST_JEV_KEY", " secret ")
	cfg, err := write("jev:\n  api_key: '!env ORRERY_TEST_JEV_KEY'\n  review: true\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Jev.APIKey != "secret" || !cfg.Jev.Review {
		t.Fatalf("jev = %+v", cfg.Jev)
	}
	if _, err := write("jev:\n  routing: true\n"); err == nil {
		t.Fatal("jev routing without an api key must be rejected")
	}
	if _, err := write("jev:\n  review: true\n"); err == nil {
		t.Fatal("jev review without an api key must be rejected")
	}
	if _, err := write("jev:\n  search_ranking: true\n"); err == nil {
		t.Fatal("search ranking without an api key must be rejected")
	}
	if cfg.Jev.Timeout() != 5*time.Second {
		t.Fatalf("default timeout = %v", cfg.Jev.Timeout())
	}
}

func TestEffectiveJev(t *testing.T) {
	t.Setenv("ORRERY_TEST_RAMP_KEY", " ramp-secret ")
	t.Setenv("ORRERY_TEST_JEV_KEY", " jev-secret ")
	for _, tt := range []struct {
		name, body, key, baseURL string
		review                   bool
	}{
		{"ramp", "providers:\n  ramp: {api_key: '!env ORRERY_TEST_RAMP_KEY'}\njev: {review: true}\n", "ramp-secret", "https://api.router.com", true},
		{"explicit", "providers:\n  ramp: {api_key: ramp}\njev: {api_key: '!env ORRERY_TEST_JEV_KEY', base_url: 'https://jev.example', review: true}\n", "jev-secret", "https://jev.example", true},
		{"direct default", "providers:\n  ramp: {api_key: ramp}\njev: {api_key: direct, review: true}\n", "direct", "", true},
		{"ramp base", "providers:\n  ramp: {api_key: ramp, base_url: 'https://router.example'}\njev: {review: true}\n", "ramp", "https://router.example", true},
		{"base override", "providers:\n  ramp: {api_key: ramp, base_url: 'https://router.example'}\njev: {base_url: 'https://jev.example', review: true}\n", "ramp", "https://jev.example", true},
		{"ramp keys", "providers:\n  ramp: {api_keys: ['', '!env ORRERY_TEST_RAMP_KEY']}\njev: {review: true}\n", "ramp-secret", "https://api.router.com", true},
		{"switches off", "providers:\n  ramp: {api_key: ramp}\n", "ramp", "https://api.router.com", false},
		{"disabled", "jev: {}\n", "", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			j := cfg.EffectiveJev()
			if j.APIKey != tt.key || j.BaseURL != tt.baseURL || j.Review != tt.review {
				t.Fatalf("effective Jev = %+v", j)
			}
			if j.Routing || j.SearchRanking {
				t.Fatal("fallback must not enable feature switches")
			}
		})
	}
}

func TestResolvePrecedence(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("ORRERY_HOME", home)
	t.Setenv("ORRERY_CONFIG", "")
	t.Chdir(cwd)
	if _, found, searched, err := Resolve(""); err != nil || found || len(searched) != 2 {
		t.Fatalf("found=%v searched=%v err=%v", found, searched, err)
	}
	user := filepath.Join(home, "orrery.yaml")
	if err := os.WriteFile(user, []byte("budget: {session_usd: 1, job_default_fraction: 0.2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, found, _, _ := Resolve(""); !found || path != user {
		t.Fatalf("user config not found: %q", path)
	}
	if err := os.WriteFile(LocalConfig, []byte("budget: {session_usd: 1, job_default_fraction: 0.2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, _, _, _ := Resolve(""); path != LocalConfig {
		t.Fatalf("./orrery.yaml must override the user config, got %q", path)
	}
	t.Setenv("ORRERY_CONFIG", user)
	if path, _, _, _ := Resolve(""); path != user {
		t.Fatalf("$ORRERY_CONFIG must win over ./orrery.yaml, got %q", path)
	}
	if _, _, _, err := Resolve(filepath.Join(cwd, "missing.yaml")); err == nil {
		t.Fatal("an explicit config that does not exist must be an error")
	}
}

func TestDatabaseDefaultsToHomeAndRelativePathsFollowTheConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ORRERY_HOME", home)
	if got := Default().Database; got != filepath.Join(home, "orrery.db") {
		t.Fatalf("default database = %q", got)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "orrery.yaml")
	if err := os.WriteFile(path, []byte("database: state/orrery.db\nworkspace_root: /abs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database != filepath.Join(dir, "state", "orrery.db") || cfg.WorkspaceRoot != "/abs" {
		t.Fatalf("database=%q workspace_root=%q", cfg.Database, cfg.WorkspaceRoot)
	}
	cfg, _ = Load(filepath.Join(dir, "absent.yaml"))
	if cfg.Database != filepath.Join(home, "orrery.db") {
		t.Fatalf("running on defaults must use the home database, got %q", cfg.Database)
	}
}

func TestModelOverrides(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) (Config, error) {
		path := filepath.Join(dir, "c.yaml")
		if err := os.WriteFile(path, []byte("listen: '127.0.0.1:1'\ndatabase: 'x.db'\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	cfg, err := write(`models:
  - id: ramp/qwen4-coder
    tier: frontier
    family: qwen
    pricing: {output: 1.5, thresholds: [{above_tokens: 200000, input: 1, output: 3, cache_read: 0.1, cache_write: 0}]}
    compat: {supports_strict_tools: true, stream_idle_timeout: 5m, first_byte_timeout: 45s}
  - id: ramp/noisy
    disabled: true
`)
	if err != nil {
		t.Fatal(err)
	}
	q := cfg.Models[0]
	if *q.Tier != "frontier" || *q.Family != "qwen" || *q.Pricing.Output != 1.5 || q.Pricing.Input != nil || len(*q.Pricing.Thresholds) != 1 || !*q.Compat.SupportsStrictTools || q.Compat.StreamIdleTimeout.Minutes() != 5 || q.Compat.FirstByteTimeout.Seconds() != 45 {
		t.Fatalf("override = %+v", q)
	}
	if !*cfg.Models[1].Disabled {
		t.Fatal("disabled lost")
	}
	for body, want := range map[string]string{
		"models:\n  - id: ramp/\n":                                         "provider/model",
		"models:\n  - id: ramp/a\n  - id: ramp/a\n":                        "duplicate",
		"models:\n  - id: ramp/a\n    family: Not Valid\n":                 "invalid family",
		"models:\n  - id: ramp/a\n    tier: legendary\n":                   "unknown tier",
		"models:\n  - id: ramp/a\n    pricing: {input: -1}\n":              "non-negative",
		"models:\n  - id: ramp/a\n    context_window: 0\n":                 "positive",
		"models:\n  - id: ramp/a\n    effort: [maximum]\n":                 "unknown effort",
		"models:\n  - id: ramp/a\n    edit_dialect: diff\n":                "edit_dialect",
		"models:\n  - id: ramp/a\n    pricing: {inptu: 1}\n":               "inptu",
		"models:\n  - id: ramp/a\n    compat: {first_byte_timeout: -1s}\n": "first_byte_timeout",
	} {
		if _, err := write(body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want it to mention %q", body, err, want)
		}
	}
}

func TestLoadUnresolvedNeedsNoSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	body := "listen: '127.0.0.1:1'\ndatabase: 'x.db'\nproviders:\n  ramp: {api_key: '!env ORRERY_TEST_UNSET_KEY'}\njev:\n  api_key: '!cmd exit 1'\n  review: true\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a normal load resolves secrets and fails without them")
	}
	cfg, err := LoadUnresolved(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database != filepath.Join(dir, "x.db") || cfg.Providers["ramp"].APIKey != "!env ORRERY_TEST_UNSET_KEY" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if _, err := LoadUnresolved(filepath.Join(dir, "missing.yaml")); err != nil {
		t.Fatal("a missing file loads defaults, as Load does")
	}
}

func TestMemoryDefaults(t *testing.T) {
	d := Default()
	if !d.Memory.Inject {
		t.Fatal("memory inject must default to true")
	}
	if d.Memory.AutoCommit {
		t.Fatal("memory auto_commit must default to false")
	}
	if d.Memory.Records() != 8 {
		t.Fatalf("default max_records = %d, want 8", d.Memory.Records())
	}
	if d.Memory.Tokens() != 1200 {
		t.Fatalf("default max_tokens = %d, want 1200", d.Memory.Tokens())
	}
	if d.Memory.RecordBytes() != 2048 {
		t.Fatalf("default max_record_bytes = %d, want 2048", d.Memory.RecordBytes())
	}
	if d.Memory.ExpiresAfterDays() != 0 {
		t.Fatalf("default retain_days = %d, want 0 (no expiry)", d.Memory.ExpiresAfterDays())
	}
}

func TestMemoryStrictDecodeAndValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) (Config, error) {
		path := filepath.Join(dir, "c.yaml")
		if err := os.WriteFile(path, []byte("listen: '127.0.0.1:1'\ndatabase: 'x.db'\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}

	for _, enabled := range []bool{false, true} {
		cfg, err := write(fmt.Sprintf("memory:\n  enabled: %t\n", enabled))
		if err != nil {
			t.Fatalf("legacy memory.enabled=%t must load: %v", enabled, err)
		}
		if cfg.Memory.LegacyEnabled == nil || *cfg.Memory.LegacyEnabled != enabled {
			t.Fatalf("legacy memory.enabled=%t was not decoded", enabled)
		}
	}

	if _, err := write("memory:\n  shadow: true\n"); err == nil {
		t.Fatal("removed memory.shadow must be rejected")
	}
	if _, err := write("memory:\n  bogus_field: true\n"); err == nil {
		t.Fatal("an unknown memory field must be rejected")
	}
	if _, err := write("memory:\n  jev:\n    selection: true\n"); err == nil {
		t.Fatal("the removed memory.jev block must be rejected")
	}
	if _, err := write("memory:\n  retain_days: -1\n"); err == nil {
		t.Fatal("negative retain_days must be rejected")
	}
	cfg, err := write("jev:\n  api_key: k\nmemory:\n  inject: false\n  auto_commit: true\n  max_records: 20\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Memory.Inject || !cfg.Memory.AutoCommit {
		t.Fatalf("memory overrides lost: %+v", cfg.Memory)
	}
	if cfg.Memory.Records() != 20 {
		t.Fatalf("max_records override lost: %d", cfg.Memory.Records())
	}
}

func TestMemoryClamping(t *testing.T) {
	// Over-ceiling values clamp down; retain_days=0 keeps meaning "no expiry"
	// rather than being clamped to the positive floor.
	m := MemoryConfig{MaxRecords: 10_000, MaxTokens: 10_000_000, MaxRecordBytes: 10_000_000, RetainDays: 0}
	if got := m.Records(); got != 64 {
		t.Fatalf("max_records clamp = %d, want 64", got)
	}
	if got := m.Tokens(); got != 20_000 {
		t.Fatalf("max_tokens clamp = %d, want 20000", got)
	}
	if got := m.RecordBytes(); got != 32_768 {
		t.Fatalf("max_record_bytes clamp = %d, want 32768", got)
	}
	if got := m.ExpiresAfterDays(); got != 0 {
		t.Fatalf("retain_days=0 must stay 0 (no expiry), got %d", got)
	}
	// A very large positive retain_days must clamp to the ceiling instead of
	// being passed through unbounded, to keep expiry timestamp math safe.
	if got := (MemoryConfig{RetainDays: 1_000_000}).ExpiresAfterDays(); got != maxMemoryRetainDays {
		t.Fatalf("retain_days clamp = %d, want %d", got, maxMemoryRetainDays)
	}
	// Negative/zero values fall back to defaults rather than clamping to the floor.
	z := MemoryConfig{MaxRecords: -1, MaxTokens: 0, MaxRecordBytes: -5}
	if got := z.Records(); got != defaultMemoryMaxRecords {
		t.Fatalf("negative max_records = %d, want default %d", got, defaultMemoryMaxRecords)
	}
	if got := z.Tokens(); got != defaultMemoryMaxTokens {
		t.Fatalf("zero max_tokens = %d, want default %d", got, defaultMemoryMaxTokens)
	}
	if got := z.RecordBytes(); got != defaultMemoryMaxRecordBytes {
		t.Fatalf("negative max_record_bytes = %d, want default %d", got, defaultMemoryMaxRecordBytes)
	}

}
