package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ductone/orrey/internal/model"
)

func TestFollowUpKeepsSystemSectionsStable(t *testing.T) {
	for _, name := range []string{"chat", "responses", "anthropic"} {
		t.Run(name, func(t *testing.T) {
			var bodies []map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				bodies = append(bodies, body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"status":"completed","output":[],"content":[{"type":"text","text":"ok"}],"usage":{}}`))
			}))
			defer srv.Close()
			var client Client = newOpenAI(srv.URL, []string{"key"}, name == "responses")
			m, _ := model.Get("openai/gpt-5.6-sol")
			if name == "anthropic" {
				client = newAnthropic(srv.URL, []string{"key"})
				m, _ = model.Get("anthropic/claude-fable-5")
			}
			r := Request{System: "stable instructions", Memory: "pinned memory", DurableSpec: "durable state", Plan: "plan snapshot", MaxOutput: 10}
			history := []Message{{Role: "user", Content: "first request"}, {Role: "assistant", Content: "first answer"}}
			for _, latest := range []string{"CURRENT REQUEST\nBuild it\nPENDING REPORT\nReport changes", "CURRENT REQUEST\nExplain it"} {
				r.Messages = append(append([]Message(nil), history...), Message{Role: "user", Content: latest})
				if _, err := client.Complete(context.Background(), m, r); err != nil {
					t.Fatal(err)
				}
			}
			var prefixes, tails []any
			for _, body := range bodies {
				switch name {
				case "chat":
					messages := body["messages"].([]any)
					prefixes = append(prefixes, messages[0])
					tails = append(tails, messages[len(messages)-1])
				case "responses":
					prefixes = append(prefixes, body["instructions"])
					input := body["input"].([]any)
					tails = append(tails, input[len(input)-1])
				case "anthropic":
					prefixes = append(prefixes, body["system"])
					messages := body["messages"].([]any)
					tails = append(tails, messages[len(messages)-1])
				}
			}
			if len(prefixes) != 2 || !reflect.DeepEqual(prefixes[0], prefixes[1]) {
				t.Fatalf("follow-up changed system sections: %+v", prefixes)
			}
			if reflect.DeepEqual(tails[0], tails[1]) {
				t.Fatal("follow-up did not change the conversation tail")
			}
		})
	}
}
