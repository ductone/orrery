package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/core"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
)

func testServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.Database = t.TempDir() + "/db.sqlite"
	s, err := store.Open(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	e := core.New(cfg, s, provider.New(cfg), nil)
	srv := New("127.0.0.1:0", e, "test-version")
	return httptest.NewServer(srv.http.Handler), s
}

func TestVersionedCapabilitiesAndOriginCheck(t *testing.T) {
	ts, st := testServer(t)
	defer ts.Close()
	defer st.Close()
	res, err := http.Get(ts.URL + "/api/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(b), `"api_version":1`) || !strings.Contains(string(b), `"idempotent_mutations":true`) || !strings.Contains(string(b), `"env_indirection":"string_prefix_v1"`) {
		t.Fatalf("status=%d body=%s", res.StatusCode, b)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/sessions", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://attacker.example")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("origin status=%d", res.StatusCode)
	}
}

func TestWebUIUsesDarkBrandPaletteAndCommandEnterComposer(t *testing.T) {
	ts, st := testServer(t)
	defer ts.Close()
	defer st.Close()
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	html := string(body)
	for _, expected := range []string{"--electric: #00ffcc", "--conductor: #5b39f5", "color-scheme: dark", "event.metaKey || event.ctrlKey", "state.current && !event.shiftKey", "composer new-session", "Messages sent now are queued FIFO", "/api/v1/sessions/${encodeURIComponent(state.current)}/messages", "request_id: crypto.randomUUID()", "result.queued", "state.pending = state.status === \"running\"", "type === \"session.started\"", "state.status = \"running\"; state.pending = true", "input.required", "Session checkpoints", "data-restore"} {
		if !strings.Contains(html, expected) {
			t.Fatalf("UI missing %q", expected)
		}
	}
	if strings.Contains(html, "prompt('Task for Orrery')") {
		t.Fatal("legacy prompt-based session creation is still present")
	}
}

func TestWebUIRendersExecCommandForToolCalls(t *testing.T) {
	ts, st := testServer(t)
	defer ts.Close()
	defer st.Close()
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	html := string(body)
	for _, expected := range []string{
		`typeof data.arguments.command === "string"`,
		`class="card-command">${escapeHTML(execCommand)}`,
		".card-command {",
	} {
		if !strings.Contains(html, expected) {
			t.Fatalf("UI missing exec command rendering %q", expected)
		}
	}
}

func TestWebUIRendersStructuredCompletionOutput(t *testing.T) {
	ts, st := testServer(t)
	defer ts.Close()
	defer st.Close()
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	html := string(body)
	for _, expected := range []string{
		"function structuredCompletion(value)",
		"function omitStructuredFields(value)",
		`const remainder = structured ? omitStructuredFields(result.result) : result.result;`,
		`["changes", "Changes"]`,
		`["verification", "Verification"]`,
		`["blockers", "Blockers"]`,
		"structuredCompletion(result.result)",
		"structuredCompletion(JSON.parse(content))",
		".structured-completion {",
	} {
		if !strings.Contains(html, expected) {
			t.Fatalf("UI missing structured completion rendering %q", expected)
		}
	}
}

func TestSSELastEventID(t *testing.T) {
	ts, st := testServer(t)
	defer ts.Close()
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, store.Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	_, _ = st.AddEvent(ctx, "s", "one", map[string]int{"n": 1})
	_, _ = st.AddEvent(ctx, "s", "two", map[string]int{"n": 2})
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/sessions/s/events", nil)
	req.Header.Set("Last-Event-ID", "s:1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	scan := bufio.NewScanner(res.Body)
	lines := []string{}
	for scan.Scan() {
		line := scan.Text()
		lines = append(lines, line)
		if line == "" {
			break
		}
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "id: s:2") || strings.Contains(joined, `\"n\":1`) {
		t.Fatalf("unexpected SSE: %s", joined)
	}
}

func TestDrainRejectsNewMutations(t *testing.T) {
	ts, st := testServer(t)
	defer ts.Close()
	defer st.Close()
	res, err := http.Post(ts.URL+"/api/v1/drain", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("drain status=%d", res.StatusCode)
	}
	res, err = http.Post(ts.URL+"/api/v1/sessions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("create while draining status=%d", res.StatusCode)
	}
	res, err = http.Get(ts.URL + "/api/v1/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ready while draining status=%d", res.StatusCode)
	}
}

func TestReadOnlySurfaceDoesNotMountMutations(t *testing.T) {
	cfg := config.Default()
	cfg.Database = t.TempDir() + "/db.sqlite"
	st, err := store.Open(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := core.New(cfg, st, provider.New(cfg), nil)
	view := NewView("127.0.0.1:0", e, "test")
	ts := httptest.NewServer(view.http.Handler)
	defer ts.Close()
	res, err := http.Post(ts.URL+"/api/v1/sessions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("mutation exposed on view surface: %d", res.StatusCode)
	}
}

func TestRuntimeReloadQueuesSecretOverrides(t *testing.T) {
	cfg := config.Default()
	cfg.Database = t.TempDir() + "/db.sqlite"
	st, err := store.Open(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New("127.0.0.1:0", core.New(cfg, st, provider.New(cfg), nil), "test")
	queued := make(chan map[string]string, 1)
	srv.SetRuntimeReload(func(env map[string]string) { queued <- env })
	ts := httptest.NewServer(srv.http.Handler)
	defer ts.Close()
	res, err := http.Post(ts.URL+"/api/v1/runtime-config/reload", "application/json", strings.NewReader(`{"env":{"OPENAI_API_KEY":"rotated"}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("reload status=%d", res.StatusCode)
	}
	if got := <-queued; got["OPENAI_API_KEY"] != "rotated" {
		t.Fatalf("queued env = %#v", got)
	}
}

func TestAddBudgetEndpoint(t *testing.T) {
	ts, st := testServer(t)
	defer ts.Close()
	defer st.Close()
	ctx := context.Background()
	sid := "budget-endpoint-session"
	if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: "budget endpoint", Phase: "plan", BudgetUSD: 4, SpentUSD: 4, Status: "interrupted"}); err != nil {
		t.Fatal(err)
	}
	post := func(body string) (*http.Response, string) {
		t.Helper()
		res, err := http.Post(ts.URL+"/api/v1/sessions/"+sid+"/budget", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		return res, string(raw)
	}

	res, body := post(`{"add_usd":6}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}
	if !strings.Contains(body, `"budget_usd":10`) || !strings.Contains(body, `"resumed":false`) {
		t.Fatalf("unexpected body: %s", body)
	}

	// A non-positive amount is a client error and must leave the ceiling alone.
	if res, body := post(`{"add_usd":0}`); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}
	s, err := st.Session(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if s.BudgetUSD != 10 {
		t.Fatalf("budget_usd=%v want 10", s.BudgetUSD)
	}

	if res, body := post(`{"add_usd":5}`); res.StatusCode != http.StatusOK {
		t.Fatalf("second increase status=%d body=%s", res.StatusCode, body)
	}
	if s, err = st.Session(ctx, sid); err != nil || s.BudgetUSD != 15 {
		t.Fatalf("increases must accumulate: %v %v", s.BudgetUSD, err)
	}
}

func TestSessionsExternalIDLookup(t *testing.T) {
	ts, st := testServer(t)
	defer ts.Close()
	defer st.Close()
	ctx := context.Background()
	for _, x := range []store.Session{
		{ID: "a", Spec: "a", Integration: "squire", ExternalID: "task-1"},
		{ID: "b", Spec: "b", Integration: "squire", ExternalID: "task-1", ExternalIncarnation: "2"},
		{ID: "c", Spec: "c", Integration: "other", ExternalID: "task-1"},
	} {
		if err := st.CreateSession(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	get := func(query string) []store.Session {
		t.Helper()
		res, err := http.Get(ts.URL + "/api/v1/sessions" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		var xs []store.Session
		if res.StatusCode != http.StatusOK || json.Unmarshal(raw, &xs) != nil || xs == nil {
			t.Fatalf("%s: status=%d body=%s", query, res.StatusCode, raw)
		}
		return xs
	}
	ids := func(xs []store.Session) string {
		out := []string{}
		for _, x := range xs {
			out = append(out, x.ID)
		}
		return strings.Join(out, ",")
	}
	for query, want := range map[string]string{
		"?external_id=task-1":                                 "a",
		"?external_id=task-1&incarnation=2":                   "b",
		"?integration=other&external_id=task-1":               "c",
		"?external_id=task-2":                                 "",
		"?integration=other&external_id=task-1&incarnation=2": "",
	} {
		if got := ids(get(query)); got != want {
			t.Errorf("%s = %q, want %q", query, got, want)
		}
	}
	if got := len(get("")); got != 3 {
		t.Fatalf("unfiltered list returned %d sessions, want 3", got)
	}
}
