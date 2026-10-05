package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/model"
)

func TestHTTPBackoffIsPerRoute(t *testing.T) {
	for _, status := range []int{429, 503} {
		for _, kind := range []string{"responses", "chat", "anthropic"} {
			t.Run(kind+"/"+http.StatusText(status), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Retry-After", "300")
					w.WriteHeader(status)
				}))
				defer srv.Close()
				var c Client
				if kind == "anthropic" {
					c = newAnthropic(srv.URL, []string{"secret-key"})
				} else {
					c = newOpenAI(srv.URL, []string{"secret-key"}, kind == "responses")
				}
				r := &Registry{clients: map[string]Client{"ramp": c}}
				first := model.ModelSpec{ID: "ramp/first", MaxOutput: 100}
				other := model.ModelSpec{ID: "ramp/other", MaxOutput: 100}
				_, err := c.Complete(context.Background(), first, Request{MaxOutput: 10})
				var h *HTTPError
				if !errors.As(err, &h) || h.Status != status {
					t.Fatalf("error = %v", err)
				}
				if r.Available(first) || !r.Available(other) || r.ReadyAt(first).IsZero() || !r.ReadyAt(other).IsZero() {
					t.Fatal("only the failing route should cool down")
				}
				_, err = c.Complete(context.Background(), first, Request{})
				if !errors.Is(err, ErrCredentialsBackoff) {
					t.Fatalf("cooling route error = %v", err)
				}
				if err := r.WaitForCredentials(context.Background(), 0, other); err != nil {
					t.Fatalf("sibling route should not wait: %v", err)
				}
			})
		}
	}
}

func TestRouteBackoffRotatesKeysAndWaitsForEarliest(t *testing.T) {
	c := newOpenAI("http://unused", []string{"a", "b"}, true)
	first := model.ModelSpec{ID: "ramp/first"}
	other := model.ModelSpec{ID: "ramp/other"}
	model.Install([]model.ModelSpec{first, other})
	t.Cleanup(func() { model.Install(model.Catalog) })
	r := &Registry{clients: map[string]Client{"ramp": c}}
	c.pool.backoff("a", first.ID, time.Minute)
	if key, ok := c.pool.take(time.Now(), first.ID); !ok || key != "b" {
		t.Fatalf("key = %q, available = %v", key, ok)
	}
	c.pool.backoff("b", first.ID, 80*time.Millisecond)
	for _, key := range []string{"a", "b"} {
		c.pool.backoff(key, other.ID, time.Minute)
	}
	summary := r.CoolingSummary()
	if !strings.Contains(summary, first.ID+" until ") || !strings.Contains(summary, other.ID+" until ") {
		t.Fatalf("cooling summary = %q", summary)
	}
	start := time.Now()
	if err := r.WaitForCredentials(context.Background(), time.Second); err != nil || time.Since(start) < 70*time.Millisecond {
		t.Fatalf("earliest route: err = %v, waited = %v", err, time.Since(start))
	}
	if !r.Available(first) || r.Available(other) {
		t.Fatal("only the earliest route should have returned")
	}
}
