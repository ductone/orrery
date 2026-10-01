package main

import (
	"errors"
	"io"
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

func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	f()
	w.Close()
	os.Stderr = old
	b, _ := io.ReadAll(r)
	return string(b)
}

func TestFinishTUIPrintsHowToResume(t *testing.T) {
	var code int
	out := captureStderr(t, func() { code = finishTUI("3c18ec20", nil, "orrery --session ") })
	if code != 0 || !strings.Contains(out, "To resume this session, run: orrery --session 3c18ec20") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	out = captureStderr(t, func() { code = finishTUI("3c18ec20", errors.New("boom"), "orrery --session ") })
	if code != 1 || !strings.Contains(out, "boom") || !strings.Contains(out, "orrery --session 3c18ec20") {
		t.Fatalf("an error still says how to resume: code=%d out=%q", code, out)
	}
	if out := captureStderr(t, func() { finishTUI("", nil, "orrery --session ") }); out != "" {
		t.Fatalf("no session, no hint: %q", out)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/home/dev/.orrery/orrery.yaml": "/home/dev/.orrery/orrery.yaml",
		"http://127.0.0.1:7433":         "http://127.0.0.1:7433",
		"my config.yaml":                "'my config.yaml'",
		"it's.yaml":                     `'it'\''s.yaml'`,
		"":                              "''",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSessionBeforeACommandIsRejected(t *testing.T) {
	old := os.Args
	defer func() { os.Args = old }()
	os.Args = []string{"orrery", "--session", "abc", "run"}
	var code int
	out := captureStderr(t, func() { code = realMain() })
	if code != 2 || !strings.Contains(out, "orrery --session abc") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	// Bare --session still needs a terminal, like bare orrery.
	os.Args = []string{"orrery", "--session", "abc"}
	out = captureStderr(t, func() { code = realMain() })
	if code != 2 || !strings.Contains(out, "needs a terminal") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}
