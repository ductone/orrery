package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/router"
)

func TestCompleteOneTakesRefusedModelsOutOfRouting(t *testing.T) {
	status, body := 403, `{"error":{"code":"provider_key_required"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	r := New(config.Config{Providers: map[string]config.ProviderConfig{"openai": {APIKey: "k", BaseURL: srv.URL}}})
	var hooked []string
	r.SetRefusalHook(func(id, _ string) { hooked = append(hooked, id) })
	m, _ := model.Get("openai/gpt-5.6-terra")
	build := func(model.ModelSpec, router.Decision) (Request, error) { return Request{MaxOutput: 10}, nil }
	if _, err := r.CompleteOne(context.Background(), router.Decision{Model: m}, build); err == nil {
		t.Fatal("expected the refusal")
	}
	if r.Available(m) || slices.Contains(r.AvailableIDs(), m.ID) || !slices.Equal(hooked, []string{m.ID}) {
		t.Fatalf("available=%v hooked=%v", r.Available(m), hooked)
	}
	// A bare 403 is taken out of routing but not remembered beyond the process.
	other, _ := model.Get("openai/gpt-5.6-sol")
	status, body = 403, `forbidden`
	_, _ = r.CompleteOne(context.Background(), router.Decision{Model: other}, build)
	if r.Available(other) || len(hooked) != 1 {
		t.Fatalf("available=%v hooked=%v", r.Available(other), hooked)
	}
	// A rate limit is not a refusal.
	luna, _ := model.Get("openai/gpt-5.6-luna")
	status, body = 429, `{}`
	_, _ = r.CompleteOne(context.Background(), router.Decision{Model: luna}, build)
	if _, refused := r.unavailable.Load(luna.ID); refused {
		t.Fatal("a rate limit must not mark a model unavailable")
	}
}
