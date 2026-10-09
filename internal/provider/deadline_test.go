package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/model"
)

// hungServer accepts requests and never answers, like the provider calls that
// stalled for many minutes before failing over.
func hungServer(t *testing.T) *httptest.Server {
	t.Helper()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	t.Cleanup(func() {
		close(done)
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv
}

func TestHungResponseFailsOverWithinFirstByteDeadline(t *testing.T) {
	srv := hungServer(t)
	spec := model.ModelSpec{ID: "test/stall", Compat: model.Compat{FirstByteTimeout: 50 * time.Millisecond}}
	clients := map[string]Client{
		"chat":      newOpenAI(srv.URL, []string{"k"}, false),
		"responses": newOpenAI(srv.URL, []string{"k"}, true),
		"anthropic": newAnthropic(srv.URL, []string{"k"}),
	}
	for name, c := range clients {
		t.Run(name, func(t *testing.T) {
			started := time.Now()
			_, err := c.Complete(context.Background(), spec, Request{Messages: []Message{{Role: "user", Content: "hi"}}})
			if err == nil {
				t.Fatal("a call that never answers must fail")
			}
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				t.Fatalf("hung response took %v, want the first-byte deadline", elapsed)
			}
			if !strings.Contains(err.Error(), "model sent no response") {
				t.Fatalf("error = %v, want the stall to name its cause", err)
			}
			if !IsRetryable(err) || !IsTransportError(err) {
				t.Fatalf("a stall must fail over like any transport failure: %v", err)
			}
		})
	}
}

func TestStalledRouteCoolsDown(t *testing.T) {
	srv := hungServer(t)
	spec := model.ModelSpec{ID: "test/stall-cool", Compat: model.Compat{FirstByteTimeout: 50 * time.Millisecond}}
	c := newOpenAI(srv.URL, []string{"k"}, false)
	if _, err := c.Complete(context.Background(), spec, Request{Messages: []Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Fatal("a call that never answers must fail")
	}
	if c.ReadyAt(spec.ID).IsZero() {
		t.Fatal("a route that stalled must cool down so the next call prefers another")
	}
}

func TestSteadyResponseIsNotCutOff(t *testing.T) {
	// Headers arrive promptly, then the body streams slowly but steadily:
	// the whole response takes far longer than the first-byte deadline, yet
	// reads keep making progress so nothing is cut off.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		chunks := []string{
			"{\"model\":\"x\",\"choices\":[{\"message\":",
			"{\"role\":\"assistant\",\"content\":\"ok\"},",
			"\"finish_reason\":\"stop\"}],\"usage\":",
			"{\"prompt_tokens\":1,\"completion_tokens\":1}}",
		}
		for _, ch := range chunks {
			_, _ = w.Write([]byte(ch))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer srv.Close()
	spec := model.ModelSpec{ID: "test/steady", Compat: model.Compat{FirstByteTimeout: 100 * time.Millisecond}}
	c := newOpenAI(srv.URL, []string{"k"}, false)
	started := time.Now()
	resp, err := c.Complete(context.Background(), spec, Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("a response that keeps making progress must not be cut off: %v", err)
	}
	if resp.Message.Content != "ok" {
		t.Fatalf("content = %q", resp.Message.Content)
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Fatalf("the deadline fired early at %v, before the body finished", elapsed)
	}
}

func TestClientCancellationIsNotARetryableStall(t *testing.T) {
	srv := hungServer(t)
	spec := model.ModelSpec{ID: "test/cancel", Compat: model.Compat{FirstByteTimeout: 10 * time.Second}}
	c := newOpenAI(srv.URL, []string{"k"}, false)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := c.Complete(ctx, spec, Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("a cancelled call must fail")
	}
	if IsRetryable(err) {
		t.Fatalf("cancellation must not be retried: %v", err)
	}
}
