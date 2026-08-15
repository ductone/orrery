package tools

import (
	"context"
	"strings"
	"testing"
)

func TestExecFailureIsAnError(t *testing.T) {
	r := New(t.TempDir())
	v, err := r.Call(context.Background(), "exec", map[string]any{"command": "echo marker; exit 7"})
	if err == nil || !strings.Contains(err.Error(), "marker") {
		t.Fatalf("value=%v error=%v", v, err)
	}
}
func TestReadScheme(t *testing.T) {
	r := New(t.TempDir())
	r.AddScheme("job", func(context.Context, map[string]any) (any, error) { return "ok", nil })
	v, err := r.Call(context.Background(), "read", map[string]any{"path": "job://123/result.answer"})
	if err != nil || v != "ok" {
		t.Fatalf("%v %v", v, err)
	}
}
