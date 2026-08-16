package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/core"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
)

func TestACPInitializeCreateAndList(t *testing.T) {
	cfg := config.Default()
	cfg.Database = t.TempDir() + "/db.sqlite"
	cfg.WorkspaceRoot = t.TempDir()
	st, err := store.Open(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	server := Server{Engine: core.New(cfg, st, provider.New(cfg), nil), Mode: ACP, Version: "test"}
	in := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":1}}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"session/new\",\"params\":{\"cwd\":\"" + cfg.WorkspaceRoot + "\"}}\n")
	var out bytes.Buffer
	if err := server.Serve(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("output=%s", out.String())
	}
	var initialized, created bool
	for _, line := range lines {
		var response map[string]any
		if json.Unmarshal([]byte(line), &response) != nil {
			t.Fatal(line)
		}
		id := int(response["id"].(float64))
		if id == 1 {
			initialized = strings.Contains(line, "agentCapabilities")
		}
		if id == 2 {
			created = strings.Contains(line, "sessionId")
		}
	}
	if !initialized || !created {
		t.Fatalf("initialized=%v created=%v output=%s", initialized, created, out.String())
	}
}

func TestPromptText(t *testing.T) {
	got := promptText(json.RawMessage(`[{"type":"text","text":"one"},{"type":"image","text":"skip"},{"type":"text","text":"two"}]`))
	if got != "one\ntwo" {
		t.Fatal(got)
	}
}
