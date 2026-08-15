package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ductone/orrey/internal/model"
)

type openAIClient struct {
	base string
	pool *pool
	http *http.Client
}

func newOpenAI(base string, keys []string) *openAIClient {
	p := &pool{}
	for _, k := range keys {
		p.creds = append(p.creds, credential{key: k})
	}
	return &openAIClient{strings.TrimSuffix(base, "/"), p, httpClient(15 * time.Minute)}
}
func (c *openAIClient) Complete(ctx context.Context, m model.ModelSpec, r Request) (Response, error) {
	key, ok := c.pool.take(time.Now())
	if !ok {
		return Response{}, fmt.Errorf("all credentials in backoff")
	}
	msgs := []map[string]any{}
	system := strings.TrimSpace(strings.Join([]string{r.System, r.DurableSpec, r.Plan}, "\n\n"))
	if m.Compat.SystemPromptStyle == model.SystemTopLevel {
		msgs = append(msgs, map[string]any{"role": "system", "content": system})
	} else {
		r.Messages = append([]Message{{Role: "user", Content: system}}, r.Messages...)
	}
	for _, x := range r.Messages {
		msg := map[string]any{"role": x.Role}
		if x.Content != "" || m.Compat.RequiresAssistantText {
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
				calls = append(calls, map[string]any{"id": tc.ID, "type": "function", "function": map[string]any{"name": tc.Name, "arguments": string(b)}})
			}
			msg["tool_calls"] = calls
		}
		msgs = append(msgs, msg)
	}
	body := map[string]any{"model": wireModel(m.ID), "messages": msgs, "stream": false}
	body[m.Compat.MaxTokensField] = min(r.MaxOutput, m.MaxOutput)
	if m.Compat.SupportsReasoningEffort && r.Effort != model.EffortNone {
		body["reasoning_effort"] = m.Compat.EffortWireMap[r.Effort]
	}
	if len(r.Tools) > 0 {
		var ts []any
		for _, t := range r.Tools {
			fn := map[string]any{"name": t.Name, "description": t.Description, "parameters": t.InputSchema}
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
				Role, Content, Reasoning string
				ToolCalls                []struct {
					ID       string
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt, Completion int
			Details            struct {
				Cached int `json:"cached_tokens"`
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
	msg := Message{Role: "assistant", Content: ch.Message.Content, Reasoning: ch.Message.Reasoning}
	for _, tc := range ch.Message.ToolCalls {
		args := map[string]any{}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return Response{}, fmt.Errorf("tool arguments: %w", err)
		}
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{tc.ID, tc.Function.Name, args})
	}
	return Response{Message: msg, Usage: Usage{InputTokens: out.Usage.Prompt, OutputTokens: out.Usage.Completion, CacheReadTokens: out.Usage.Details.Cached}, StopReason: ch.Finish, Latency: time.Since(start), Model: out.Model}, nil
}
