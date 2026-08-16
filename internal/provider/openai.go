package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ductone/orrey/internal/model"
)

type openAIClient struct {
	base      string
	pool      *pool
	http      *http.Client
	responses bool
}

func newOpenAI(base string, keys []string, responses bool) *openAIClient {
	p := &pool{}
	for _, k := range keys {
		p.creds = append(p.creds, credential{key: k})
	}
	return &openAIClient{strings.TrimSuffix(base, "/"), p, httpClient(15 * time.Minute), responses}
}
func (c *openAIClient) Available(now time.Time) bool { return c.pool.available(now) }
func (c *openAIClient) Complete(ctx context.Context, m model.ModelSpec, r Request) (Response, error) {
	if c.responses {
		return c.completeResponses(ctx, m, r)
	}
	key, ok := c.pool.take(time.Now())
	if !ok {
		return Response{}, ErrCredentialsBackoff
	}
	toWire, fromWire := toolNameMaps(r.Tools)
	msgs := []map[string]any{}
	system := strings.TrimSpace(strings.Join([]string{r.System, r.DurableSpec, r.Plan}, "\n\n"))
	if m.Compat.SystemPromptStyle == model.SystemTopLevel {
		msgs = append(msgs, map[string]any{"role": "system", "content": system})
	} else {
		r.Messages = append([]Message{{Role: "user", Content: system}}, r.Messages...)
	}
	for _, x := range r.Messages {
		msg := map[string]any{"role": x.Role}
		if len(x.Images) > 0 && x.Role != "tool" {
			parts := []any{}
			if x.Content != "" {
				parts = append(parts, map[string]any{"type": "text", "text": x.Content})
			}
			for _, image := range x.Images {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL(image)}})
			}
			msg["content"] = parts
		} else if x.Content != "" || m.Compat.RequiresAssistantText {
			msg["content"] = x.Content
		}
		if x.ToolCallID != "" {
			msg["tool_call_id"] = x.ToolCallID
		}
		if x.Reasoning != "" && m.Compat.RequiresReasoningEcho {
			msg["reasoning_content"] = x.Reasoning
		}
		if len(x.ToolCalls) > 0 {
			var calls []any
			for _, tc := range x.ToolCalls {
				b, _ := json.Marshal(tc.Arguments)
				name := tc.Name
				if wire := toWire[name]; wire != "" {
					name = wire
				}
				calls = append(calls, map[string]any{"id": tc.ID, "type": "function", "function": map[string]any{"name": name, "arguments": string(b)}})
			}
			msg["tool_calls"] = calls
		}
		msgs = append(msgs, msg)
	}
	body := map[string]any{"model": wireModel(m.ID), "messages": msgs, "stream": false}
	if r.CacheKey != "" && m.Compat.CacheControl {
		body["prompt_cache_key"] = r.CacheKey
	}
	body[m.Compat.MaxTokensField] = min(r.MaxOutput, m.MaxOutput)
	if m.Compat.SupportsReasoningEffort && r.Effort != model.EffortNone {
		body["reasoning_effort"] = m.Compat.EffortWireMap[r.Effort]
	}
	if len(r.Tools) > 0 {
		var ts []any
		for _, t := range r.Tools {
			schema := toolInputSchema(t.InputSchema)
			if r.Strict && m.Compat.SupportsStrictTools {
				schema = strictifySchema(schema)
			}
			fn := map[string]any{"name": toWire[t.Name], "description": t.Description, "parameters": schema}
			if r.Strict && m.Compat.SupportsStrictTools {
				fn["strict"] = true
			}
			ts = append(ts, map[string]any{"type": "function", "function": fn})
		}
		body["tools"] = ts
	}
	b, _ := json.Marshal(body)
	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, "POST", c.base+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			c.pool.backoff(key, 30*time.Second)
		}
		return Response{}, &HTTPError{resp.StatusCode, string(raw)}
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role               string `json:"role"`
				Content            string `json:"content"`
				Reasoning          string `json:"reasoning_content"`
				ReasoningAlternate string `json:"reasoning"`
				ToolCalls          []struct {
					ID       string
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
			Details    struct {
				Cached     int `json:"cached_tokens"`
				CacheWrite int `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return Response{}, err
	}
	if len(out.Choices) == 0 {
		return Response{}, fmt.Errorf("provider returned no choices")
	}
	ch := out.Choices[0]
	if ch.Message.Reasoning == "" {
		ch.Message.Reasoning = ch.Message.ReasoningAlternate
	}
	msg := Message{Role: "assistant", Content: ch.Message.Content, Reasoning: ch.Message.Reasoning}
	for _, tc := range ch.Message.ToolCalls {
		args := map[string]any{}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return Response{}, fmt.Errorf("tool arguments: %w", err)
		}
		name := tc.Function.Name
		if internal := fromWire[name]; internal != "" {
			name = internal
		}
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{tc.ID, name, args})
	}
	return Response{Message: msg, Usage: Usage{InputTokens: out.Usage.Prompt, OutputTokens: out.Usage.Completion, CacheReadTokens: out.Usage.Details.Cached, CacheWriteTokens: out.Usage.Details.CacheWrite}, StopReason: ch.Finish, Latency: time.Since(start), Model: out.Model}, nil
}

func imageURL(image Image) string {
	if image.URL != "" {
		return image.URL
	}
	mediaType := image.MediaType
	if mediaType == "" {
		mediaType = "image/png"
	}
	return "data:" + mediaType + ";base64," + image.Data
}
func strictifySchema(s map[string]any) map[string]any { return strictify(s, false).(map[string]any) }
func strictify(v any, nullable bool) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := map[string]any{}
	for k, x := range m {
		out[k] = x
	}
	switch m["type"] {
	case "object":
		props, _ := m["properties"].(map[string]any)
		original := map[string]bool{}
		switch xs := m["required"].(type) {
		case []string:
			for _, x := range xs {
				original[x] = true
			}
		case []any:
			for _, x := range xs {
				original[fmt.Sprint(x)] = true
			}
		}
		next := map[string]any{}
		keys := make([]string, 0, len(props))
		for k, x := range props {
			keys = append(keys, k)
			next[k] = strictify(x, !original[k])
		}
		sort.Strings(keys)
		out["properties"] = next
		out["required"] = keys
		out["additionalProperties"] = false
	case "array":
		out["items"] = strictify(m["items"], false)
	}
	if nullable {
		return map[string]any{"anyOf": []any{out, map[string]any{"type": "null"}}}
	}
	return out
}
