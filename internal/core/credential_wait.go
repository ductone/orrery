package core

import (
	"context"
	"time"

	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
)

// Credential backoff (a rate limit, or credits reserved by requests still in
// flight) is short and its end is known. When every configured credential is
// backed off, waiting beats failing: failing a turn, a worker, or a review
// discards the work around it, and a review retried immediately fails the
// same way.
const (
	maxCredentialWait = 2 * time.Minute
	// maxCredentialWaitsPerTurn bounds repeated waits when a provider keeps
	// returning rate limits.
	maxCredentialWaitsPerTurn = 4
)

// decideWaiting routes, and when routing fails only because every credential
// is in backoff, waits for one to return and routes again.
func (e *Engine) decideWaiting(ctx context.Context, sid string, policy router.Policy, providers *provider.Registry, state *router.RoutingState, emit EmitFunc) (router.Decision, router.Explanation, error) {
	decision, why, err := policy.Decide(ctx, *state)
	if err == nil || !e.waitForCredentials(ctx, sid, providers, emit) {
		return decision, why, err
	}
	state.AvailableModels = providers.AvailableIDs()
	return policy.Decide(ctx, *state)
}

// waitForCredentials waits when no configured model is usable because every
// credential is backed off, and reports whether a credential came back.
func (e *Engine) waitForCredentials(ctx context.Context, sid string, providers *provider.Registry, emit EmitFunc) bool {
	if providers == nil || len(providers.AvailableIDs()) > 0 {
		return false
	}
	start := time.Now()
	e.emit(ctx, sid, "routing.credential_wait", map[string]any{"max_wait": maxCredentialWait.String()}, emit)
	if err := providers.WaitForCredentials(ctx, maxCredentialWait); err != nil {
		e.emit(ctx, sid, "routing.credential_wait", map[string]any{"error": err.Error(), "waited": time.Since(start).Round(time.Millisecond).String()}, emit)
		return false
	}
	e.emit(ctx, sid, "routing.credential_wait", map[string]any{"resumed": true, "waited": time.Since(start).Round(time.Millisecond).String()}, emit)
	return len(providers.AvailableIDs()) > 0
}
