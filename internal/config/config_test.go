package config

import (
	"os"
	"path/filepath"
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
	for _, phase := range d.Router.FrontierFloorPhases {
		if phase == "review" {
			t.Fatal("review must not be in the default frontier_floor_phases")
		}
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
	if len(cfg.Router.FrontierFloorPhases) != 2 {
		t.Fatalf("frontier floor phases = %v, want the 2 defaults", cfg.Router.FrontierFloorPhases)
	}
	if cfg.Budget.SessionUSD != 5 {
		t.Fatalf("session budget override lost: %v", cfg.Budget.SessionUSD)
	}
}

func TestInterventionDefaultsAndValidation(t *testing.T) {
	d := Default()
	if !d.Interventions.Enabled() {
		t.Fatal("the judge must be on by default")
	}
	if d.Interventions.Backoff() != 1.5 {
		t.Fatalf("default backoff = %v, want 1.5", d.Interventions.Backoff())
	}
	if d.Interventions.JudgeTimeout() != 20*time.Second {
		t.Fatalf("default judge timeout = %v, want 20s", d.Interventions.JudgeTimeout())
	}

	dir := t.TempDir()
	write := func(body string) (Config, error) {
		path := filepath.Join(dir, "c.yaml")
		if err := os.WriteFile(path, []byte("listen: '127.0.0.1:1'\ndatabase: 'x.db'\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}

	// A backoff at or below 1 sets a floor no higher than the value that tripped,
	// so the judge would be re-asked every turn forever.
	if _, err := write("interventions:\n  judge_backoff: 1.0\n"); err == nil {
		t.Fatal("judge_backoff of 1.0 must be rejected")
	}
	if _, err := write("interventions:\n  judge_timeout_seconds: -1\n"); err == nil {
		t.Fatal("negative judge timeout must be rejected")
	}

	cfg, err := write("interventions:\n  judge_enabled: false\n  judge_backoff: 2.5\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interventions.Enabled() {
		t.Fatal("judge_enabled: false must disable the judge")
	}
	if cfg.Interventions.Backoff() != 2.5 {
		t.Fatalf("backoff override lost: %v", cfg.Interventions.Backoff())
	}
}

func TestJevShadowConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) (Config, error) {
		path := filepath.Join(dir, "c.yaml")
		if err := os.WriteFile(path, []byte("listen: '127.0.0.1:1'\ndatabase: 'x.db'\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	if d := Default(); d.Jev.Shadows("phase") {
		t.Fatal("shadowing must be off by default")
	}
	if _, err := write("jev:\n  api_key: k\n  shadow: [phaze]\n"); err == nil {
		t.Fatal("an unknown shadow site must be rejected")
	}
	if _, err := write("jev:\n  shadow: [phase]\n"); err == nil {
		t.Fatal("shadow sites without an api key must be rejected")
	}
	t.Setenv("ORRERY_TEST_JEV_KEY", " secret ")
	cfg, err := write("jev:\n  api_key: '!env ORRERY_TEST_JEV_KEY'\n  shadow: [phase, review]\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Jev.APIKey != "secret" || !cfg.Jev.Shadows("phase") || cfg.Jev.Shadows("stall_judge") {
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
