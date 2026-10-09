package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandSummaryInlineAndElision(t *testing.T) {
	for _, count := range []int{300, 400, 600} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "output.log")
			var output strings.Builder
			for i := 1; i <= count; i++ {
				fmt.Fprintf(&output, "line %03d\n", i)
			}
			if err := os.WriteFile(path, []byte(output.String()), 0600); err != nil {
				t.Fatal(err)
			}
			value, err := commandSummary(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			summary := value.(map[string]any)["summary"].(string)
			if count <= 400 {
				if summary != output.String() {
					t.Fatal("inline output was not returned whole")
				}
				return
			}
			if !strings.HasPrefix(summary, "line 001\n") || !strings.HasSuffix(summary, "line 600") {
				t.Fatal("summary lost head or tail")
			}
			want := fmt.Sprintf("lines 201-400 omitted; read path=%q start=201 limit=200", path)
			if !strings.Contains(summary, want) {
				t.Fatalf("summary missing read range: %s", summary)
			}
			v, err := New(root).Call(context.Background(), "read", map[string]any{"path": path, "start": 201, "limit": 200})
			if err != nil || v == nil {
				t.Fatalf("suggested read failed: %v", err)
			}
		})
	}
}

func TestCommandSummaryCharacterLimit(t *testing.T) {
	for _, output := range []string{
		strings.Repeat("abcdefghij\n", 3000),
		"head" + strings.Repeat("x", 40000) + "tail",
		strings.Repeat("x", 40000) + "\nmiddle\n" + strings.Repeat("y", 40000),
	} {
		path := filepath.Join(t.TempDir(), "output.log")
		if err := os.WriteFile(path, []byte(output), 0600); err != nil {
			t.Fatal(err)
		}
		value, err := commandSummary(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		summary := value.(map[string]any)["summary"].(string)
		if len(summary) > commandSummaryMaxChars+1000 || !strings.Contains(summary, "read path=") || !strings.Contains(summary, "start=") {
			t.Fatalf("unbounded or unactionable summary (%d bytes)", len(summary))
		}
		if !strings.HasPrefix(summary, output[:4]) || !strings.HasSuffix(summary, strings.TrimSuffix(output, "\n")[len(strings.TrimSuffix(output, "\n"))-4:]) {
			t.Fatal("summary lost head or tail")
		}
	}
}

func TestReadOutsideWorkspaceSuggestsScratchPath(t *testing.T) {
	r := New(t.TempDir())
	_, err := r.Call(context.Background(), "read", map[string]any{"path": "../scratch.txt"})
	if err == nil || !strings.Contains(err.Error(), "path escapes workspace") || !strings.Contains(err.Error(), ".orrery/") {
		t.Fatalf("missing scratch guidance: %v", err)
	}
}
