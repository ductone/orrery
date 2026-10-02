package provider

import (
	"context"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/router"
)

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
	// ReadOnly is transport metadata used to filter external MCP tools from
	// read-workspace sessions. It is not sent to model providers.
	ReadOnly          bool `json:"-"`
	ReadWorkspaceSafe bool `json:"-"`
}

func toolNameMaps(tools []Tool) (map[string]string, map[string]string) {
	toWire := make(map[string]string, len(tools))
	fromWire := make(map[string]string, len(tools))
	for _, tool := range tools {
		wire := wireToolName(tool.Name)
		toWire[tool.Name] = wire
		fromWire[wire] = tool.Name
	}
	return toWire, fromWire
}

func toolInputSchema(schema map[string]any) map[string]any {
	if schema != nil {
		return schema
	}
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}
}

type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}
type Image struct {
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Reasoning  string     `json:"reasoning,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	Images     []Image    `json:"images,omitempty"`
	// Harness marks a user-role message the harness wrote itself (a nudge,
	// a rejection, a worker handoff) rather than one a person sent. Providers
	// never see it; it keeps harness messages from counting as new
	// instructions.
	Harness bool `json:"harness,omitempty"`
}
type Request struct {
	System, DurableSpec, Plan string
	// Memory is the pinned, workspace-scoped memory set for this cache epoch
	// (session start, phase transition, or compaction). It is rendered after
	// System and before DurableSpec/Plan so a refresh invalidates only the
	// volatile suffix. Empty unless memory is enabled and injection is on.
	Memory                    string
	CacheKey                  string
	Messages                  []Message
	Tools                     []Tool
	MaxOutput                 int
	Effort                    model.Effort
	Strict                    bool
	// NoToolCalls keeps the tool definitions in the request but forbids calls.
	// Removing definitions instead would change the cached prefix and strand
	// a model whose history is full of tool use.
	NoToolCalls bool
}

// systemSections joins a request's system-level text in the cache-aware order
// System, Memory, DurableSpec, Plan: stable instructions first, then the
// volatile pinned-memory suffix, then the per-turn durable spec/plan. A
// memory refresh therefore invalidates only its own section, not the whole
// prefix.
func systemSections(r Request) []string {
	sections := []string{r.System}
	if r.Memory != "" {
		sections = append(sections, r.Memory)
	}
	return append(sections, r.DurableSpec, r.Plan)
}
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}
type Response struct {
	Message    Message
	Usage      Usage
	StopReason string
	// Truncated reports that the response stopped at the output token limit.
	Truncated bool
	// OutputKinds lists the response's content block or output item types in
	// order, so an empty message can be explained after the fact.
	OutputKinds []string
	Latency     time.Duration
	Model       string
}
type Client interface {
	Complete(context.Context, model.ModelSpec, Request) (Response, error)
}

var ErrCredentialsBackoff = errors.New("all credentials in backoff")

type RequestBuilder func(model.ModelSpec, router.Decision) (Request, error)

type credential struct {
	key          string
	backoffUntil time.Time
}
type pool struct {
	mu    sync.Mutex
	next  int
	creds []credential
}

func (p *pool) take(now time.Time) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for range p.creds {
		idx := p.next % len(p.creds)
		p.next++
		if now.After(p.creds[idx].backoffUntil) {
			return p.creds[idx].key, true
		}
	}
	return "", false
}
func (p *pool) backoff(key string, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.creds {
		if p.creds[i].key == key {
			p.creds[i].backoffUntil = time.Now().Add(d)
		}
	}
}

func (p *pool) available(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, cred := range p.creds {
		if !now.Before(cred.backoffUntil) {
			return true
		}
	}
	return false
}

type availability interface {
	Available(time.Time) bool
	// ReadyAt is when the client's earliest credential leaves backoff.
	ReadyAt() time.Time
}

const (
	defaultCredentialBackoff = 30 * time.Second
	maxCredentialBackoff     = 5 * time.Minute
)

// backoffFor is how long a credential rests after a rate limit or server
// error: the provider's Retry-After when it sends one (bounded), otherwise a
// default.
func backoffFor(resp *http.Response) time.Duration {
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return min(time.Duration(secs)*time.Second, maxCredentialBackoff)
		}
		if at, err := http.ParseTime(v); err == nil {
			return min(max(time.Until(at), 0), maxCredentialBackoff)
		}
	}
	return defaultCredentialBackoff
}

// readyAt returns when the earliest credential leaves backoff; the zero time
// means one is usable now.
func (p *pool) readyAt(now time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	var earliest time.Time
	for _, cred := range p.creds {
		if !now.Before(cred.backoffUntil) {
			return time.Time{}
		}
		if earliest.IsZero() || cred.backoffUntil.Before(earliest) {
			earliest = cred.backoffUntil
		}
	}
	return earliest
}

// WaitForCredentials blocks until some configured provider has a usable
// credential, when that will happen within maxWait. Credential backoff is a
// short, known wait (a rate limit, or credits reserved by requests still in
// flight); failing the whole task over it would throw away the work done so
// far. It returns ErrCredentialsBackoff when the wait would be longer, and
// the context's error if it ends first.
func (r *Registry) WaitForCredentials(ctx context.Context, maxWait time.Duration) error {
	now := time.Now()
	var earliest time.Time
	for _, c := range r.clients {
		a, ok := c.(availability)
		if !ok || a.Available(now) {
			return nil
		}
		if at := a.ReadyAt(); earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	if len(r.clients) == 0 {
		return nil
	}
	wait := earliest.Sub(now)
	if wait > maxWait {
		return ErrCredentialsBackoff
	}
	timer := time.NewTimer(max(wait, 0) + 10*time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type Registry struct {
	clients    map[string]Client
	configured map[string]bool
	// unavailable holds models a provider refused for this account, such as
	// ones that need a provider key the account lacks. Routing skips them
	// for the life of the process.
	unavailable sync.Map
	onRefusal   func(id, reason string)
}

// MarkUnavailable removes a model from routing for the life of the registry.
func (r *Registry) MarkUnavailable(id string) { r.unavailable.Store(id, true) }

// SetRefusalHook is told about refusals that certainly concern a model, so a
// caller can remember them beyond this process.
func (r *Registry) SetRefusalHook(f func(id, reason string)) { r.onRefusal = f }

func New(cfg config.Config) *Registry {
	r := &Registry{clients: map[string]Client{}, configured: map[string]bool{}}
	for name, p := range cfg.Providers {
		keys := append([]string(nil), p.Keys...)
		if p.APIKey != "" {
			keys = append(keys, p.APIKey)
		}
		if len(keys) == 0 {
			continue
		}
		base := p.BaseURL
		switch name {
		case "anthropic":
			if base == "" {
				base = "https://api.anthropic.com"
			}
			r.clients[name] = newAnthropic(base, keys)
		case "openai":
			if base == "" {
				base = "https://api.openai.com"
			}
			r.clients[name] = newOpenAI(base, keys, true)
		case "ramp":
			// Ramp Router fronts multiple vendors with the OpenAI Responses
			// API, so it reuses the Responses client verbatim.
			if base == "" {
				base = "https://api.router.com"
			}
			r.clients[name] = newOpenAI(base, keys, true)
		case "fireworks":
			if base == "" {
				base = "https://api.fireworks.ai/inference"
			}
			r.clients[name] = newOpenAI(base, keys, false)
		case "xai":
			if base == "" {
				base = "https://api.x.ai"
			}
			r.clients[name] = newOpenAI(base, keys, false)
		case "together":
			if base == "" {
				base = "https://api.together.xyz"
			}
			r.clients[name] = newOpenAI(base, keys, false)
		}
		r.configured[name] = true
	}
	return r
}
func providerName(id string) string {
	for i, c := range id {
		if c == '/' {
			return id[:i]
		}
	}
	return id
}
func wireModel(id string) string {
	for i, c := range id {
		if c == '/' {
			return id[i+1:]
		}
	}
	return id
}
func (r *Registry) Available(spec model.ModelSpec) bool {
	if _, refused := r.unavailable.Load(spec.ID); refused {
		return false
	}
	c, ok := r.clients[providerName(spec.ID)]
	if !ok {
		return false
	}
	if a, ok := c.(availability); ok {
		return a.Available(time.Now())
	}
	return true
}
func (r *Registry) AvailableIDs() []string {
	out := []string{}
	for _, m := range model.All() {
		if r.Available(m) {
			out = append(out, m.ID)
		}
	}
	return out
}
func (r *Registry) CompleteOne(ctx context.Context, d router.Decision, build RequestBuilder) (Response, error) {
	ctx, span := otel.Tracer("orrery/provider").Start(ctx, "provider.request")
	defer span.End()
	if timeout := d.Model.Compat.StreamIdleTimeout; timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	span.SetAttributes(attribute.String("model", d.Model.ID))
	c, ok := r.clients[providerName(d.Model.ID)]
	if !ok {
		return Response{}, fmt.Errorf("provider for %s not configured", d.Model.ID)
	}
	req, err := build(d.Model, d)
	if err != nil {
		return Response{}, err
	}
	resp, err := c.Complete(ctx, d.Model, req)
	if refused, persistent := ModelRefusal(err); refused {
		r.MarkUnavailable(d.Model.ID)
		if persistent && r.onRefusal != nil {
			r.onRefusal(d.Model.ID, err.Error())
		}
	}
	return resp, err
}
func IsRetryable(err error) bool { return retryable(err) }
func (r *Registry) Complete(ctx context.Context, d router.Decision, build RequestBuilder) (Response, model.ModelSpec, error) {
	chain := []model.ModelSpec{d.Model}
	for _, m := range model.All() {
		if m.ID != d.Model.ID && m.Tier == d.Model.Tier && r.Available(m) {
			chain = append(chain, m)
		}
	}
	var errs []error
	for _, m := range chain {
		c, ok := r.clients[providerName(m.ID)]
		if !ok {
			continue
		}
		dd := d
		dd.Model = m
		dd.EditDialect = m.EditDialect
		req, err := build(m, dd)
		if err != nil {
			return Response{}, m, err
		}
		resp, err := c.Complete(ctx, m, req)
		if err == nil {
			return resp, m, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", m.ID, err))
		if !retryable(err) {
			break
		}
	}
	return Response{}, d.Model, errors.Join(errs...)
}

type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("provider HTTP %d: %s", e.Status, e.Body) }

// MalformedToolArgumentsError reports a tool call whose arguments were not
// valid JSON (usually a truncated model response). It is recoverable: the
// engine feeds the error back to the model and asks for a smaller valid call
// instead of terminating the session.
type MalformedToolArgumentsError struct {
	Name string
	Err  error
}

func (e *MalformedToolArgumentsError) Error() string {
	return fmt.Sprintf("tool arguments for %s: %v", e.Name, e.Err)
}
func (e *MalformedToolArgumentsError) Unwrap() error { return e.Err }

// IsMalformedToolArguments reports whether err is a recoverable malformed
// tool-call-arguments failure.
func IsMalformedToolArguments(err error) bool {
	var m *MalformedToolArgumentsError
	return errors.As(err, &m)
}

// ResponseDecodeError reports a response body that could not be decoded
// (usually a truncated or corrupted HTTP response). It is transient: the
// provider stream hiccuped, so the request is safe to retry.
type ResponseDecodeError struct {
	Err error
}

func (e *ResponseDecodeError) Error() string { return "decode provider response: " + e.Err.Error() }
func (e *ResponseDecodeError) Unwrap() error { return e.Err }
func retryable(err error) bool {
	if errors.Is(err, ErrCredentialsBackoff) {
		return true
	}
	// A truncated/corrupted response body is transient; the same request can be
	// retried against the same model.
	var d *ResponseDecodeError
	if errors.As(err, &d) {
		return true
	}
	var h *HTTPError
	return errors.As(err, &h) && (h.Status == 429 || h.Status >= 500)
}

// ModelRefusal reports whether err is a provider refusing a specific model
// for this account rather than the request failing: no access to the model,
// a model that needs the account's own upstream key, or an unknown model.
// Persistent is set when the refusal certainly concerns the model itself (a
// provider error code says so), as opposed to a 403 that may be about the
// whole account.
func ModelRefusal(err error) (refused, persistent bool) {
	var h *HTTPError
	if !errors.As(err, &h) {
		return false, false
	}
	body := strings.ToLower(h.Body)
	for _, code := range modelRefusalCodes {
		if strings.Contains(body, `"`+code+`"`) {
			return true, true
		}
	}
	return h.Status == http.StatusForbidden || h.Status == http.StatusNotFound, false
}

// modelRefusalCodes are provider error codes that name a model as unusable.
var modelRefusalCodes = []string{"provider_key_required", "model_not_found", "model_not_available", "unsupported_model"}

func httpClient(timeout time.Duration) *http.Client { return &http.Client{Timeout: timeout} }
