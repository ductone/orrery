package config

import (
	"os"
	"path/filepath"
	"testing"
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
