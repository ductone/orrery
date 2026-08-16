// Package lsp provides a deliberately read-only Language Server Protocol
// client. Semantic edits remain the hashline tool's responsibility.
package lsp

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ductone/orrey/internal/config"
)

type Manager struct {
	mu      sync.Mutex
	configs map[string]config.LSPConfig
	clients map[string]*client
}

func New(configs map[string]config.LSPConfig) *Manager {
	return &Manager{configs: cloneConfigs(configs), clients: map[string]*client{}}
}
func cloneConfigs(in map[string]config.LSPConfig) map[string]config.LSPConfig {
	out := map[string]config.LSPConfig{}
	for k, v := range in {
		v.Command = append([]string(nil), v.Command...)
		v.Extensions = append([]string(nil), v.Extensions...)
		out[k] = v
	}
	return out
}
func (m *Manager) Configured() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.configs) > 0 }
func (m *Manager) Reconfigure(configs map[string]config.LSPConfig) {
	m.Close()
	m.mu.Lock()
	m.configs = cloneConfigs(configs)
	m.clients = map[string]*client{}
	m.mu.Unlock()
}
func (m *Manager) Close() error {
	m.mu.Lock()
	clients := m.clients
	m.clients = map[string]*client{}
	m.mu.Unlock()
	var errs []error
	for _, c := range clients {
		errs = append(errs, c.close())
	}
	return errors.Join(errs...)
}

type Request struct {
	Operation, Path, Query, Server string
	Line, Character                int
}

func (m *Manager) Call(ctx context.Context, root string, r Request) (any, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	name, cfg, err := m.selectServer(r)
	if err != nil {
		return nil, err
	}
	c, err := m.get(ctx, root, name, cfg)
	if err != nil {
		return nil, err
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if r.Operation == "workspace_symbols" {
		return c.request(ctx, "workspace/symbol", map[string]any{"query": r.Query})
	}
	path, err := inside(root, r.Path)
	if err != nil {
		return nil, err
	}
	if err = c.open(ctx, path, cfg.LanguageID); err != nil {
		return nil, err
	}
	params := map[string]any{"textDocument": map[string]any{"uri": fileURI(path)}, "position": map[string]any{"line": max(0, r.Line), "character": max(0, r.Character)}}
	switch r.Operation {
	case "definition":
		return c.request(ctx, "textDocument/definition", params)
	case "references":
		params["context"] = map[string]any{"includeDeclaration": true}
		return c.request(ctx, "textDocument/references", params)
	case "hover":
		return c.request(ctx, "textDocument/hover", params)
	case "document_symbols":
		delete(params, "position")
		return c.request(ctx, "textDocument/documentSymbol", params)
	case "diagnostics":
		delete(params, "position")
		result, requestErr := c.request(ctx, "textDocument/diagnostic", params)
		if requestErr == nil {
			return result, nil
		}
		if cached := c.diagnosticsFor(fileURI(path)); cached != nil {
			return cached, nil
		}
		return nil, requestErr
	default:
		return nil, fmt.Errorf("unsupported LSP operation %q", r.Operation)
	}
}
func (m *Manager) selectServer(r Request) (string, config.LSPConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Server != "" {
		cfg, ok := m.configs[r.Server]
		if !ok {
			return "", cfg, fmt.Errorf("unknown LSP server %q", r.Server)
		}
		return r.Server, cfg, nil
	}
	ext := strings.ToLower(filepath.Ext(r.Path))
	for name, cfg := range m.configs {
		for _, candidate := range cfg.Extensions {
			if candidate == ext {
				return name, cfg, nil
			}
		}
	}
	if r.Operation == "workspace_symbols" && len(m.configs) == 1 {
		for name, cfg := range m.configs {
			return name, cfg, nil
		}
	}
	return "", config.LSPConfig{}, fmt.Errorf("no LSP server configured for %q", ext)
}
func (m *Manager) get(ctx context.Context, root, name string, cfg config.LSPConfig) (*client, error) {
	key := root + "\x00" + name
	m.mu.Lock()
	if c := m.clients[key]; c != nil {
		m.mu.Unlock()
		return c, nil
	}
	m.mu.Unlock()
	c, err := start(ctx, root, cfg)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if existing := m.clients[key]; existing != nil {
		m.mu.Unlock()
		_ = c.close()
		return existing, nil
	}
	m.clients[key] = c
	m.mu.Unlock()
	return c, nil
}

type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}
type client struct {
	opMu        sync.Mutex
	mu          sync.Mutex
	cmd         *exec.Cmd
	in          io.WriteCloser
	out         *bufio.Reader
	next        int
	root        string
	opened      map[string]openDocument
	diagnostics map[string]json.RawMessage
}
type openDocument struct {
	Hash    [32]byte
	Version int
}

func start(ctx context.Context, root string, cfg config.LSPConfig) (*client, error) {
	cmd := exec.CommandContext(context.WithoutCancel(ctx), cfg.Command[0], cfg.Command[1:]...)
	cmd.Dir = root
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	c := &client{cmd: cmd, in: stdin, out: bufio.NewReader(stdout), root: root, opened: map[string]openDocument{}, diagnostics: map[string]json.RawMessage{}}
	_, err = c.request(ctx, "initialize", map[string]any{"processId": os.Getpid(), "rootUri": fileURI(root), "workspaceFolders": []map[string]any{{"uri": fileURI(root), "name": filepath.Base(root)}}, "capabilities": map[string]any{"textDocument": map[string]any{"definition": map[string]any{}, "references": map[string]any{}, "hover": map[string]any{}, "documentSymbol": map[string]any{}, "diagnostic": map[string]any{}}, "workspace": map[string]any{"symbol": map[string]any{}}}})
	if err != nil {
		_ = c.close()
		return nil, err
	}
	if err = c.notify("initialized", map[string]any{}); err != nil {
		_ = c.close()
		return nil, err
	}
	return c, nil
}
func (c *client) open(ctx context.Context, path, language string) error {
	uri := fileURI(path)
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if language == "" {
		language = strings.TrimPrefix(filepath.Ext(path), ".")
	}
	hash := sha256.Sum256(content)
	if previous, ok := c.opened[uri]; ok {
		if previous.Hash == hash {
			return nil
		}
		previous.Version++
		if err = c.notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": previous.Version}, "contentChanges": []map[string]any{{"text": string(content)}}}); err != nil {
			return err
		}
		previous.Hash = hash
		c.opened[uri] = previous
		return nil
	}
	if err = c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": language, "version": 1, "text": string(content)}}); err != nil {
		return err
	}
	c.opened[uri] = openDocument{Hash: hash, Version: 1}
	return nil
}
func (c *client) request(ctx context.Context, method string, params any) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	id := c.next
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		msg, err := c.read()
		if err != nil {
			return nil, err
		}
		if msg.Method != "" {
			c.handle(msg)
			continue
		}
		var got int
		if json.Unmarshal(msg.ID, &got) != nil || got != id {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("lsp %s: %d %s", method, msg.Error.Code, msg.Error.Message)
		}
		var result any
		if len(msg.Result) > 0 && string(msg.Result) != "null" {
			if err = json.Unmarshal(msg.Result, &result); err != nil {
				return nil, err
			}
		}
		return result, nil
	}
}
func (c *client) notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}
func (c *client) handle(msg envelope) {
	if msg.Method == "textDocument/publishDiagnostics" {
		var p struct {
			URI         string          `json:"uri"`
			Diagnostics json.RawMessage `json:"diagnostics"`
		}
		if json.Unmarshal(msg.Params, &p) == nil {
			c.diagnostics[p.URI] = p.Diagnostics
		}
		return
	}
	if len(msg.ID) > 0 {
		_ = c.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(msg.ID), "error": map[string]any{"code": -32601, "message": "client method unsupported"}})
	}
}
func (c *client) diagnosticsFor(uri string) any {
	raw := c.diagnostics[uri]
	if raw == nil {
		return nil
	}
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}
func (c *client) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.in, "Content-Length: %d\r\n\r\n%s", len(b), b)
	return err
}
func (c *client) read() (envelope, error) {
	length := -1
	for {
		line, err := c.out.ReadString('\n')
		if err != nil {
			return envelope{}, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if key, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(key, "Content-Length") {
			length, _ = strconv.Atoi(strings.TrimSpace(value))
		}
	}
	if length < 0 {
		return envelope{}, errors.New("LSP response missing Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(c.out, body); err != nil {
		return envelope{}, err
	}
	var msg envelope
	err := json.Unmarshal(body, &msg)
	return msg, err
}
func (c *client) close() error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil {
		return nil
	}
	_ = c.write(map[string]any{"jsonrpc": "2.0", "id": c.next + 1, "method": "shutdown", "params": nil})
	_ = c.write(map[string]any{"jsonrpc": "2.0", "method": "exit"})
	_ = c.in.Close()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		_ = c.cmd.Process.Kill()
		err = <-done
	}
	c.cmd = nil
	return err
}
func inside(root, path string) (string, error) {
	if path == "" {
		return "", errors.New("path required")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	clean, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("LSP path escapes workspace")
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("LSP path must be a regular non-symlink file")
	}
	return clean, nil
}
func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}
