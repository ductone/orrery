package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}
type rpcResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}
type Client interface {
	Call(context.Context, string, any) (json.RawMessage, error)
	Close() error
}
type Server struct {
	Name    string
	cfg     config.MCPConfig
	client  Client
	mu      sync.RWMutex
	tools   []provider.Tool
	pending bool
	logRoot string
}
type Manager struct{ servers map[string]*Server }

func New(ctx context.Context, cfg map[string]config.MCPConfig, logRoot string) (*Manager, error) {
	m := &Manager{servers: map[string]*Server{}}
	for name, c := range cfg {
		var cl Client
		var err error
		switch c.Transport {
		case "http":
			cl = &httpClient{url: c.URL, auth: c.AuthHeader, headers: c.Headers, http: &http.Client{Timeout: 2 * time.Minute}}
		case "stdio":
			cl, err = newStdio(ctx, c.Command)
		default:
			err = fmt.Errorf("unknown transport %q", c.Transport)
		}
		if err != nil {
			m.Close()
			return nil, fmt.Errorf("mcp %s: %w", name, err)
		}
		s := &Server{Name: name, cfg: c, client: cl, logRoot: logRoot}
		if err = s.initialize(ctx); err != nil {
			cl.Close()
			m.Close()
			return nil, fmt.Errorf("mcp %s: %w", name, err)
		}
		m.servers[name] = s
	}
	return m, nil
}
func (m *Manager) Close() error {
	var es []error
	for _, s := range m.servers {
		es = append(es, s.client.Close())
	}
	return errors.Join(es...)
}
func (s *Server) initialize(ctx context.Context) error {
	_, err := s.client.Call(ctx, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "orrery", "version": "0.3"}})
	if err != nil {
		return err
	}
	_, _ = s.client.Call(ctx, "notifications/initialized", nil)
	return s.refresh(ctx)
}
func (s *Server) refresh(ctx context.Context) error {
	raw, err := s.client.Call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return err
	}
	var x struct {
		Tools []struct {
			Name, Description string
			InputSchema       map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err = json.Unmarshal(raw, &x); err != nil {
		return err
	}
	defs := make([]provider.Tool, 0, len(x.Tools))
	for _, t := range x.Tools {
		defs = append(defs, provider.Tool{Name: s.Name + "." + t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	s.mu.Lock()
	s.tools = defs
	s.pending = false
	s.mu.Unlock()
	return nil
}
func (m *Manager) Definitions() []provider.Tool {
	var out []provider.Tool
	for _, s := range m.servers {
		s.mu.RLock()
		out = append(out, s.tools...)
		s.mu.RUnlock()
	}
	return out
}
func (m *Manager) Call(ctx context.Context, name string, args map[string]any) (any, error) {
	dot := strings.IndexByte(name, '.')
	if dot < 1 {
		return nil, errors.New("invalid MCP tool name")
	}
	s := m.servers[name[:dot]]
	if s == nil {
		return nil, errors.New("MCP server not found")
	}
	raw, err := s.client.Call(ctx, "tools/call", map[string]any{"name": name[dot+1:], "arguments": args})
	if err != nil {
		return nil, err
	}
	if len(raw) > 64<<10 {
		if err = os.MkdirAll(s.logRoot, 0700); err != nil {
			return nil, err
		}
		path := filepath.Join(s.logRoot, fmt.Sprintf("mcp-%d.json", time.Now().UnixNano()))
		if err = os.WriteFile(path, raw, 0600); err != nil {
			return nil, err
		}
		return map[string]any{"untrusted": true, "summary": string(raw[:32<<10]), "truncated": true, "log": path}, nil
	}
	var v any
	if err = json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return map[string]any{"untrusted": true, "result": v}, nil
}
func (m *Manager) MarkChanged(server string) {
	if s := m.servers[server]; s != nil {
		s.mu.Lock()
		s.pending = true
		s.mu.Unlock()
	}
}
func (m *Manager) PhaseBoundary(ctx context.Context) error {
	var es []error
	for _, s := range m.servers {
		// Tool definitions are cache-relevant. Refresh only here, never mid-phase;
		// an unchanged list preserves the exact mounted prefix.
		es = append(es, s.refresh(ctx))
	}
	return errors.Join(es...)
}

type httpClient struct {
	url, auth string
	headers   map[string]string
	http      *http.Client
	id        atomic.Int64
}

func (c *httpClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	b, _ := json.Marshal(rpcRequest{"2.0", c.id.Add(1), method, params})
	req, _ := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.auth != "" {
		req.Header.Set("Authorization", c.auth)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, raw)
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "data:") {
				raw = []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				break
			}
		}
	}
	var out rpcResponse
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("rpc %d: %s", out.Error.Code, out.Error.Message)
	}
	return out.Result, nil
}
func (c *httpClient) Close() error { return nil }

type stdioClient struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
	mu  sync.Mutex
	id  atomic.Int64
}

func newStdio(ctx context.Context, argv []string) (*stdioClient, error) {
	if len(argv) == 0 {
		return nil, errors.New("command required")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	return &stdioClient{cmd: cmd, in: in, out: bufio.NewReader(out)}, nil
}
func (c *stdioClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := json.Marshal(rpcRequest{"2.0", c.id.Add(1), method, params})
	if _, err := fmt.Fprintf(c.in, "Content-Length: %d\r\n\r\n%s", len(b), b); err != nil {
		return nil, err
	}
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := c.out.ReadString('\n')
		if err != nil {
			ch <- result{nil, err}
			return
		}
		n := 0
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			n, _ = strconv.Atoi(strings.TrimSpace(strings.SplitN(line, ":", 2)[1]))
			for {
				h, e := c.out.ReadString('\n')
				if e != nil {
					ch <- result{nil, e}
					return
				}
				if strings.TrimSpace(h) == "" {
					break
				}
			}
		} else {
			ch <- result{nil, errors.New("invalid MCP framing")}
			return
		}
		raw := make([]byte, n)
		_, err = io.ReadFull(c.out, raw)
		ch <- result{raw, err}
	}()
	select {
	case x := <-ch:
		if x.err != nil {
			return nil, x.err
		}
		var out rpcResponse
		if err := json.Unmarshal(x.b, &out); err != nil {
			return nil, err
		}
		if out.Error != nil {
			return nil, fmt.Errorf("rpc %d: %s", out.Error.Code, out.Error.Message)
		}
		return out.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (c *stdioClient) Close() error {
	_ = c.in.Close()
	if c.cmd.Process != nil {
		return c.cmd.Process.Kill()
	}
	return nil
}
