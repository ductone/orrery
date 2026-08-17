package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGoCompileCheck(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go command not available:", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testmod\n\ngo 1.23\n"), 0644); err != nil {
		t.Fatal(err)
	}
	valid := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(valid), 0644); err != nil {
		t.Fatal(err)
	}

	output, ok, ran := goCompileCheck(context.Background(), dir)
	if !ran {
		t.Fatal("expected check to run")
	}
	if !ok {
		t.Fatalf("expected build to pass, got output:\n%s", output)
	}

	broken := "package main\n\nfunc main() {\n\tthis is not valid go\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(broken), 0644); err != nil {
		t.Fatal(err)
	}
	output, ok, ran = goCompileCheck(context.Background(), dir)
	if !ran {
		t.Fatal("expected check to run")
	}
	if ok {
		t.Fatal("expected build to fail")
	}
	if output == "" {
		t.Fatal("expected non-empty compiler output")
	}
}
