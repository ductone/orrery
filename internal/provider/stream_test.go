package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/model"
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
