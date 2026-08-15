package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/ductone/orrey/internal/model"
	"io"
	"net/http"
	"strings"
	"time"
)

type anthropicClient struct {
	base string
	pool *pool
	http *http.Client
}

func newAnthropic(base string, keys []string) *anthropicClient {
	p := &pool{}
	for _, k := range keys {
		p.creds = append(p.creds, credential{key: k})
	}
	return &anthropicClient{strings.TrimSuffix(base, "/"), p, httpClient(15 * time.Minute)}
}
func (c *anthropicClient) Available(now time.Time) bool { return c.pool.available(now) }
func (c *anthropicClient) Complete(ctx context.Context, m model.ModelSpec, r Request) (Response, error) {
	key, ok := c.pool.take(time.Now())
	if !ok {
		return Response{}, ErrCredentialsBackoff
	}
	system := []any{map[string]any{"type": "text", "text": r.System + "\n\n" + r.DurableSpec + "\n\n" + r.Plan, "cache_control": map[string]any{"type": "ephemeral"}}}
	toWire := map[string]string{}
	fromWire := map[string]string{}
	for _, t := range r.Tools {
		wire := anthropicToolName(t.Name)
		toWire[t.Name] = wire
		fromWire[wire] = t.Name
	}
	msgs := []any{}
	for _, x := range r.Messages {
		content := []any{}
		if x.Content != "" {
			content = append(content, map[string]any{"type": "text", "text": x.Content})
		}
		for _, image := range x.Images {
			if image.Data != "" {
				mediaType := image.MediaType
				if mediaType == "" {
					mediaType = "image/png"
				}
				content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": image.Data}})
			} else if image.URL != "" {
				content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": image.URL}})
			}
		}
		for _, tc := range x.ToolCalls {
			name := tc.Name
			if wire := toWire[name]; wire != "" {
				name = wire
			}
			content = append(content, map[string]any{"type": "tool_use", "id": tc.ID, "name": name, "input": tc.Arguments})
		}
		if x.Role == "tool" {
			content = []any{map[string]any{"type": "tool_result", "tool_use_id": x.ToolCallID, "content": x.Content}}
		}
		msgs = append(msgs, map[string]any{"role": mapRole(x.Role), "content": content})
	}
	body := map[string]any{"model": wireModel(m.ID), "max_tokens": min(r.MaxOutput, m.MaxOutput), "system": system, "messages": msgs}
	if m.Compat.SupportsReasoningEffort && r.Effort != model.EffortNone {
		body["output_config"] = map[string]any{"effort": m.Compat.EffortWireMap[r.Effort]}
	}
	if len(r.Tools) > 0 {
		var ts []any
		for _, t := range r.Tools {
			ts = append(ts, map[string]any{"name": toWire[t.Name], "description": t.Description, "input_schema": t.InputSchema})
		}
		body["tools"] = ts
	}
	b, _ := json.Marshal(body)
	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, "POST", c.base+"/v1/messages", bytes.NewReader(b))
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
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
	var wire struct {
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		Usage struct {
			Input      int `json:"input_tokens"`
			Output     int `json:"output_tokens"`
			CacheRead  int `json:"cache_read_input_tokens"`
			CacheWrite int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err = json.Unmarshal(raw, &wire); err != nil {
		return Response{}, err
	}
	msg := Message{Role: "assistant"}
	for _, b := range wire.Content {
		if b.Type == "text" {
			msg.Content += b.Text
		}
		if b.Type == "tool_use" {
			name := b.Name
			if internal := fromWire[name]; internal != "" {
				name = internal
			}
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{b.ID, name, b.Input})
		}
	}
	return Response{Message: msg, Usage: Usage{wire.Usage.Input + wire.Usage.CacheRead + wire.Usage.CacheWrite, wire.Usage.Output, wire.Usage.CacheRead, wire.Usage.CacheWrite}, StopReason: wire.StopReason, Latency: time.Since(start), Model: wire.Model}, nil
}

func anthropicToolName(name string) string {
	valid := len(name) > 0 && len(name) <= 128
	for i := 0; valid && i < len(name); i++ {
		c := name[i]
		valid = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
	}
	if valid {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("orrery_%x", sum[:12])
}

func mapRole(r string) string {
	if r == "assistant" {
		return r
	}
	return "user"
}
