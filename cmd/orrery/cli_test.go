package main

import (
	"os"
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/config"
)

func TestClosestCommand(t *testing.T) {
	for typo, want := range map[string]string{"serv": "serve", "tiu": "tui", "shadwo": "shadow", "deploy": ""} {
		if got := closestCommand(typo); got != want {
			t.Errorf("closestCommand(%q) = %q, want %q", typo, got, want)
		}
	}
}

func TestRequireProviders(t *testing.T) {
	missing := configRef{searched: []string{"orrery.yaml", "/h/.orrery/orrery.yaml"}}
	if err := missing.requireProviders(config.Default()); err == nil || !strings.Contains(err.Error(), "/h/.orrery/orrery.yaml") {
		t.Fatalf("err = %v", err)
	}
	empty := configRef{path: "x.yaml", found: true}
	if err := empty.requireProviders(config.Default()); err == nil || !strings.Contains(err.Error(), "x.yaml") {
		t.Fatalf("err = %v", err)
	}
	cfg := config.Default()
	cfg.Providers["ramp"] = config.ProviderConfig{APIKey: "k"}
	if err := empty.requireProviders(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestBareOrreryRefusesWithoutATerminal(t *testing.T) {
	// go test's stdin and stdout are not terminals.
	old := os.Args
	defer func() { os.Args = old }()
	os.Args = []string{"orrery"}
	if code := realMain(); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	os.Args = []string{"orrery", "-p", "fix it", "run"}
	if code := realMain(); code != 2 {
		t.Fatalf("-p before a command must be rejected, exit = %d", code)
	}
	os.Args = []string{"orrery", "serv"}
	if code := realMain(); code != 2 {
		t.Fatalf("unknown command exit = %d", code)
	}
}
