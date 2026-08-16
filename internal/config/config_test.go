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
