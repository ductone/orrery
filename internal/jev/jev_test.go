package jev

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAskSendsTypedQuestionsAndDecodesAnswers(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("path=%s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"stuck":{"type":"noul","noul":0.86},"kind":{"type":"choice","choice":"discipline","confidence":0.73,"probabilities":{"capability":0.14,"discipline":0.86}},"hard":{"type":"score","score":0.8,"confidence":0.0,"legend":{"0":"easy","1":"hard"},"probabilities":{"0":0.2,"1":0.8}}},"usage":{"input_tokens":378,"output_tokens":68}}`))
	}))
	defer server.Close()
	c := New("k", server.URL+"/", "", time.Second)
	resp, err := c.Ask(context.Background(), map[string]any{"task": "x"}, map[string]Question{
		"stuck": Noul("stuck?", "yes", "no"),
		"kind":  Choice("why?", map[string]string{"capability": "hard", "discipline": "redundant"}),
		"hard":  Score("how hard?", "easy", "hard"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != DefaultModel {
		t.Fatalf("model = %v", got["model"])
	}
	levels := got["questions"].(map[string]any)["hard"].(map[string]any)["criteria"].([]any)
	if len(levels) != 2 || levels[0] != "easy" {
		t.Fatalf("score criteria must be an ordered list, got %v", levels)
	}
	if resp.Model != "jev-1.13.0" || *resp.Answers["stuck"].Noul != 0.86 || resp.Answers["kind"].Choice != "discipline" || *resp.Answers["hard"].Score != 0.8 {
		t.Fatalf("answers = %+v", resp)
	}
}

func TestAskRejectsErrorsAndMissingAnswers(t *testing.T) {
	status := http.StatusTooManyRequests
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"answers":{}}`))
	}))
	defer server.Close()
	c := New("k", server.URL, "", time.Second)
	q := map[string]Question{"a": Noul("a?", "yes", "no")}
	if _, err := c.Ask(context.Background(), "s", q); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v", err)
	}
	status = http.StatusOK
	if _, err := c.Ask(context.Background(), "s", q); err == nil || !strings.Contains(err.Error(), "missing answer") {
		t.Fatalf("err = %v", err)
	}
}
