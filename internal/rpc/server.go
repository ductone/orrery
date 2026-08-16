// Package rpc exposes Orrery over newline-delimited JSON-RPC 2.0. The ACP mode
// implements the stable v1 lifecycle subset while preserving Orrery-specific
// events in _meta fields and extension methods.
package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/core"
	"github.com/ductone/orrey/internal/provider"
)

type Mode string

const (
	Native Mode = "rpc"
	ACP    Mode = "acp"
)

type Server struct {
	Engine  *core.Engine
	Mode    Mode
	Version string
	mu      sync.Mutex
	enc     *json.Encoder
}
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (s *Server) Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	s.enc = json.NewEncoder(output)
	scan := bufio.NewScanner(input)
	scan.Buffer(make([]byte, 64<<10), 8<<20)
	var requests sync.WaitGroup
	for scan.Scan() {
		var req request
		if err := json.Unmarshal(scan.Bytes(), &req); err != nil {
			s.write(response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		requests.Add(1)
		go func(r request) { defer requests.Done(); s.handle(ctx, r) }(req)
	}
	requests.Wait()
	return scan.Err()
}

func (s *Server) handle(ctx context.Context, req request) {
	result, err := s.dispatch(ctx, req)
	if len(req.ID) == 0 {
		return
	}
	res := response{JSONRPC: "2.0", ID: req.ID, Result: result}
	if err != nil {
		res.Result = nil
		res.Error = &rpcError{Code: -32000, Message: err.Error()}
	}
	s.write(res)
}

func (s *Server) dispatch(ctx context.Context, req request) (any, error) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion any `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := p.ProtocolVersion
		if version == nil {
			version = 1
		}
		if s.Mode == ACP {
			return map[string]any{"protocolVersion": version, "agentCapabilities": map[string]any{"loadSession": true, "sessionCapabilities": map[string]any{"list": map[string]any{}, "resume": map[string]any{}, "fork": map[string]any{}}}, "authMethods": []any{}, "agentInfo": map[string]any{"name": "Orrery", "version": s.Version}}, nil
		}
		return map[string]any{"name": "Orrery", "version": s.Version, "protocol": "orrery-jsonrpc-1", "capabilities": []string{"events", "input_required", "checkpoint", "restore", "fork", "compact"}}, nil
	case "session/new", "orrery/session/new":
		var p struct {
			CWD       string  `json:"cwd"`
			Workspace string  `json:"workspace"`
			BudgetUSD float64 `json:"budgetUsd"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err
		}
		if p.CWD == "" {
			p.CWD = p.Workspace
		}
		x, err := s.Engine.CreateIdle(ctx, p.CWD, p.BudgetUSD)
		if err != nil {
			return nil, err
		}
		return map[string]any{"sessionId": x.ID}, nil
	case "session/prompt", "orrery/session/prompt":
		return s.prompt(ctx, req.Params)
	case "session/cancel", "orrery/session/cancel":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(req.Params, &p)
		return map[string]any{"cancelled": s.Engine.Cancel(p.SessionID)}, nil
	case "session/load", "session/resume":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(req.Params, &p)
		x, err := s.Engine.Store().Session(ctx, p.SessionID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"sessionId": x.ID}, nil
	case "session/list", "orrery/session/list":
		xs, err := s.Engine.Store().Sessions(ctx)
		if err != nil {
			return nil, err
		}
		items := make([]map[string]any, 0, len(xs))
		for _, x := range xs {
			items = append(items, map[string]any{"sessionId": x.ID, "cwd": x.WorkspacePath, "title": x.Spec, "updatedAt": x.UpdatedAt.Format(time.RFC3339), "_meta": map[string]any{"status": x.Status, "phase": x.Phase, "spentUsd": x.SpentUSD}})
		}
		return map[string]any{"sessions": items}, nil
	case "session/fork", "orrery/session/fork":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(req.Params, &p)
		x, err := s.Engine.Fork(ctx, p.SessionID, nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"sessionId": x.ID}, nil
	case "session/delete", "orrery/session/delete":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(req.Params, &p)
		return map[string]any{}, s.Engine.Delete(ctx, p.SessionID)
	case "session/close":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(req.Params, &p)
		s.Engine.Cancel(p.SessionID)
		return map[string]any{}, nil
	case "orrery/session/checkpoint":
		var p struct{ SessionID, Label string }
		_ = json.Unmarshal(req.Params, &p)
		cp, err := s.Engine.Checkpoint(ctx, p.SessionID, p.Label)
		return cp, err
	case "orrery/session/checkpoints":
		var p struct{ SessionID string }
		_ = json.Unmarshal(req.Params, &p)
		return s.Engine.Store().Checkpoints(ctx, p.SessionID)
	case "orrery/session/restore":
		var p struct{ SessionID, CheckpointID string }
		_ = json.Unmarshal(req.Params, &p)
		return map[string]any{}, s.Engine.RestoreCheckpoint(ctx, p.SessionID, p.CheckpointID, nil)
	case "orrery/session/compact":
		var p struct{ SessionID string }
		_ = json.Unmarshal(req.Params, &p)
		return map[string]any{}, s.Engine.Compact(ctx, p.SessionID, "rpc", nil)
	default:
		return nil, fmt.Errorf("method not found: %s", req.Method)
	}
}

func (s *Server) prompt(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		SessionID string          `json:"sessionId"`
		Prompt    json.RawMessage `json:"prompt"`
		Content   string          `json:"content"`
		RequestID string          `json:"requestId"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	text := strings.TrimSpace(p.Content)
	if text == "" {
		text = promptText(p.Prompt)
	}
	if p.RequestID == "" {
		p.RequestID = uuid.NewString()
	}
	emit := func(ev agentproto.AgentEvent) { s.event(p.SessionID, ev) }
	info, err := s.Engine.ContinueIntegrated(ctx, p.SessionID, text, p.RequestID, string(s.Mode), emit)
	if err != nil {
		return nil, err
	}
	if info.Duplicate {
		return map[string]any{"stopReason": "end_turn", "_meta": map[string]any{"duplicate": true, "turnId": info.TurnID}}, nil
	}
	result := <-info.Result
	stop := "end_turn"
	if result.Status == agentproto.Cancelled {
		stop = "cancelled"
	}
	if result.Status == agentproto.BudgetExhausted {
		stop = "max_tokens"
	}
	return map[string]any{"stopReason": stop, "_meta": map[string]any{"orreryStatus": result.Status, "result": result}}, nil
}

func promptText(raw json.RawMessage) string {
	var blocks []struct{ Type, Text string }
	if json.Unmarshal(raw, &blocks) == nil {
		var out []string
		for _, b := range blocks {
			if b.Type == "text" || b.Type == "" {
				out = append(out, b.Text)
			}
		}
		return strings.Join(out, "\n")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return ""
}

func (s *Server) event(sessionID string, ev agentproto.AgentEvent) {
	if s.Mode == Native {
		s.notify("orrery/event", map[string]any{"sessionId": sessionID, "event": ev})
		return
	}
	update := map[string]any{"sessionUpdate": "orrery_event", "_meta": map[string]any{"type": ev.Type, "data": ev.Data}}
	if ev.Type == "assistant.message" {
		if m, ok := ev.Data.(map[string]any); ok {
			b, _ := json.Marshal(m["message"])
			var message provider.Message
			_ = json.Unmarshal(b, &message)
			update = map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": message.Content}}
		}
	}
	if ev.Type == "input.required" {
		update = map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": inputQuestion(ev.Data)}, "_meta": map[string]any{"orreryInputRequired": ev.Data}}
	}
	s.notify("session/update", map[string]any{"sessionId": sessionID, "update": update})
}

func inputQuestion(v any) string {
	b, _ := json.Marshal(v)
	var x agentproto.InputRequest
	if json.Unmarshal(b, &x) == nil {
		return x.Question
	}
	return fmt.Sprint(v)
}
func (s *Server) notify(method string, params any) {
	s.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}
func (s *Server) write(v any) { s.mu.Lock(); defer s.mu.Unlock(); _ = s.enc.Encode(v) }
