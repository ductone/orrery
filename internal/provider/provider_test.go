package provider

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/router"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type blockingClient struct{}

func (blockingClient) Complete(ctx context.Context, _ model.ModelSpec, _ Request) (Response, error) {
	<-ctx.Done()
	return Response{}, ctx.Err()
}

func TestOpenAICompatAssembly(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key" {
			t.Error("auth")
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-5.6-sol","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":20}}}`))
	}))
	defer srv.Close()
	m, _ := model.Get("openai/gpt-5.6-sol")
	c := newOpenAI(srv.URL, []string{"key"}, false)
	resp, err := c.Complete(context.Background(), m, Request{System: "sys", DurableSpec: "task", Plan: "todo", CacheKey: "s:m", MaxOutput: 99, Effort: model.EffortHigh, Strict: true, Messages: []Message{{Role: "user", Content: "go"}}, Tools: []Tool{{Name: "x", Description: "x", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}, "additionalProperties": false}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got["max_completion_tokens"] != float64(99) || got["reasoning_effort"] != "high" || got["prompt_cache_key"] != "s:m" {
		t.Fatalf("body %+v", got)
	}
	if resp.Usage.CacheReadTokens != 40 || resp.Usage.CacheWriteTokens != 20 {
		t.Fatalf("usage %+v", resp.Usage)
	}
}

func TestOpenAIChatCarriesImages(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"model":"grok-4.6","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer srv.Close()
	m, _ := model.Get("xai/grok-4.6")
	c := newOpenAI(srv.URL, []string{"key"}, false)
	_, err := c.Complete(context.Background(), m, Request{MaxOutput: 10, Messages: []Message{{Role: "user", Content: "inspect", Images: []Image{{MediaType: "image/png", Data: "aGVsbG8="}}}}})
	if err != nil {
		t.Fatal(err)
	}
	messages := got["messages"].([]any)
	content := messages[len(messages)-1].(map[string]any)["content"].([]any)
	url := content[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if url != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("url=%q", url)
	}
}

func TestOpenAIChatMapsNamespacedToolNames(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		name := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "grok-4.6",
			"choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function", "function": map[string]any{"name": name, "arguments": "{}"},
				}}},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]any{},
		})
	}))
	defer srv.Close()
	m, _ := model.Get("openai/gpt-5.6-sol")
	c := newOpenAI(srv.URL, []string{"key"}, false)
	resp, err := c.Complete(context.Background(), m, Request{MaxOutput: 10, Strict: true, Tools: []Tool{{Name: "gateway.identity", InputSchema: map[string]any{
		"type": "object", "properties": map[string]any{"features": map[string]any{"type": "array"}},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	wire := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"].(string)
	parameters := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"]
	if wire == "gateway.identity" || resp.Message.ToolCalls[0].Name != "gateway.identity" {
		t.Fatalf("wire=%q response=%+v", wire, resp.Message.ToolCalls)
	}
	if parameters == nil {
		t.Fatal("nil MCP schema must be sent as an object schema")
	}
	function := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if function["strict"] != nil {
		t.Fatal("tool schemas that omit array item types must degrade from strict mode without changing their meaning")
	}
}

func TestAnthropicCacheAndUsage(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"model":"claude-fable-5","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":20,"cache_creation_input_tokens":30}}`))
	}))
	defer srv.Close()
	m, _ := model.Get("anthropic/claude-fable-5")
	c := newAnthropic(srv.URL, []string{"key"})
	resp, err := c.Complete(context.Background(), m, Request{System: "sys", MaxOutput: 10, Effort: model.EffortHigh, Messages: []Message{{Role: "user", Content: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.InputTokens != 60 {
		t.Fatalf("usage %+v", resp.Usage)
	}
	if got["output_config"].(map[string]any)["effort"] != "high" {
		t.Fatalf("body %+v", got)
	}
}

func TestAnthropicCarriesImages(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"model":"claude-fable-5","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{}}`))
	}))
	defer srv.Close()
	m, _ := model.Get("anthropic/claude-fable-5")
	c := newAnthropic(srv.URL, []string{"key"})
	_, err := c.Complete(context.Background(), m, Request{MaxOutput: 10, Messages: []Message{{Role: "user", Images: []Image{{MediaType: "image/jpeg", Data: "aGVsbG8="}}}}})
	if err != nil {
		t.Fatal(err)
	}
	message := got["messages"].([]any)[0].(map[string]any)
	image := message["content"].([]any)[0].(map[string]any)
	if image["type"] != "image" {
		t.Fatalf("content=%+v", message["content"])
	}
}

func TestAnthropicMapsNamespacedToolNames(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		tools := got["tools"].([]any)
		name := tools[0].(map[string]any)["name"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "claude-fable-5", "stop_reason": "tool_use",
			"content": []any{map[string]any{"type": "tool_use", "id": "call_1", "name": name, "input": map[string]any{}}},
			"usage":   map[string]any{"input_tokens": 10, "output_tokens": 2},
		})
	}))
	defer srv.Close()
	m, _ := model.Get("anthropic/claude-fable-5")
	c := newAnthropic(srv.URL, []string{"key"})
	resp, err := c.Complete(context.Background(), m, Request{MaxOutput: 10, Tools: []Tool{{Name: "gateway.identity"}}})
	if err != nil {
		t.Fatal(err)
	}
	wire := got["tools"].([]any)[0].(map[string]any)["name"].(string)
	if wire == "gateway.identity" || resp.Message.ToolCalls[0].Name != "gateway.identity" {
		t.Fatalf("wire=%q response=%+v", wire, resp.Message.ToolCalls)
	}
}
func TestOpenAIResponsesToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"model":"gpt-5.6-sol","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"exec","arguments":"{\"command\":\"true\"}"}],"usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":4}}}`))
	}))
	defer srv.Close()
	m, _ := model.Get("openai/gpt-5.6-sol")
	c := newOpenAI(srv.URL, []string{"key"}, true)
	resp, err := c.Complete(context.Background(), m, Request{MaxOutput: 10, Messages: []Message{{Role: "user", Content: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].ID != "call_1" || resp.Usage.InputTokens != 10 {
		t.Fatalf("response %+v", resp)
	}
}

func TestOpenAIResponsesMapsNamespacedToolNames(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		name := got["tools"].([]any)[0].(map[string]any)["name"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "gpt-5.6-sol", "status": "completed",
			"output": []any{map[string]any{"type": "function_call", "call_id": "call_1", "name": name, "arguments": "{}"}},
			"usage":  map[string]any{"input_tokens": 10, "output_tokens": 5},
		})
	}))
	defer srv.Close()
	m, _ := model.Get("openai/gpt-5.6-sol")
	c := newOpenAI(srv.URL, []string{"key"}, true)
	resp, err := c.Complete(context.Background(), m, Request{MaxOutput: 10, Tools: []Tool{{Name: "gateway.identity", InputSchema: map[string]any{"type": "object"}}}})
	if err != nil {
		t.Fatal(err)
	}
	wire := got["tools"].([]any)[0].(map[string]any)["name"].(string)
	if wire == "gateway.identity" || resp.Message.ToolCalls[0].Name != "gateway.identity" {
		t.Fatalf("wire=%q response=%+v", wire, resp.Message.ToolCalls)
	}
}

func TestCredentialBackoffIsRetryable(t *testing.T) {
	if !IsRetryable(ErrCredentialsBackoff) {
		t.Fatal("credential backoff should reroute instead of terminating the session")
	}
	if !IsRetryable(errors.Join(errors.New("model failed"), ErrCredentialsBackoff)) {
		t.Fatal("wrapped credential backoff should remain retryable")
	}
}

func TestCompleteOneHonorsModelRequestTimeout(t *testing.T) {
	spec := model.ModelSpec{ID: "xai/timeout-test", Compat: model.Compat{StreamIdleTimeout: 10 * time.Millisecond}}
	r := &Registry{clients: map[string]Client{"xai": blockingClient{}}}
	started := time.Now()
	_, err := r.CompleteOne(context.Background(), router.Decision{Model: spec}, func(model.ModelSpec, router.Decision) (Request, error) {
		return Request{}, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("request timeout took %v", elapsed)
	}
}

func TestCredentialPoolAvailabilityHonorsCooldown(t *testing.T) {
	now := time.Now()
	p := &pool{creds: []credential{{key: "a", backoffUntil: now.Add(time.Minute)}, {key: "b", backoffUntil: now.Add(-time.Second)}}}
	if !p.available(now) {
		t.Fatal("one usable credential should keep provider available")
	}
	p.creds[1].backoffUntil = now.Add(time.Minute)
	if p.available(now) {
		t.Fatal("provider should be unavailable while all credentials cool down")
	}
}
