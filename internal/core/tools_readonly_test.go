package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
	runtimemcp "github.com/ductone/orrey/internal/mcp"
)

func TestReadOnlyToolRegistryOmitsSpawn(t *testing.T) {
	e, _ := testEngine(t)
	registry := e.toolRegistry("session-read", "", agentproto.TaskRequest{
		Workspace: agentproto.Workspace{Path: t.TempDir(), Mode: "read"},
	}, "", &instructionDiscovery{}, nil)
	for _, definition := range registry.Definitions() {
		if definition.Name == "spawn" {
			t.Fatal("read-only tool registry exposed spawn")
		}
	}
}

func TestSharedWriteToolRegistryIncludesSpawn(t *testing.T) {
	e, _ := testEngine(t)
	registry := e.toolRegistry("session-write", "", agentproto.TaskRequest{
		Workspace: agentproto.Workspace{Path: t.TempDir(), Mode: "shared-write"},
	}, "", &instructionDiscovery{}, nil)
	for _, definition := range registry.Definitions() {
		if definition.Name == "spawn" {
			return
		}
	}
	t.Fatal("shared-write tool registry omitted spawn")
}

func TestReadOnlyToolRegistryFiltersExternalMCPMutations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := any(map[string]any{})
		if in.Method == "tools/list" {
			result = map[string]any{"tools": []any{
				map[string]any{"name": "inspect", "annotations": map[string]any{"readOnlyHint": true}, "inputSchema": map[string]any{"type": "object"}},
				map[string]any{"name": "mutate", "inputSchema": map[string]any{"type": "object"}},
			}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": in.ID, "result": result})
	}))
	defer srv.Close()

	m, err := runtimemcp.New(context.Background(), map[string]config.MCPConfig{
		"external": {Transport: "http", URL: srv.URL},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	e, _ := testEngine(t)
	e.mcp = m
	for _, tc := range []struct {
		mode       string
		wantMutate bool
	}{
		{mode: "read", wantMutate: false},
		{mode: "shared-write", wantMutate: true},
	} {
		registry := e.toolRegistry("session-"+tc.mode, "", agentproto.TaskRequest{
			Workspace: agentproto.Workspace{Path: t.TempDir(), Mode: tc.mode},
		}, "", &instructionDiscovery{}, nil)
		seenInspect, seenMutate := false, false
		for _, definition := range registry.Definitions() {
			seenInspect = seenInspect || definition.Name == "external.inspect"
			seenMutate = seenMutate || definition.Name == "external.mutate"
		}
		if !seenInspect || seenMutate != tc.wantMutate {
			t.Fatalf("mode %s: inspect=%v mutate=%v, want mutate=%v", tc.mode, seenInspect, seenMutate, tc.wantMutate)
		}
	}
}

func TestReadOnlyToolRegistryPreservesScopedSquireTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := any(map[string]any{})
		if in.Method == "tools/list" {
			result = map[string]any{"tools": []any{
				map[string]any{"name": "squire.fs.write", "inputSchema": map[string]any{"type": "object"}},
				map[string]any{"name": "squire.task.link", "inputSchema": map[string]any{"type": "object"}},
				map[string]any{"name": "squire.task.complete", "inputSchema": map[string]any{"type": "object"}},
				map[string]any{"name": "squire.task.delete", "inputSchema": map[string]any{"type": "object"}},
				map[string]any{"name": "squire.user_instructions.set", "inputSchema": map[string]any{"type": "object"}},
			}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": in.ID, "result": result})
	}))
	defer srv.Close()

	m, err := runtimemcp.New(context.Background(), map[string]config.MCPConfig{
		"bridge": {Transport: "http", URL: srv.URL, Squire: true},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	e, _ := testEngine(t)
	e.mcp = m
	registry := e.toolRegistry("session-squire-read", "", agentproto.TaskRequest{
		Workspace: agentproto.Workspace{Path: t.TempDir(), Mode: "read"},
	}, "", &instructionDiscovery{}, nil)
	seen := map[string]bool{}
	for _, definition := range registry.Definitions() {
		seen[definition.Name] = true
	}
	for _, name := range []string{"bridge.squire.fs.write", "bridge.squire.task.link", "bridge.squire.task.complete"} {
		if !seen[name] {
			t.Errorf("read registry omitted scoped Squire tool %s", name)
		}
	}
	for _, name := range []string{"bridge.squire.task.delete", "bridge.squire.user_instructions.set"} {
		if seen[name] {
			t.Errorf("read registry exposed unsafe Squire tool %s", name)
		}
	}
}
