package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommandLogIDSurvivesRegistryRebuild(t *testing.T) {
	for _, command := range []string{"printf marker", "printf marker; exit 7"} {
		t.Run(command, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			root := t.TempDir()
			state := &SessionState{}
			first := NewWithState(root, state)
			result, runErr := first.Call(ctx, "exec", map[string]any{"command": command})
			failed := strings.Contains(command, "exit 7")
			if (runErr != nil) != failed {
				t.Fatalf("exec error = %v", runErr)
			}
			log := result.(map[string]any)["log"].(string)
			id := strings.TrimSuffix(filepath.Base(log), ".log")
			next := NewWithState(root, state)
			logs, err := next.Call(ctx, "job", map[string]any{"id": id, "action": "logs"})
			if err != nil || !strings.Contains(logs.(map[string]any)["summary"].(string), "marker") {
				t.Fatalf("logs = %v, error = %v", logs, err)
			}
			for range 2 {
				wait, err := next.Call(ctx, "job", map[string]any{"id": id, "action": "wait"})
				if (err != nil) != failed || wait.(map[string]any)["ok"] != !failed {
					t.Fatalf("wait = %v, error = %v", wait, err)
				}
			}
			other := NewWithState(root, &SessionState{})
			for _, unknown := range []string{id, "cmd-unknown", "../" + id} {
				for _, action := range []string{"logs", "wait"} {
					if _, err := other.Call(ctx, "job", map[string]any{"id": unknown, "action": action}); err == nil {
						t.Fatalf("another session accepted %q for %s", unknown, action)
					}
				}
			}
		})
	}
}

func TestBackgroundCommandWaitSurvivesRegistryRebuild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	state := &SessionState{}
	first := NewWithState(root, state)
	result, err := first.Call(ctx, "exec", map[string]any{"command": "sleep 0.1; printf background; exit 7", "background": true})
	if err != nil {
		t.Fatal(err)
	}
	id := result.(map[string]any)["id"].(string)
	next := NewWithState(root, state)
	for range 2 {
		result, err := next.Call(ctx, "job", map[string]any{"id": id, "action": "wait"})
		if err == nil || !strings.Contains(err.Error(), "exit status 7") || !strings.Contains(result.(map[string]any)["summary"].(string), "background") {
			t.Fatalf("wait = %v, error = %v", result, err)
		}
	}
}
