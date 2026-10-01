package provider

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestBackoffFor(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"", defaultCredentialBackoff},
		{"7", 7 * time.Second},
		{"0", 0},
		{"99999", maxCredentialBackoff},
		{"soon", defaultCredentialBackoff},
	} {
		resp := &http.Response{Header: http.Header{}}
		if tc.header != "" {
			resp.Header.Set("Retry-After", tc.header)
		}
		if got := backoffFor(resp); got != tc.want {
			t.Errorf("Retry-After %q: %v, want %v", tc.header, got, tc.want)
		}
	}
	resp := &http.Response{Header: http.Header{"Retry-After": {time.Now().Add(20 * time.Second).UTC().Format(http.TimeFormat)}}}
	if got := backoffFor(resp); got <= 10*time.Second || got > 21*time.Second {
		t.Errorf("HTTP-date Retry-After gave %v", got)
	}
}

func TestWaitForCredentials(t *testing.T) {
	c := newOpenAI("http://unused", []string{"k"}, true)
	r := &Registry{clients: map[string]Client{"ramp": c}, configured: map[string]bool{"ramp": true}}
	ctx := context.Background()
	if err := r.WaitForCredentials(ctx, time.Second); err != nil {
		t.Fatalf("an available credential needs no wait: %v", err)
	}
	c.pool.backoff("k", 80*time.Millisecond)
	start := time.Now()
	if err := r.WaitForCredentials(ctx, time.Second); err != nil || time.Since(start) < 70*time.Millisecond {
		t.Fatalf("err=%v waited=%v", err, time.Since(start))
	}
	c.pool.backoff("k", time.Hour)
	if err := r.WaitForCredentials(ctx, time.Second); !errors.Is(err, ErrCredentialsBackoff) {
		t.Fatalf("a wait beyond the bound must not block: %v", err)
	}
	c.pool.backoff("k", 500*time.Millisecond)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := r.WaitForCredentials(cancelled, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled wait returns the context error: %v", err)
	}
	// One provider with a free credential is enough.
	other := newOpenAI("http://unused", []string{"k2"}, true)
	r.clients["openai"] = other
	if err := r.WaitForCredentials(ctx, 0); err != nil {
		t.Fatal(err)
	}
}

func TestModelRefusal(t *testing.T) {
	for _, tc := range []struct {
		err                 error
		refused, persistent bool
	}{
		{&HTTPError{403, `{"error":{"type":"permission_error","code":"provider_key_required"}}`}, true, true},
		{&HTTPError{404, `{"error":{"code":"model_not_found"}}`}, true, true},
		{&HTTPError{403, `{"error":{"message":"forbidden"}}`}, true, false},
		{&HTTPError{404, `not found`}, true, false},
		{&HTTPError{400, `{"error":{"message":"bad request"}}`}, false, false},
		{&HTTPError{429, `{"error":{"code":"credits_reserved"}}`}, false, false},
		{&HTTPError{401, `invalid key`}, false, false},
		{errors.New("dial tcp: refused"), false, false},
		{nil, false, false},
	} {
		refused, persistent := ModelRefusal(tc.err)
		if refused != tc.refused || persistent != tc.persistent {
			t.Errorf("%v: refused=%v persistent=%v", tc.err, refused, persistent)
		}
	}
}
