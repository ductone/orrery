// Package jev is a minimal client for TypeSafe's System One endpoint. Jev
// answers typed questions about a state (noul, choice, score) with calibrated
// probabilities; it does not generate text.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ductone/orrey/internal/classify"
)

const (
	DefaultBaseURL = "https://api.typesafe.ai"
	DefaultModel   = "jev-latest"
)

// The question and answer types are classify's, so a Client is a
// classify.Classifier.
type (
	Question = classify.Question
	Answer   = classify.Answer
	Usage    = classify.Usage
	Response = classify.Response
)

var (
	Noul   = classify.Noul
	Choice = classify.Choice
	Score  = classify.Score
)

type Client struct {
	key, baseURL, model string
	http                *http.Client
}

func New(key, baseURL, model string, timeout time.Duration) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if model == "" {
		model = DefaultModel
	}
	return &Client{key: key, baseURL: strings.TrimRight(baseURL, "/"), model: model, http: &http.Client{Timeout: timeout}}
}

// Ask sends one state with its questions. It does not retry: callers are
// shadow observers, and a missed answer is cheaper than a delayed one.
func (c *Client) Ask(ctx context.Context, state any, questions map[string]Question) (Response, error) {
	body, err := json.Marshal(map[string]any{"model": c.model, "state": state, "questions": questions})
	if err != nil {
		return Response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Response{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("jev: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 300)])))
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, fmt.Errorf("jev: decode response: %w", err)
	}
	for name := range questions {
		if _, ok := out.Answers[name]; !ok {
			return Response{}, errors.New("jev: response is missing answer " + name)
		}
	}
	return out, nil
}

var _ classify.Classifier = (*Client)(nil)
