package provider

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ductone/orrery/config"
)

func TestCompleteTextReturnsTextAndCost(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range []string{
			`{"type":"message_start","message":{"model":"claude-haiku-5-5","usage":{"input_tokens":10000}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"two decisions"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1000}}`,
			`{"type":"message_stop"}`,
		} {
			_, _ = io.WriteString(w, "data: "+e+"\n\n")
		}
	}))
	defer srv.Close()
	r := New(config.Config{Providers: map[string]config.ProviderConfig{"anthropic": {APIKey: "k", BaseURL: srv.URL}}})
	resp, err := r.CompleteText(context.Background(), TextRequest{Model: "anthropic/claude-haiku-5-5", System: "sys", User: "the messages", MaxOutput: 2000, Effort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "two decisions" || resp.Model != "anthropic/claude-haiku-5-5" || resp.Usage.InputTokens != 10000 || resp.Usage.OutputTokens != 1000 {
		t.Fatalf("resp = %+v", resp)
	}
	// $0.10/MTok in, $0.50/MTok out.
	if want := 0.0015; math.Abs(resp.CostUSD-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", resp.CostUSD, want)
	}
	if got["model"] != "claude-haiku-5-5" || got["max_tokens"] != float64(2000) {
		t.Fatalf("request = %v", got)
	}
	if _, err := r.CompleteText(context.Background(), TextRequest{Model: "anthropic/nope"}); err == nil {
		t.Fatal("an unknown model must fail")
	}
}
