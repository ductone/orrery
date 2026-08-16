package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeReloadAppliesAtBoundaryWithEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orrery.yaml")
	write := func(instruction string) {
		content := "database: '" + filepath.Join(dir, "db.sqlite") + "'\nworkspace_root: '" + dir + "'\nproviders:\n  openai:\n    api_key: '!env OPENAI_API_KEY'\ninstructions: ['" + instruction + "']\nbudget: {session_usd: 1, job_default_fraction: 0.2}\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OPENAI_API_KEY", "initial")
	write("old")
	runtime, err := openRuntime(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.close(context.Background())
	write("new")
	runtime.queueReload(map[string]string{"OPENAI_API_KEY": "rotated"})
	if runtime.cfg.Instructions[0] != "old" {
		t.Fatal("queued reload applied before a phase boundary")
	}
	if err := runtime.phaseBoundary(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.cfg.Instructions[0] != "new" || runtime.cfg.Providers["openai"].APIKey != "rotated" {
		t.Fatalf("reloaded config = %#v", runtime.cfg)
	}
}
