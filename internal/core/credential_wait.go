package core

import (
	"context"
	"fmt"

	"github.com/ductone/orrey/internal/model"
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
	maxCredentialWait = 5 * time.Minute
	// maxCredentialWaitsPerTurn bounds repeated waits when a provider keeps
	// returning rate limits.
	maxCredentialWaitsPerTurn = 4
	// maxTransportWait bounds how long a turn retries the same model through
	// network failures (a dropped connection, a timeout) before rerouting; a
	// blip should never need the person.
	maxTransportWait = 2 * time.Minute
)

// decideWaiting routes, and when routing fails only because every credential
// is in backoff, waits for one to return and routes again.
func (e *Engine) decideWaiting(ctx context.Context, sid string, policy router.Policy, providers *provider.Registry, state *router.RoutingState, emit EmitFunc) (router.Decision, router.Explanation, error) {
	deadline := time.Now().Add(maxCredentialWait)
	for {
		decision, why, err := policy.Decide(ctx, *state)
		if err == nil || providers == nil {
			return decision, why, err
		}
		// Probe with cooling routes restored: only wait if backoff alone
		// prevents a compatible decision, not for an unrelated free model.
		probe := *state
		probe.AvailableModels = append([]string{}, state.AvailableModels...)
		for _, spec := range model.All() {
			if !providers.ReadyAt(spec).IsZero() {
				probe.AvailableModels = append(probe.AvailableModels, spec.ID)
			}
		}
		var recovering []model.ModelSpec
		probe.ExcludeModels = append([]string(nil), state.ExcludeModels...)
		for {
			recovery, _, probeErr := policy.Decide(ctx, probe)
			if probeErr != nil {
				break
			}
			if providers.ReadyAt(recovery.Model).IsZero() {
				break
			}
			recovering = append(recovering, recovery.Model)
			probe.ExcludeModels = append(probe.ExcludeModels, recovery.Model.ID)
		}
		if len(recovering) == 0 || !e.waitForRoutes(ctx, sid, providers, time.Until(deadline), emit, recovering...) {
			if cooling := providers.CoolingSummary(); cooling != "" {
				err = fmt.Errorf("routes cooling down: %s (%w)", cooling, err)
			}
			return decision, why, err
		}
		state.AvailableModels = providers.AvailableIDs()
	}
}

// waitForCredentials waits when no configured model is usable because every
// credential is backed off, and reports whether a credential came back.
func (e *Engine) waitForCredentials(ctx context.Context, sid string, providers *provider.Registry, emit EmitFunc) bool {
	if providers == nil || len(providers.AvailableIDs()) > 0 {
		return false
	}
	return e.waitForRoutes(ctx, sid, providers, maxCredentialWait, emit)
}

func (e *Engine) waitForRoutes(ctx context.Context, sid string, providers *provider.Registry, maxWait time.Duration, emit EmitFunc, specs ...model.ModelSpec) bool {
	start := time.Now()
	e.emit(ctx, sid, "routing.credential_wait", map[string]any{"max_wait": maxWait.String()}, emit)
	if err := providers.WaitForCredentials(ctx, maxWait, specs...); err != nil {
		e.emit(ctx, sid, "routing.credential_wait", map[string]any{"error": err.Error(), "waited": time.Since(start).Round(time.Millisecond).String()}, emit)
		return false
	}
	e.emit(ctx, sid, "routing.credential_wait", map[string]any{"resumed": true, "waited": time.Since(start).Round(time.Millisecond).String()}, emit)
	return true
}
