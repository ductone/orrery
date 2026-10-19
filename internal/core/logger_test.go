package core

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/ductone/orrery/agentproto"
	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/store"
	"github.com/ductone/orrery/workspace"
)

func TestEngineLogsThroughItsLogger(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	e := New(config.Config{}, openStore(t), nil, nil, WithScratchRoot(t.TempDir()), WithLogger(l))
	e.UseWorkspaces(func(context.Context, string, agentproto.Workspace) (workspace.Workspace, error) {
		return workspace.Workspace{}, errors.New("no such workspace")
	})
	e.sessionWorkspace(context.Background(), "s1", agentproto.Workspace{Path: "moraine://x"})
	if out := buf.String(); !strings.Contains(out, "resolve workspace") || !strings.Contains(out, "no such workspace") {
		t.Fatalf("engine logger got %q", out)
	}
}

func TestToolRegistryUsesEngineLogger(t *testing.T) {
	var buf bytes.Buffer
	st := openStore(t)
	if err := st.CreateSession(context.Background(), store.Session{ID: "s1", Spec: "x", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	e := New(config.Config{}, st, nil, nil, WithScratchRoot(t.TempDir()), WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	r := e.toolRegistry("s1", "", agentproto.TaskRequest{Workspace: agentproto.Workspace{Path: t.TempDir()}}, "", nil, nil)
	r.Add("boom", "panics", map[string]any{"type": "object"}, func(context.Context, map[string]any) (any, error) { panic("kaboom") })
	if _, err := r.Call(context.Background(), "boom", nil); err == nil {
		t.Fatal("panicking tool returned no error")
	}
	if !strings.Contains(buf.String(), "tool panicked") {
		t.Fatalf("registry did not log through the engine logger: %q", buf.String())
	}
}
