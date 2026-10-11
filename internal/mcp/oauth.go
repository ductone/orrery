package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// openBrowser launches the user's browser for the authorization step. Tests
// replace it to drive the flow without a person.
var openBrowser = func(u string) error {
	fmt.Fprintf(os.Stderr, "orrery: open this URL to authorize MCP access:\n%s\n", u)
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}

// authorizeTimeout bounds how long Orrery waits for the browser callback.
var authorizeTimeout = 5 * time.Minute

// oauthState is what persists between runs: the dynamically registered client
// and the current tokens for one MCP server.
type oauthState struct {
	ClientID      string    `json:"client_id"`
	ClientSecret  string    `json:"client_secret,omitempty"`
	RedirectURI   string    `json:"redirect_uri"`
	TokenEndpoint string    `json:"token_endpoint"`
	AuthEndpoint  string    `json:"authorization_endpoint"`
	Resource      string    `json:"resource"`
	AccessToken   string    `json:"access_token,omitempty"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	Expiry        time.Time `json:"expiry,omitempty"`
	Scope         string    `json:"scope,omitempty"`
}

// oauthClient implements the MCP authorization spec for remote servers:
// protected resource metadata (RFC 9728), authorization server metadata
// (RFC 8414 / OIDC discovery), dynamic client registration (RFC 7591), and
// the PKCE authorization code flow with a loopback redirect.
type oauthClient struct {
	serverURL string
	path      string
	http      *http.Client
	mu        sync.Mutex
	state     oauthState
}

func newOAuthClient(serverURL, storePath string, hc *http.Client) *oauthClient {
	o := &oauthClient{serverURL: serverURL, path: storePath, http: hc}
	if b, err := os.ReadFile(storePath); err == nil {
		_ = json.Unmarshal(b, &o.state)
	}
	if o.state.Resource != "" && o.state.Resource != canonicalResource(serverURL) {
		o.state = oauthState{}
	}
	return o
}

// token returns the current access token, refreshing it when it has expired.
// An empty result means no token is available yet; the server's 401 drives
// authorization.
func (o *oauthClient) token(ctx context.Context) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.state.AccessToken != "" && !o.state.Expiry.IsZero() && time.Until(o.state.Expiry) < 30*time.Second && o.state.RefreshToken != "" {
		if err := o.refreshLocked(ctx); err != nil {
			o.state.AccessToken = ""
		}
	}
	return o.state.AccessToken
}

// unauthorized handles a 401: refresh when possible, else run the full
// interactive authorization. failed is the token that was rejected, so
// concurrent callers do not each re-authorize.
func (o *oauthClient) unauthorized(ctx context.Context, failed, wwwAuth string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.state.AccessToken != "" && o.state.AccessToken != failed {
		return nil
	}
	if o.state.RefreshToken != "" && o.state.TokenEndpoint != "" {
		if err := o.refreshLocked(ctx); err == nil {
			return nil
		}
	}
	return o.authorizeLocked(ctx, wwwAuth)
}

var resourceMetadataRE = regexp.MustCompile(`resource_metadata="([^"]+)"`)
var scopeRE = regexp.MustCompile(`scope="([^"]+)"`)

func canonicalResource(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	u.RawQuery = ""
	return strings.TrimSuffix(u.String(), "/")
}

func wellKnownURLs(base *url.URL, suffix string) []string {
	origin := base.Scheme + "://" + base.Host
	path := strings.TrimSuffix(base.Path, "/")
	var out []string
	if path != "" {
		out = append(out, origin+"/.well-known/"+suffix+path)
	}
	return append(out, origin+"/.well-known/"+suffix)
}

func (o *oauthClient) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2025-03-26")
	resp, err := o.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

type asMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
}

// discover finds the authorization server for the MCP server and returns its
// metadata plus any scope hint.
func (o *oauthClient) discover(ctx context.Context, wwwAuth string) (asMetadata, string, error) {
	server, err := url.Parse(o.serverURL)
	if err != nil {
		return asMetadata{}, "", err
	}
	var scope string
	if m := scopeRE.FindStringSubmatch(wwwAuth); m != nil {
		scope = m[1]
	}
	prmURLs := wellKnownURLs(server, "oauth-protected-resource")
	if m := resourceMetadataRE.FindStringSubmatch(wwwAuth); m != nil {
		prmURLs = append([]string{m[1]}, prmURLs...)
	}
	issuer := server.Scheme + "://" + server.Host
	for _, u := range prmURLs {
		var prm struct {
			AuthorizationServers []string `json:"authorization_servers"`
			ScopesSupported      []string `json:"scopes_supported"`
		}
		if o.getJSON(ctx, u, &prm) == nil && len(prm.AuthorizationServers) > 0 {
			issuer = prm.AuthorizationServers[0]
			if scope == "" && len(prm.ScopesSupported) > 0 {
				scope = strings.Join(prm.ScopesSupported, " ")
			}
			break
		}
	}
	iu, err := url.Parse(issuer)
	if err != nil {
		return asMetadata{}, "", err
	}
	candidates := wellKnownURLs(iu, "oauth-authorization-server")
	candidates = append(candidates, wellKnownURLs(iu, "openid-configuration")...)
	for _, u := range candidates {
		var md asMetadata
		if o.getJSON(ctx, u, &md) == nil && md.AuthorizationEndpoint != "" && md.TokenEndpoint != "" {
			return md, scope, nil
		}
	}
	return asMetadata{}, "", fmt.Errorf("no OAuth authorization server metadata found for %s", issuer)
}

func (o *oauthClient) register(ctx context.Context, endpoint, redirect string) error {
	if endpoint == "" {
		return errors.New("authorization server does not support dynamic client registration")
	}
	body, _ := json.Marshal(map[string]any{
		"client_name":                "Orrery",
		"redirect_uris":              []string{redirect},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := o.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("client registration: HTTP %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ClientID == "" {
		return fmt.Errorf("client registration: invalid response: %s", raw)
	}
	o.state.ClientID, o.state.ClientSecret, o.state.RedirectURI = out.ClientID, out.ClientSecret, redirect
	return nil
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// listenLoopback reuses the registered redirect port when it is free, so a
// stored client registration stays valid across runs.
func (o *oauthClient) listenLoopback() (net.Listener, bool, error) {
	if u, err := url.Parse(o.state.RedirectURI); err == nil && o.state.ClientID != "" && u.Host != "" {
		if ln, err := net.Listen("tcp", u.Host); err == nil {
			return ln, true, nil
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	return ln, false, err
}

func (o *oauthClient) authorizeLocked(ctx context.Context, wwwAuth string) error {
	md, scope, err := o.discover(ctx, wwwAuth)
	if err != nil {
		return err
	}
	if o.state.TokenEndpoint != "" && o.state.TokenEndpoint != md.TokenEndpoint {
		o.state.ClientID = ""
	}
	ln, reused, err := o.listenLoopback()
	if err != nil {
		return err
	}
	defer ln.Close()
	if !reused {
		redirect := "http://" + ln.Addr().String() + "/callback"
		if err := o.register(ctx, md.RegistrationEndpoint, redirect); err != nil {
			return err
		}
	}
	o.state.AuthEndpoint, o.state.TokenEndpoint = md.AuthorizationEndpoint, md.TokenEndpoint
	o.state.Resource = canonicalResource(o.serverURL)
	if scope == "" && len(md.ScopesSupported) > 0 {
		scope = strings.Join(md.ScopesSupported, " ")
	}
	o.state.Scope = scope

	verifier := randomString(32)
	sum := sha256.Sum256([]byte(verifier))
	state := randomString(16)
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {o.state.ClientID},
		"redirect_uri":          {o.state.RedirectURI},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"resource":              {o.state.Resource},
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	authURL := md.AuthorizationEndpoint
	if strings.Contains(authURL, "?") {
		authURL += "&" + q.Encode()
	} else {
		authURL += "?" + q.Encode()
	}

	type result struct {
		code string
		err  error
	}
	ch := make(chan result, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		v := r.URL.Query()
		var res result
		switch {
		case v.Get("state") != state:
			res.err = errors.New("OAuth callback state mismatch")
		case v.Get("error") != "":
			res.err = fmt.Errorf("authorization denied: %s %s", v.Get("error"), v.Get("error_description"))
		case v.Get("code") == "":
			res.err = errors.New("OAuth callback missing code")
		default:
			res.code = v.Get("code")
		}
		if res.err != nil {
			http.Error(w, res.err.Error(), http.StatusBadRequest)
		} else {
			fmt.Fprintln(w, "Orrery is authorized. You can close this window.")
		}
		select {
		case ch <- res:
		default:
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()
	if err := openBrowser(authURL); err != nil {
		fmt.Fprintf(os.Stderr, "orrery: could not open browser (%v); open the URL above manually\n", err)
	}
	timer := time.NewTimer(authorizeTimeout)
	defer timer.Stop()
	var res result
	select {
	case res = <-ch:
	case <-timer.C:
		return errors.New("timed out waiting for OAuth authorization")
	case <-ctx.Done():
		return ctx.Err()
	}
	if res.err != nil {
		return res.err
	}
	return o.tokenRequest(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {res.code},
		"redirect_uri":  {o.state.RedirectURI},
		"code_verifier": {verifier},
	})
}

func (o *oauthClient) refreshLocked(ctx context.Context) error {
	err := o.tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {o.state.RefreshToken},
	})
	if err != nil {
		o.state.RefreshToken = ""
		o.state.AccessToken = ""
		_ = o.save()
	}
	return err
}

func (o *oauthClient) tokenRequest(ctx context.Context, form url.Values) error {
	form.Set("client_id", o.state.ClientID)
	if o.state.Resource != "" {
		form.Set("resource", o.state.Resource)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", o.state.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if o.state.ClientSecret != "" {
		req.SetBasicAuth(url.QueryEscape(o.state.ClientID), url.QueryEscape(o.state.ClientSecret))
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("token request: HTTP %d: %s", resp.StatusCode, raw)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("token request: invalid response")
	}
	o.state.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		o.state.RefreshToken = tok.RefreshToken
	}
	o.state.Expiry = time.Time{}
	if tok.ExpiresIn > 0 {
		o.state.Expiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	return o.save()
}

func (o *oauthClient) save() error {
	if err := os.MkdirAll(filepath.Dir(o.path), 0700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(o.state, "", "  ")
	tmp := o.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, o.path)
}
