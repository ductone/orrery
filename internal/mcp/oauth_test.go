package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ductone/orrey/internal/config"
)

// fakeAS implements the OAuth endpoints Orrery drives: protected resource
// metadata, authorization server metadata, dynamic client registration, the
// authorization endpoint that redirects to the loopback callback, and the
// token endpoint that verifies PKCE and issues refresh tokens.
type fakeAS struct {
	t        *testing.T
	srv      *httptest.Server
	resource string

	mu            sync.Mutex
	clientID      string
	redirectURI   string
	codes         map[string]string
	refTokens     map[string]string
	nextAccess    int
	registrations int
}

func newFakeAS(t *testing.T) *fakeAS {
	f := &fakeAS{t: t, codes: map[string]string{}, refTokens: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAS) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/.well-known/oauth-protected-resource":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorization_servers": []string{f.srv.URL},
			"scopes_supported":      []string{"read", "write"},
		})
	case r.URL.Path == "/.well-known/oauth-authorization-server":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 f.srv.URL,
			"authorization_endpoint": f.srv.URL + "/authorize",
			"token_endpoint":         f.srv.URL + "/token",
			"registration_endpoint":  f.srv.URL + "/register",
			"scopes_supported":       []string{"read", "write"},
		})
	case r.URL.Path == "/register" && r.Method == http.MethodPost:
		var in struct {
			RedirectURIs []string `json:"redirect_uris"`
			GrantTypes   []string `json:"grant_types"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.RedirectURIs) == 0 {
			http.Error(w, "bad registration", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.clientID = "client-" + strconv.Itoa(f.registrations)
		f.registrations++
		f.redirectURI = in.RedirectURIs[0]
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": f.clientID, "redirect_uris": in.RedirectURIs})
	case r.URL.Path == "/authorize":
		q := r.URL.Query()
		if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" {
			http.Error(w, "missing pkce params", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		if q.Get("client_id") != f.clientID || q.Get("redirect_uri") != f.redirectURI || q.Get("resource") != f.resource {
			f.mu.Unlock()
			http.Error(w, "authorize mismatch", http.StatusBadRequest)
			return
		}
		code := "code-" + strconv.Itoa(len(f.codes))
		f.codes[code] = q.Get("code_challenge")
		f.mu.Unlock()
		loc := f.redirectURI + "?code=" + code + "&state=" + q.Get("state")
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusFound)
	case r.URL.Path == "/token" && r.Method == http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			challenge, ok := f.codes[r.Form.Get("code")]
			if !ok {
				http.Error(w, "bad code", http.StatusBadRequest)
				return
			}
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				http.Error(w, "bad verifier", http.StatusBadRequest)
				return
			}
			at := "tok-" + strconv.Itoa(f.nextAccess)
			f.nextAccess++
			rt := "rt-" + strconv.Itoa(len(f.refTokens))
			f.refTokens[rt] = at
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": at, "refresh_token": rt, "expires_in": 3600, "scope": "read write",
			})
		case "refresh_token":
			rt := r.Form.Get("refresh_token")
			if _, ok := f.refTokens[rt]; !ok {
				http.Error(w, "bad refresh token", http.StatusBadRequest)
				return
			}
			at := "tok-" + strconv.Itoa(f.nextAccess)
			f.nextAccess++
			f.refTokens[rt] = at
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": at, "expires_in": 3600})
		default:
			http.Error(w, "unknown grant", http.StatusBadRequest)
		}
	default:
		http.NotFound(w, r)
	}
}

// newOAuthMCPServer is a minimal MCP server that requires Bearer
// required.Load().(string); anything else gets a 401 with a resource
// metadata challenge pointing at the fake AS.
func newOAuthMCPServer(t *testing.T, as *fakeAS, required *atomic.Value) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+required.Load().(string) {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+as.srv.URL+`/.well-known/oauth-protected-resource", scope="read write"`)
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": json.Number("0"),
				"error": map[string]any{"code": -32001, "message": "unauthorized"},
			})
			return
		}
		var in rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := any(map[string]any{})
		if in.Method == "tools/list" {
			result = map[string]any{"tools": []any{map[string]any{
				"name": "ping", "description": "p", "inputSchema": map[string]any{"type": "object"},
			}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": in.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOAuthAuthorizeRefreshAndPersist(t *testing.T) {
	as := newFakeAS(t)
	var required atomic.Value
	required.Store("tok-0")
	mcpSrv := newOAuthMCPServer(t, as, &required)
	as.resource = canonicalResource(mcpSrv.URL)

	var browserCalls atomic.Int32
	oldBrowser := openBrowser
	openBrowser = func(u string) error {
		browserCalls.Add(1)
		go func() {
			if resp, err := http.Get(u); err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	t.Cleanup(func() { openBrowser = oldBrowser })

	storeRoot := t.TempDir()
	oauthRoot := t.TempDir()
	cfg := map[string]config.MCPConfig{
		"linear": {Transport: "http", URL: mcpSrv.URL, OAuth: true},
	}

	m, err := newManager(context.Background(), cfg, storeRoot, oauthRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if got := m.Definitions(); len(got) != 1 || got[0].Name != "linear.ping" {
		t.Fatalf("definitions = %+v", got)
	}
	if _, err := m.Call(context.Background(), "linear.ping", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if browserCalls.Load() != 1 {
		t.Fatalf("browser flow ran %d times, want 1 (authorization only)", browserCalls.Load())
	}

	// The server now rejects the current token; the 401 must trigger the
	// refresh-token path without another browser round trip.
	required.Store("tok-1")
	if _, err := m.Call(context.Background(), "linear.ping", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Call(context.Background(), "linear.ping", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if browserCalls.Load() != 1 {
		t.Fatalf("refresh required a browser: %d calls", browserCalls.Load())
	}

	// The current access token was persisted; a fresh manager reuses it.
	storeFile := filepath.Join(oauthRoot, "linear.json")
	stateBytes, err := os.ReadFile(storeFile)
	if err != nil {
		t.Fatal(err)
	}
	var st oauthState
	if err := json.Unmarshal(stateBytes, &st); err != nil {
		t.Fatal(err)
	}
	if st.AccessToken != "tok-1" || st.RefreshToken == "" || st.Resource != canonicalResource(mcpSrv.URL) {
		t.Fatalf("persisted state = %+v", st)
	}

	m2, err := newManager(context.Background(), cfg, storeRoot, oauthRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if got := m2.Definitions(); len(got) != 1 {
		t.Fatalf("reloaded definitions = %+v", got)
	}
	if browserCalls.Load() != 1 {
		t.Fatalf("reloaded manager re-authorized: %d browser calls", browserCalls.Load())
	}
}

func TestOAuthRejectsAuthHeaderCombination(t *testing.T) {
	_, err := newManager(context.Background(), map[string]config.MCPConfig{
		"x": {Transport: "http", URL: "http://example.invalid", OAuth: true, AuthHeader: "Bearer x"},
	}, t.TempDir(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
}

func TestCanonicalResource(t *testing.T) {
	cases := map[string]string{
		"https://mcp.linear.app/mcp":  "https://mcp.linear.app/mcp",
		"https://mcp.linear.app/mcp/": "https://mcp.linear.app/mcp",
		"HTTPS://MCP.LINEAR.APP/mcp":  "https://mcp.linear.app/mcp",
	}
	for in, want := range cases {
		if got := canonicalResource(in); got != want {
			t.Errorf("canonicalResource(%q) = %q, want %q", in, got, want)
		}
	}
}
