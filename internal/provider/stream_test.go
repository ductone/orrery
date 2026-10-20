package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrery/internal/model"
)

var streams = map[string]struct {
	client func(base string) Client
	events []string
}{
	"anthropic": {func(b string) Client { return newAnthropic(b, []string{"k"}) }, []string{
		`{"type":"message_start","message":{"model":"m","usage":{"input_tokens":3}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"o"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"k"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"read","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"a\"}"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	}},
	"responses": {func(b string) Client { return newOpenAI(b, []string{"k"}, true) }, []string{
		`{"type":"response.created"}`,
		`{"type":"response.output_text.delta","delta":"o"}`,
		`{"type":"response.output_text.delta","delta":"k"}`,
		`{"type":"response.completed","response":{"model":"m","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]},{"type":"function_call","call_id":"t1","name":"read","arguments":"{\"path\":\"a\"}"}],"usage":{"input_tokens":3,"output_tokens":2}}}`,
	}},
	"chat": {func(b string) Client { return newOpenAI(b, []string{"k"}, false) }, []string{
		`{"model":"m","choices":[{"delta":{"role":"assistant","content":"o"}}]}`,
		`{"choices":[{"delta":{"content":"k"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t1","function":{"name":"read","arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
		`[DONE]`,
	}},
}

// sseServer sends events with gap between them; when stallAfter >= 0 it
// stops sending after that many events and holds the connection open.
func sseServer(t *testing.T, events []string, gap time.Duration, stallAfter int) *httptest.Server {
	t.Helper()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		for i, ev := range events {
			if i == stallAfter {
				select {
				case <-r.Context().Done():
				case <-done:
				}
				return
			}
			_, _ = w.Write([]byte("data: " + ev + "\n\n"))
			w.(http.Flusher).Flush()
			time.Sleep(gap)
		}
	}))
	t.Cleanup(func() {
		close(done)
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv
}

func TestSlowSteadyStreamIsNotCutOffByIdleTimeout(t *testing.T) {
	for name, s := range streams {
		t.Run(name, func(t *testing.T) {
			// Each gap is under the idle timeout, but the whole stream takes
			// several times longer than it.
			srv := sseServer(t, s.events, 60*time.Millisecond, -1)
			spec := model.ModelSpec{ID: "test/steady-" + name, MaxOutput: 100, Compat: model.Compat{FirstByteTimeout: time.Second, StreamIdleTimeout: 150 * time.Millisecond}}
			started := time.Now()
			resp, err := s.client(srv.URL).Complete(context.Background(), spec, Request{MaxOutput: 100, Messages: []Message{{Role: "user", Content: "hi"}}})
			if err != nil {
				t.Fatalf("steady stream was cut off: %v", err)
			}
			if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
				t.Fatalf("stream finished in %v, test does not outlast the idle timeout", elapsed)
			}
			if resp.Message.Content != "ok" || resp.Model != "m" || resp.Usage.OutputTokens != 2 {
				t.Fatalf("response = %+v", resp)
			}
			if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].ID != "t1" || resp.Message.ToolCalls[0].Arguments["path"] != "a" {
				t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
			}
		})
	}
}

func TestMidStreamStallFailsOverAfterIdleTimeout(t *testing.T) {
	for name, s := range streams {
		t.Run(name, func(t *testing.T) {
			srv := sseServer(t, s.events, 0, 2)
			spec := model.ModelSpec{ID: "test/stall-" + name, MaxOutput: 100, Compat: model.Compat{FirstByteTimeout: time.Second, StreamIdleTimeout: 100 * time.Millisecond}}
			c := s.client(srv.URL)
			started := time.Now()
			_, err := c.Complete(context.Background(), spec, Request{MaxOutput: 100, Messages: []Message{{Role: "user", Content: "hi"}}})
			if err == nil {
				t.Fatal("a stream that stops sending must fail")
			}
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				t.Fatalf("stall took %v, want the idle timeout", elapsed)
			}
			if !strings.Contains(err.Error(), "model sent no data") || !IsRetryable(err) {
				t.Fatalf("error = %v, want a retryable idle stall", err)
			}
			if c.(availability).ReadyAt(spec.ID).IsZero() {
				t.Fatal("a route that stalled mid-stream must cool down")
			}
		})
	}
}

// realAnthropicStream is shaped like a Messages API stream: usage carries
// service_tier and the cache_creation and server_tool_use objects, a thinking
// block ends with a signature, and three tool calls arrive with no input
// delta, one empty partial_json, and arguments split across chunks.
var realAnthropicStream = []string{
	`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":100,"cache_creation_input_tokens":20,"cache_read_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":20},"output_tokens":1,"service_tier":"standard","server_tool_use":{"web_search_requests":0}}}}`,
	`{"type":"ping"}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Look "}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"first."}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"c2lnbmF0dXJl"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Checking."}}`,
	`{"type":"content_block_stop","index":1}`,
	`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t1","name":"status","input":{}}}`,
	`{"type":"content_block_stop","index":2}`,
	`{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"t2","name":"status","input":{}}}`,
	`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":""}}`,
	`{"type":"content_block_stop","index":3}`,
	`{"type":"content_block_start","index":4,"content_block":{"type":"tool_use","id":"t3","name":"read","input":{}}}`,
	`{"type":"content_block_delta","index":4,"delta":{"type":"input_json_delta","partial_json":""}}`,
	`{"type":"content_block_delta","index":4,"delta":{"type":"input_json_delta","partial_json":"{\"path\": \"a"}}`,
	`{"type":"content_block_delta","index":4,"delta":{"type":"input_json_delta","partial_json":".go\", \"lim"}}`,
	`{"type":"content_block_delta","index":4,"delta":{"type":"input_json_delta","partial_json":"it\": 5}"}}`,
	`{"type":"content_block_stop","index":4}`,
	`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":100,"cache_creation_input_tokens":20,"cache_read_input_tokens":50,"output_tokens":42,"server_tool_use":{"web_search_requests":0}}}`,
	`{"type":"message_stop"}`,
}

func TestAnthropicRealStreamParsesAndReplaysObjectInputs(t *testing.T) {
	var replayed map[string]any
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 1 {
			_ = json.NewDecoder(r.Body).Decode(&replayed)
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		for _, ev := range realAnthropicStream {
			var typ struct{ Type string }
			_ = json.Unmarshal([]byte(ev), &typ)
			_, _ = w.Write([]byte("event: " + typ.Type + "\ndata: " + ev + "\n\n"))
		}
	}))
	defer srv.Close()
	c := newAnthropic(srv.URL, []string{"k"})
	spec := model.ModelSpec{ID: "anthropic/claude-sonnet-5-5", MaxOutput: 100}
	tools := []Tool{{Name: "status"}, {Name: "read"}}
	history := []Message{{Role: "user", Content: "go"}}
	resp, err := c.Complete(context.Background(), spec, Request{MaxOutput: 100, Tools: tools, Messages: history})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage != (Usage{InputTokens: 170, OutputTokens: 42, CacheReadTokens: 50, CacheWriteTokens: 20}) {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if resp.StopReason != "tool_use" || resp.Model != "claude-sonnet-5-5" || resp.Message.Content != "Checking." || resp.Message.Reasoning != "Look first." {
		t.Fatalf("response = %+v", resp)
	}
	tc := resp.Message.ToolCalls
	if len(tc) != 3 || tc[0].ID != "t1" || tc[1].ID != "t2" || tc[2].ID != "t3" {
		t.Fatalf("tool calls = %+v", tc)
	}
	if tc[0].Arguments == nil || len(tc[0].Arguments) != 0 || tc[1].Arguments == nil || len(tc[1].Arguments) != 0 {
		t.Fatalf("no-argument calls must keep an empty input: %+v", tc)
	}
	if tc[2].Arguments["path"] != "a.go" || tc[2].Arguments["limit"] != float64(5) {
		t.Fatalf("split arguments = %+v", tc[2].Arguments)
	}

	// Replay the assistant turn, plus a stored call whose arguments are nil.
	assistant := resp.Message
	assistant.ToolCalls = append(assistant.ToolCalls, ToolCall{ID: "t4", Name: "status"})
	history = append(history, assistant)
	for _, call := range assistant.ToolCalls {
		history = append(history, Message{Role: "tool", ToolCallID: call.ID, Content: "ok"})
	}
	if _, err := c.Complete(context.Background(), spec, Request{MaxOutput: 100, Tools: tools, Messages: history}); err != nil {
		t.Fatal(err)
	}
	inputs := map[string]any{}
	for _, m := range replayed["messages"].([]any) {
		for _, b := range m.(map[string]any)["content"].([]any) {
			if b := b.(map[string]any); b["type"] == "tool_use" {
				inputs[b["id"].(string)] = b["input"]
			}
		}
	}
	if len(inputs) != 4 {
		t.Fatalf("replayed tool_use inputs = %+v", inputs)
	}
	for id, in := range inputs {
		if _, ok := in.(map[string]any); !ok {
			t.Fatalf("tool_use %s input = %#v, want an object", id, in)
		}
	}
	if inputs["t3"].(map[string]any)["path"] != "a.go" {
		t.Fatalf("replayed arguments = %+v", inputs["t3"])
	}
}
