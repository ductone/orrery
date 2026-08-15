package mcp

import (
	"context"
	"encoding/json"
	"github.com/ductone/orrey/internal/config"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHTTPLifecycleAndBoundaryRefresh(t *testing.T) {
	var lists atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		var result any = map[string]any{}
		switch in.Method {
		case "tools/list":
			lists.Add(1)
			result = map[string]any{"tools": []any{map[string]any{"name": "ping", "description": "p", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "pong"}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": in.ID, "result": result})
	}))
	defer srv.Close()
	m, err := New(context.Background(), map[string]config.MCPConfig{"test": {Transport: "http", URL: srv.URL}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if len(m.Definitions()) != 1 || m.Definitions()[0].Name != "test.ping" {
		t.Fatalf("defs %+v", m.Definitions())
	}
	if _, err = m.Call(context.Background(), "test.ping", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err = m.PhaseBoundary(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lists.Load() != 2 {
		t.Fatalf("list count %d", lists.Load())
	}
}
