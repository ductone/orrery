package core

import (
	"errors"
	"time"

	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/provider"
)

// failureClass is what kind of failure a model call met. Each class has one
// entry in failurePolicy, which is the whole of what the turn does next.
type failureClass string

const (
	// failMalformed: the model's tool-call arguments were not valid JSON,
	// usually a response cut off mid-call.
	failMalformed failureClass = "malformed"
	// failRefused: the provider refuses this model for the account (no
	// access, a missing provider key, an unknown model). The registry has
	// already taken the model out of routing.
	failRefused failureClass = "refused"
	// failRejected: the provider rejects this request for this model (a 4xx
	// over the history it was sent); another model usually serves it.
	failRejected failureClass = "rejected"
	// failTransport: the network failed (a dropped connection, a timeout, no
	// response). It says nothing about the model or the request.
	failTransport failureClass = "transport"
	// failCooling: the route or its credentials are in backoff.
	failCooling failureClass = "cooling"
	// failTransient: any other retryable provider error.
	failTransient failureClass = "transient"
	// failFatal: not retryable; the turn ends and asks the person.
	failFatal failureClass = "fatal"
)

// failureAction is what to do about one class of failure.
type failureAction struct {
	// retries is how many times the same model is retried before rerouting.
	retries int
	// window, when set, retries the same model until the failures have lasted
	// this long instead of counting retries.
	window time.Duration
	// backoff waits retryDelay(attempt) before a same-model retry.
	backoff bool
	// hint is a harness message added before a same-model retry.
	hint string
	// dropForRun takes the model out of routing for the rest of the run when
	// rerouting; otherwise it is excluded for this turn only (and not at all
	// for a cooling route, which recovers on its own).
	dropForRun bool
	// wait lets rerouting wait for cooling routes when nothing else is
	// eligible.
	wait bool
}

const malformedHint = "Your last tool call's arguments were not valid JSON (usually a truncated response). Do not retry the same large call. Issue one small, complete tool call at a time with valid JSON arguments."

var failurePolicy = map[failureClass]failureAction{
	failMalformed: {retries: 2, hint: malformedHint, dropForRun: true},
	failRefused:   {},
	failRejected:  {},
	failTransport: {window: maxTransportWait, backoff: true},
	failCooling:   {wait: true},
	failTransient: {retries: 2, backoff: true},
	failFatal:     {},
}

// classifyCallError sorts a failed model call into a failure class.
func classifyCallError(err error, providers *provider.Registry, m model.ModelSpec) failureClass {
	switch {
	case provider.IsMalformedToolArguments(err):
		return failMalformed
	}
	if refused, _ := provider.ModelRefusal(err); refused {
		return failRefused
	}
	switch {
	case modelRejected(err):
		return failRejected
	case !provider.IsRetryable(err):
		return failFatal
	case provider.IsTransportError(err):
		return failTransport
	case errors.Is(err, provider.ErrCredentialsBackoff) || !providers.ReadyAt(m).IsZero():
		return failCooling
	}
	return failTransient
}
