package configload

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParse(t *testing.T) {
	cfg, err := Parse(strings.NewReader("# c\n\na = 1\nb=two\n"))
	if err != nil || cfg["a"] != "1" || cfg["b"] != "two" || len(cfg) != 2 {
		t.Fatalf("cfg=%v err=%v", cfg, err)
	}
	if _, err := Parse(strings.NewReader("a=1\nbroken\n")); err == nil {
		t.Fatal("want error")
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(write(t, dir, "a.conf", "x=1\n"))
	if err != nil || cfg["x"] != "1" {
		t.Fatalf("cfg=%v err=%v", cfg, err)
	}
	if _, err := Load(filepath.Join(dir, "missing.conf")); err == nil {
		t.Fatal("want error")
	}
}
