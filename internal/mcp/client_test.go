package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/ductone/orrey/internal/config"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

func TestHTTPLifecycleAndBoundaryRefresh(t *testing.T) {
	var lists atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if r.Header.Get("Mcp-Session-Id") != "test-session" {
				t.Error("missing session on delete")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var in rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "test-session")
		} else if r.Header.Get("Mcp-Session-Id") != "test-session" {
			t.Errorf("missing session for %s", in.Method)
		}
		if r.Header.Get("MCP-Protocol-Version") != "2025-03-26" {
			t.Error("missing protocol version")
		}
		if in.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{}
		switch in.Method {
		case "tools/list":
			lists.Add(1)
			result = map[string]any{"tools": []any{map[string]any{"name": "ping", "description": "p", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "pong"}}}
		}
		if in.Method == "tools/list" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n"))
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": in.ID, "result": result})
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			return
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

func TestCompactToolDescriptionKeepsToolSpecificTail(t *testing.T) {
	description := strings.Repeat("shared gateway policy ", 200) + "tool-specific contract"
	got := compactToolDescription(description)
	if utf8.RuneCountInString(got) > 1700 {
		t.Fatalf("description was not compacted: %d runes", utf8.RuneCountInString(got))
	}
	if !strings.Contains(got, "shared MCP description elided") || !strings.HasSuffix(got, "tool-specific contract") {
		t.Fatalf("unexpected compact description: %q", got)
	}
	if got = compactToolDescription(" concise tool "); got != "concise tool" {
		t.Fatalf("short description changed: %q", got)
	}
}

func TestSquireSessionIdentityIsHiddenAndScoped(t *testing.T) {
	var seen []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := any(map[string]any{})
		if in.Method == "tools/list" {
			result = map[string]any{"tools": []any{map[string]any{"name": "ping", "description": "p", "inputSchema": map[string]any{"type": "object"}}}}
		}
		if in.Method == "tools/call" {
			params, _ := in.Params.(map[string]any)
			args, _ := params["arguments"].(map[string]any)
			seen = append(seen, args)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": in.ID, "result": result})
	}))
	defer srv.Close()
	m, err := New(context.Background(), map[string]config.MCPConfig{
		"squire": {Transport: "http", URL: srv.URL, Squire: true},
		"plain":  {Transport: "http", URL: srv.URL},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	original := map[string]any{"value": "hello"}
	if _, err = m.CallForSession(context.Background(), "session-1", "squire.ping", original); err != nil {
		t.Fatal(err)
	}
	if _, err = m.CallForSession(context.Background(), "session-1", "plain.ping", original); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0]["_squire_session_id"] != "orrery:session-1" {
		t.Fatalf("squire args=%+v", seen)
	}
	if _, ok := seen[1]["_squire_session_id"]; ok {
		t.Fatalf("identity leaked to ordinary MCP: %+v", seen[1])
	}
	if _, ok := original["_squire_session_id"]; ok {
		t.Fatalf("caller arguments were mutated: %+v", original)
	}
}

func TestStdioLifecycleAndCall(t *testing.T) {
	m, err := New(context.Background(), map[string]config.MCPConfig{
		"test": {Transport: "stdio", Command: []string{os.Args[0], "-test.run=TestStdioHelperProcess"}},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if len(m.Definitions()) != 1 || m.Definitions()[0].Name != "test.ping" {
		t.Fatalf("defs %+v", m.Definitions())
	}
	got, err := m.Call(context.Background(), "test.ping", map[string]any{"value": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if string(b) != `{"result":{"content":[{"text":"pong","type":"text"}]},"untrusted":true}` {
		t.Fatalf("result %s", b)
	}
}

func TestStdioHelperProcess(t *testing.T) {
	helper := false
	for _, arg := range os.Args[1:] {
		if arg == "-test.run=TestStdioHelperProcess" {
			helper = true
			break
		}
	}
	if !helper {
		return
	}
	s := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for s.Scan() {
		var in rpcRequest
		if json.Unmarshal(s.Bytes(), &in) != nil {
			os.Exit(2)
		}
		if in.Method == "notifications/initialized" {
			continue
		}
		result := any(map[string]any{})
		switch in.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}}
		case "tools/list":
			// A real server may interleave notifications with responses.
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
			result = map[string]any{"tools": []any{map[string]any{"name": "ping", "description": "p", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "pong"}}}
		}
		if enc.Encode(map[string]any{"jsonrpc": "2.0", "id": in.ID, "result": result}) != nil {
			os.Exit(3)
		}
	}
	os.Exit(0)
}
