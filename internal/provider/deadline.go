package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// defaultFirstByteTimeout bounds the wait for a response's headers. Nothing
	// is streamed yet, so a call that hangs sends no headers at all; treating
	// that silence as a retryable transport failure fails over in about a
	// minute instead of waiting out the model's whole idle budget. Models that
	// legitimately think longer before answering set first_byte_timeout.
	defaultFirstByteTimeout = 90 * time.Second
	// defaultStreamIdleTimeout bounds the silence between body bytes once a
	// response has started, for models that set no stream_idle_timeout.
	defaultStreamIdleTimeout = 10 * time.Minute
	// totalRequestCap bounds a whole call, however steadily it streams. It
	// is well above any idle timeout so it only stops runaway responses.
	totalRequestCap = time.Hour
)

// stallError reports a response that went silent. It is a timeout, so it is
// retried like any other transport failure.
type stallError struct {
	phase string
	after time.Duration
}

func (e *stallError) Error() string {
	return fmt.Sprintf("model sent no %s for %s", e.phase, e.after)
}
func (e *stallError) Timeout() bool   { return true }
func (e *stallError) Temporary() bool { return true }

// sendWithDeadlines performs req, failing when no response headers arrive
// within firstByte or when the body then goes idle (no bytes) for idle. A call
// that goes silent is a retryable transport failure, and its route is cooled
// so the next call prefers another. The returned release must be called once
// the response body is done with.
func sendWithDeadlines(client *http.Client, req *http.Request, p *pool, key, model string, firstByte, idle time.Duration) (*http.Response, func(), error) {
	if idle <= 0 {
		idle = defaultStreamIdleTimeout
	}
	if firstByte <= 0 {
		firstByte = defaultFirstByteTimeout
	}
	ctx, cancel := context.WithCancelCause(req.Context())
	timer := time.AfterFunc(firstByte, func() { cancel(&stallError{"response", firstByte}) })
	resp, err := client.Do(req.WithContext(ctx))
	timer.Stop()
	if stall, ok := context.Cause(ctx).(*stallError); ok {
		// The deadline fired: the call went silent. Drop any response that
		// raced in with it and let the caller retry or reroute.
		if resp != nil {
			resp.Body.Close()
		}
		p.backoff(key, model, defaultCredentialBackoff)
		cancel(nil)
		return nil, nil, stall
	}
	if err != nil {
		cancel(nil)
		return nil, nil, err
	}
	idleTimer := time.AfterFunc(idle, func() {
		p.backoff(key, model, defaultCredentialBackoff)
		cancel(&stallError{"data", idle})
	})
	resp.Body = &idleBody{ReadCloser: resp.Body, ctx: ctx, timer: idleTimer, idle: idle}
	return resp, func() { idleTimer.Stop(); cancel(nil) }, nil
}

// idleBody restarts the idle timer whenever bytes arrive, so a response that
// streams slowly but steadily is never cut off, and reports an idle stall as
// the stall rather than as a bare cancellation.
type idleBody struct {
	io.ReadCloser
	ctx   context.Context
	timer *time.Timer
	idle  time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(b.idle)
	}
	if err != nil && err != io.EOF {
		if stall, ok := context.Cause(b.ctx).(*stallError); ok {
			return n, stall
		}
	}
	return n, err
}
