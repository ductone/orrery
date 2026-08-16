package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/ductone/orrey/internal/model"
	"io"
	"net/http"
	"time"
)

func (c *openAIClient) completeResponses(ctx context.Context, m model.ModelSpec, r Request) (Response, error) {
	key, ok := c.pool.take(time.Now())
	if !ok {
		return Response{}, ErrCredentialsBackoff
	}
	toWire, fromWire := toolNameMaps(r.Tools)
	instructions := r.System + "\n\n" + r.DurableSpec + "\n\n" + r.Plan
	input := []any{}
	for _, x := range r.Messages {
		if x.Role == "tool" {
			input = append(input, map[string]any{"type": "function_call_output", "call_id": x.ToolCallID, "output": x.Content})
			continue
		}
		if x.Content != "" || len(x.Images) > 0 {
			if len(x.Images) == 0 {
				input = append(input, map[string]any{"role": x.Role, "content": x.Content})
			} else {
				content := []any{}
				if x.Content != "" {
					textType := "input_text"
					if x.Role == "assistant" {
						textType = "output_text"
					}
					content = append(content, map[string]any{"type": textType, "text": x.Content})
				}
				for _, image := range x.Images {
					content = append(content, map[string]any{"type": "input_image", "image_url": imageURL(image)})
				}
				input = append(input, map[string]any{"role": x.Role, "content": content})
			}
		}
		for _, tc := range x.ToolCalls {
			b, _ := json.Marshal(tc.Arguments)
			name := tc.Name
			if wire := toWire[name]; wire != "" {
				name = wire
			}
			input = append(input, map[string]any{"type": "function_call", "call_id": tc.ID, "name": name, "arguments": string(b)})
		}
	}
	body := map[string]any{"model": wireModel(m.ID), "instructions": instructions, "input": input, "max_output_tokens": min(r.MaxOutput, m.MaxOutput), "store": false}
	if r.CacheKey != "" {
		body["prompt_cache_key"] = r.CacheKey
	}
	if m.Compat.SupportsReasoningEffort && r.Effort != model.EffortNone {
		body["reasoning"] = map[string]any{"effort": m.Compat.EffortWireMap[r.Effort]}
	}
	if len(r.Tools) > 0 {
		tools := []any{}
		for _, t := range r.Tools {
			schema := toolInputSchema(t.InputSchema)
			strict := r.Strict && m.Compat.SupportsStrictTools && supportsStrictSchema(schema)
			if strict {
				schema = strictifySchema(schema)
			}
			tool := map[string]any{"type": "function", "name": toWire[t.Name], "description": t.Description, "parameters": schema}
			if strict {
				tool["strict"] = true
			}
			tools = append(tools, tool)
		}
		body["tools"] = tools
	}
	b, _ := json.Marshal(body)
	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, "POST", c.base+"/v1/responses", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			c.pool.backoff(key, 30*time.Second)
		}
		return Response{}, &HTTPError{resp.StatusCode, string(raw)}
	}
	var out struct {
		Model  string `json:"model"`
		Status string `json:"status"`
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			Input   int `json:"input_tokens"`
			Output  int `json:"output_tokens"`
			Details struct {
				Cached     int `json:"cached_tokens"`
				CacheWrite int `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return Response{}, err
	}
	msg := Message{Role: "assistant"}
	for _, item := range out.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					msg.Content += part.Text
				}
			}
		case "function_call":
			args := map[string]any{}
			if err = json.Unmarshal([]byte(item.Arguments), &args); err != nil {
				return Response{}, fmt.Errorf("tool arguments: %w", err)
			}
			name := item.Name
			if internal := fromWire[name]; internal != "" {
				name = internal
			}
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{item.CallID, name, args})
		}
	}
	return Response{Message: msg, Usage: Usage{InputTokens: out.Usage.Input, OutputTokens: out.Usage.Output, CacheReadTokens: out.Usage.Details.Cached, CacheWriteTokens: out.Usage.Details.CacheWrite}, StopReason: out.Status, Latency: time.Since(start), Model: out.Model}, nil
}
