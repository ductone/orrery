package provider

import (
	"context"
	"fmt"
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
// within firstByte. A call that never answers is a retryable transport failure,
// and its route is cooled so the next call prefers another. The returned
// release must be called once the response body is done with.
func sendWithDeadlines(client *http.Client, req *http.Request, p *pool, key, model string, firstByte time.Duration) (*http.Response, func(), error) {
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
	return resp, func() { cancel(nil) }, nil
}
