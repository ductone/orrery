package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/config"
)

func TestManagerDefinition(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := New(map[string]config.LSPConfig{"fake": {Command: []string{os.Args[0], "-test.run=TestLSPHelper", "--", "lsp-helper"}, Extensions: []string{".go"}, LanguageID: "go"}})
	defer m.Close()
	result, err := m.Call(context.Background(), root, Request{Operation: "definition", Path: "main.go", Line: 0})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(result)
	if !strings.Contains(string(b), "main.go") || !strings.Contains(string(b), `"line":2`) {
		t.Fatalf("result=%s", b)
	}
}

func TestWorkspaceBoundary(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "x.go")
	_ = os.WriteFile(outside, []byte("x"), 0600)
	if _, err := inside(root, outside); err == nil {
		t.Fatal("outside path accepted")
	}
}

func TestLSPHelper(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "lsp-helper" {
		return
	}
	r := bufio.NewReader(os.Stdin)
	for {
		body, err := readTestFrame(r)
		if err != nil {
			if err == io.EOF {
				os.Exit(0)
			}
			os.Exit(2)
		}
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		method := fmt.Sprint(req["method"])
		if method == "exit" {
			os.Exit(0)
		}
		id, hasID := req["id"]
		if !hasID {
			continue
		}
		var result any = map[string]any{}
		if method == "textDocument/definition" {
			result = []map[string]any{{"uri": "file:///workspace/main.go", "range": map[string]any{"start": map[string]any{"line": 2, "character": 1}, "end": map[string]any{"line": 2, "character": 4}}}}
		}
		writeTestFrame(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
}
func readTestFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "Content-Length") {
			length, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	if length < 0 {
		return nil, io.ErrUnexpectedEOF
	}
	body := make([]byte, length)
	_, err := io.ReadFull(r, body)
	return body, err
}
func writeTestFrame(v any) {
	b, _ := json.Marshal(v)
	fmt.Printf("Content-Length: %d\r\n\r\n%s", len(b), b)
}
