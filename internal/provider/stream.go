package provider

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// Providers are asked to stream (server-sent events) so the idle timeout in
// sendWithDeadlines measures silence between bytes rather than the length of
// the whole generation. Each streamed response is assembled back into the
// provider's non-streamed JSON shape so one decoder handles both; a server
// that answers with plain JSON anyway is decoded as before.

func isEventStream(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
}

// readBody reads a non-streamed body, surfacing read failures (including an
// idle stall) instead of handing a truncated body to the decoder.
func readBody(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}

// readSSE calls fn for each event in r until fn reports done or the stream
// ends. A stream that ends before fn reports done was cut off.
func readSSE(r io.Reader, fn func(event, data string) (done bool, err error)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var event string
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if len(data) > 0 {
				done, err := fn(event, strings.Join(data, "\n"))
				if err != nil || done {
					return err
				}
			}
			event, data = "", nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(data) > 0 {
		if done, err := fn(event, strings.Join(data, "\n")); err != nil || done {
			return err
		}
	}
	return &ResponseDecodeError{Err: fmt.Errorf("stream ended early: %w", io.ErrUnexpectedEOF)}
}

// streamError reports an error event sent mid-stream. The request was
// accepted, so the failure is the provider's and worth retrying elsewhere.
func streamError(data string) error {
	return &HTTPError{Status: http.StatusBadGateway, Body: "stream error: " + data}
}

func decodeEvent(data string, v any) error {
	if err := json.Unmarshal([]byte(data), v); err != nil {
		return &ResponseDecodeError{Err: err}
	}
	return nil
}

// assembleAnthropic rebuilds a Messages API response from its event stream.
func assembleAnthropic(r io.Reader) ([]byte, error) {
	type block struct {
		Type     string         `json:"type"`
		Text     string         `json:"text,omitempty"`
		Thinking string         `json:"thinking,omitempty"`
		ID       string         `json:"id,omitempty"`
		Name     string         `json:"name,omitempty"`
		Input    map[string]any `json:"input,omitempty"`
		json     strings.Builder
	}
	var msg struct {
		Model      string         `json:"model"`
		StopReason string         `json:"stop_reason"`
		Content    []*block       `json:"content"`
		Usage      map[string]int `json:"usage"`
	}
	msg.Usage = map[string]int{}
	blocks := map[int]*block{}
	err := readSSE(r, func(_, data string) (bool, error) {
		var ev struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Model string         `json:"model"`
				Usage map[string]int `json:"usage"`
			} `json:"message"`
			ContentBlock *block `json:"content_block"`
			Delta        struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage map[string]int `json:"usage"`
		}
		if err := decodeEvent(data, &ev); err != nil {
			return false, err
		}
		switch ev.Type {
		case "message_start":
			msg.Model = ev.Message.Model
			for k, v := range ev.Message.Usage {
				msg.Usage[k] = v
			}
		case "content_block_start":
			if ev.ContentBlock != nil {
				blocks[ev.Index] = ev.ContentBlock
			}
		case "content_block_delta":
			b := blocks[ev.Index]
			if b == nil {
				return false, nil
			}
			switch ev.Delta.Type {
			case "text_delta":
				b.Text += ev.Delta.Text
			case "thinking_delta":
				b.Thinking += ev.Delta.Thinking
			case "input_json_delta":
				b.json.WriteString(ev.Delta.PartialJSON)
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				msg.StopReason = ev.Delta.StopReason
			}
			for k, v := range ev.Usage {
				msg.Usage[k] = v
			}
		case "message_stop":
			return true, nil
		case "error":
			return false, streamError(data)
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	indexes := make([]int, 0, len(blocks))
	for i := range blocks {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		b := blocks[i]
		if b.Type == "tool_use" {
			if s := strings.TrimSpace(b.json.String()); s != "" {
				b.Input = nil
				if err := json.Unmarshal([]byte(s), &b.Input); err != nil {
					return nil, &MalformedToolArgumentsError{Name: b.Name, Err: err}
				}
			}
		}
		msg.Content = append(msg.Content, b)
	}
	return json.Marshal(msg)
}

// assembleResponses returns the final response object of a Responses API
// event stream, which has the same shape as a non-streamed response.
func assembleResponses(r io.Reader) ([]byte, error) {
	var out json.RawMessage
	err := readSSE(r, func(_, data string) (bool, error) {
		var ev struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if err := decodeEvent(data, &ev); err != nil {
			return false, err
		}
		switch ev.Type {
		case "response.completed", "response.incomplete":
			out = ev.Response
			return true, nil
		case "response.failed", "error":
			return false, streamError(data)
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, &ResponseDecodeError{Err: errors.New("stream finished without a response")}
	}
	return out, nil
}

// assembleChat rebuilds a chat completion from its chunk stream.
func assembleChat(r io.Reader) ([]byte, error) {
	type call struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	var model, content, reasoning, finish string
	var usage json.RawMessage
	calls := map[int]*call{}
	err := readSSE(r, func(_, data string) (bool, error) {
		if strings.TrimSpace(data) == "[DONE]" {
			return true, nil
		}
		var ch struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content            string `json:"content"`
					Reasoning          string `json:"reasoning_content"`
					ReasoningAlternate string `json:"reasoning"`
					ToolCalls          []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				Finish string `json:"finish_reason"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
			Error json.RawMessage `json:"error"`
		}
		if err := decodeEvent(data, &ch); err != nil {
			return false, err
		}
		if len(ch.Error) > 0 && string(ch.Error) != "null" {
			return false, streamError(data)
		}
		if ch.Model != "" {
			model = ch.Model
		}
		if len(ch.Usage) > 0 && string(ch.Usage) != "null" {
			usage = ch.Usage
		}
		for _, c := range ch.Choices {
			content += c.Delta.Content
			reasoning += c.Delta.Reasoning + c.Delta.ReasoningAlternate
			if c.Finish != "" {
				finish = c.Finish
			}
			for _, tc := range c.Delta.ToolCalls {
				x := calls[tc.Index]
				if x == nil {
					x = &call{}
					calls[tc.Index] = x
				}
				if tc.ID != "" {
					x.ID = tc.ID
				}
				x.Function.Name += tc.Function.Name
				x.Function.Arguments += tc.Function.Arguments
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	indexes := make([]int, 0, len(calls))
	for i := range calls {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	ordered := make([]*call, 0, len(calls))
	for _, i := range indexes {
		ordered = append(ordered, calls[i])
	}
	if usage == nil {
		usage = json.RawMessage("{}")
	}
	return json.Marshal(map[string]any{
		"model": model,
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": content, "reasoning_content": reasoning, "tool_calls": ordered},
			"finish_reason": finish,
		}},
		"usage": usage,
	})
}

// readResponse returns the response JSON for a successful call, whether the
// provider streamed it or not.
func readResponse(resp *http.Response, limit int64, assemble func(io.Reader) ([]byte, error)) ([]byte, error) {
	if isEventStream(resp) {
		return assemble(resp.Body)
	}
	return readBody(resp.Body, limit)
}
