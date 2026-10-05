package settings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOK(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(p, []byte(`{"name":"a","workers":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil || s.Name != "a" || s.Workers != 3 {
		t.Fatalf("Load = %+v, %v", s, err)
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want errors.Is os.ErrNotExist", err)
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || err == pathErr {
		t.Fatalf("err = %v, want contextual wrapper preserving path error", err)
	}
}

func TestLoadBadJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(p, []byte(`{"name":`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	var syn *json.SyntaxError
	if !errors.As(err, &syn) {
		t.Fatalf("err = %v, want wrapped json syntax error", err)
	}
	if !errors.Is(err, syn) || err == syn {
		t.Fatalf("err = %v, want contextual wrapper preserving json error", err)
	}
}
