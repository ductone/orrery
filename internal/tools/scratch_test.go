package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScratchDirReadableAndExported(t *testing.T) {
	root := t.TempDir()
	scratch := filepath.Join(t.TempDir(), "orrery", "sid")
	r := New(root)
	r.SetScratch(scratch)
	out, err := r.Call(context.Background(), "exec", map[string]any{"command": `echo hi > "$ORRERY_SCRATCH/x"`})
	if err != nil {
		t.Fatal(err, out)
	}
	if _, err := r.Call(context.Background(), "read", map[string]any{"path": filepath.Join(scratch, "x")}); err != nil {
		t.Fatalf("read scratch: %v", err)
	}
	other := filepath.Join(t.TempDir(), "y")
	if err := os.WriteFile(other, []byte("a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = r.Call(context.Background(), "read", map[string]any{"path": other})
	if err == nil || !strings.Contains(err.Error(), "escapes workspace") {
		t.Fatalf("expected refusal, got %v", err)
	}
	_, err = r.Call(context.Background(), "read", map[string]any{"path": scratch + "/../../y"})
	if err == nil {
		t.Fatal("traversal out of scratch must be refused")
	}
}
