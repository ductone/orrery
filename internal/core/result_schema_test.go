package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
)

func TestInvalidResultSchemaResponseIsRetried(t *testing.T) {
	e, _ := testEngine(t)
	workspace := t.TempDir()

	var taskCalls atomic.Int32
	var secondRequest string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		requestBody := string(body)
		if !strings.Contains(requestBody, "REQUIRED RESULT SCHEMA") {
			writeResponsesResult(t, w, "Schema retry test")
			return
		}
		attempt := taskCalls.Add(1)
		if attempt == 2 {
			secondRequest = requestBody
		}
		result := `{"ok":"wrong type"}`
		if attempt > 1 {
			result = `{"ok":true}`
		}
		writeResponsesResult(t, w, result)
	}))
	defer server.Close()

	cfg := config.Config{
		WorkspaceRoot: workspace,
		Providers:     map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: server.URL}},
		Router:        config.RouterConfig{DisableSwitch: true, DefaultModel: "openai/gpt-5.6-terra"},
	}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)

	schema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"ok": map[string]any{"type": "boolean"}},
		"required":             []any{"ok"},
		"additionalProperties": false,
	}
	req := agentproto.TaskRequest{
		Spec:         "Return whether the task succeeded",
		ResultSchema: schema,
		Budget:       agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute, MaxDepth: 0},
		Workspace:    agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"},
	}
	_, results, err := e.Start(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := <-results
	if result.Status != agentproto.Pass || result.Result["ok"] != true {
		t.Fatalf("result=%+v", result)
	}
	if got := taskCalls.Load(); got != 2 {
		t.Fatalf("provider calls=%d, want 2", got)
	}
	if result.Outcome.CompletionRejects != 1 {
		t.Fatalf("completion rejections=%d, want 1", result.Outcome.CompletionRejects)
	}
	for _, expected := range []string{"Completion rejected", "required result schema", `\"ok\"`} {
		if !strings.Contains(secondRequest, expected) {
			t.Fatalf("second request missing %q: %s", expected, secondRequest)
		}
	}
}

func writeResponsesResult(t *testing.T, w http.ResponseWriter, text string) {
	t.Helper()
	payload := map[string]any{
		"model":  "gpt-5.6-terra",
		"status": "completed",
		"output": []any{map[string]any{
			"type":    "message",
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		}},
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
