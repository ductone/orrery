package provider

import (
	"context"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"net/http"
	"sync"
	"time"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/router"
)

type Tool struct {
	Name, Description string
	InputSchema       map[string]any
}
type ToolCall struct {
	ID, Name  string
	Arguments map[string]any
}
type Message struct {
	Role, Content, ToolCallID, Reasoning string
	ToolCalls                            []ToolCall
}
type Request struct {
	System, DurableSpec, Plan string
	CacheKey                  string
	Messages                  []Message
	Tools                     []Tool
	MaxOutput                 int
	Effort                    model.Effort
	Strict                    bool
}
type Usage struct{ InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens int }
type Response struct {
	Message    Message
	Usage      Usage
	StopReason string
	Latency    time.Duration
	Model      string
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

type Registry struct {
	clients    map[string]Client
	configured map[string]bool
}

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
	_, ok := r.clients[providerName(spec.ID)]
	return ok
}
func (r *Registry) AvailableIDs() []string {
	var out []string
	for _, m := range model.Catalog {
		if r.Available(m) {
			out = append(out, m.ID)
		}
	}
	return out
}
func (r *Registry) CompleteOne(ctx context.Context, d router.Decision, build RequestBuilder) (Response, error) {
	ctx, span := otel.Tracer("orrery/provider").Start(ctx, "provider.request")
	defer span.End()
	span.SetAttributes(attribute.String("model", d.Model.ID))
	c, ok := r.clients[providerName(d.Model.ID)]
	if !ok {
		return Response{}, fmt.Errorf("provider for %s not configured", d.Model.ID)
	}
	req, err := build(d.Model, d)
	if err != nil {
		return Response{}, err
	}
	return c.Complete(ctx, d.Model, req)
}
func IsRetryable(err error) bool { return retryable(err) }
func (r *Registry) Complete(ctx context.Context, d router.Decision, build RequestBuilder) (Response, model.ModelSpec, error) {
	chain := []model.ModelSpec{d.Model}
	for _, m := range model.Catalog {
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
func retryable(err error) bool {
	if errors.Is(err, ErrCredentialsBackoff) {
		return true
	}
	var h *HTTPError
	return errors.As(err, &h) && (h.Status == 429 || h.Status >= 500)
}
func httpClient(timeout time.Duration) *http.Client { return &http.Client{Timeout: timeout} }
