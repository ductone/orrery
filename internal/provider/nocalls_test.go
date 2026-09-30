package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/ductone/orrey/internal/model"
)

var probeTool = Tool{Name: "read", Description: "read", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}, "additionalProperties": false}}

// capture serves one canned response and records the request body.
func capture(t *testing.T, response string) (*httptest.Server, *map[string]any) {
	t.Helper()
	body := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return srv, &body
}

func TestResponsesNoToolCallsKeepsDefinitionsAndReportsTruncation(t *testing.T) {
	srv, body := capture(t, `{"model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"reasoning","summary":[]}],"usage":{"input_tokens":10,"output_tokens":8000}}`)
	m, _ := model.Get("openai/gpt-5.6-sol")
	resp, err := newOpenAI(srv.URL, []string{"k"}, true).Complete(context.Background(), m, Request{MaxOutput: 8000, NoToolCalls: true, Tools: []Tool{probeTool}, Messages: []Message{{Role: "user", Content: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if (*body)["tool_choice"] != "none" || len((*body)["tools"].([]any)) != 1 {
		t.Fatalf("tools must stay defined with tool_choice none: %v", *body)
	}
	if !resp.Truncated || resp.StopReason != "incomplete:max_output_tokens" || !slices.Equal(resp.OutputKinds, []string{"reasoning"}) {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestResponsesDefaultsLeaveToolChoiceUnset(t *testing.T) {
	srv, body := capture(t, `{"model":"m","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{}}`)
	m, _ := model.Get("openai/gpt-5.6-sol")
	resp, err := newOpenAI(srv.URL, []string{"k"}, true).Complete(context.Background(), m, Request{MaxOutput: 10, Tools: []Tool{probeTool}, Messages: []Message{{Role: "user", Content: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, set := (*body)["tool_choice"]; set || resp.Truncated || resp.StopReason != "completed" {
		t.Fatalf("body=%v resp=%+v", *body, resp)
	}
}

func TestChatNoToolCallsAndLengthTruncation(t *testing.T) {
	srv, body := capture(t, `{"model":"m","choices":[{"message":{"role":"assistant","content":""},"finish_reason":"length"}],"usage":{}}`)
	m, _ := model.Get("xai/grok-4.6")
	resp, err := newOpenAI(srv.URL, []string{"k"}, false).Complete(context.Background(), m, Request{MaxOutput: 10, NoToolCalls: true, Tools: []Tool{probeTool}, Messages: []Message{{Role: "user", Content: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if (*body)["tool_choice"] != "none" || !resp.Truncated || len(resp.OutputKinds) != 0 {
		t.Fatalf("body=%v resp=%+v", *body, resp)
	}
}

func TestAnthropicNoToolCallsMergesTrailingUserAndReportsTruncation(t *testing.T) {
	srv, body := capture(t, `{"model":"m","stop_reason":"max_tokens","content":[{"type":"thinking","thinking":""},{"type":"redacted_thinking"}],"usage":{"input_tokens":1,"output_tokens":8000}}`)
	m, _ := model.Get("anthropic/claude-fable-5")
	history := []Message{
		{Role: "user", Content: "review"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "t1", Name: "read", Arguments: map[string]any{}}}},
		{Role: "tool", ToolCallID: "t1", Content: "file"},
		{Role: "user", Content: "No more tool calls. Return the result now."},
	}
	resp, err := newAnthropic(srv.URL, []string{"k"}).Complete(context.Background(), m, Request{MaxOutput: 8000, NoToolCalls: true, Tools: []Tool{probeTool}, Messages: history})
	if err != nil {
		t.Fatal(err)
	}
	if choice, _ := (*body)["tool_choice"].(map[string]any); choice["type"] != "none" {
		t.Fatalf("tool_choice = %v", (*body)["tool_choice"])
	}
	msgs := (*body)["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("the directive must merge into the tool-result user turn, got %d messages", len(msgs))
	}
	last := msgs[2].(map[string]any)["content"].([]any)
	if last[0].(map[string]any)["type"] != "tool_result" || last[1].(map[string]any)["type"] != "text" {
		t.Fatalf("tool results must lead the merged user turn: %v", last)
	}
	if !resp.Truncated || !slices.Equal(resp.OutputKinds, []string{"thinking", "redacted_thinking"}) {
		t.Fatalf("resp = %+v", resp)
	}
}
