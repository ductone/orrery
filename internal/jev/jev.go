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
)

const (
	DefaultBaseURL = "https://api.typesafe.ai"
	DefaultModel   = "jev-latest"
)

// Question is one named question. Criteria is {"true":..,"false":..} for a
// noul, an option→description map for a choice, and an ordered []string of
// level descriptions for a score.
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

func Noul(instructions, whenTrue, whenFalse string) Question {
	return Question{Type: "noul", Instructions: instructions, Criteria: map[string]string{"true": whenTrue, "false": whenFalse}}
}

func Choice(instructions string, options map[string]string) Question {
	return Question{Type: "choice", Instructions: instructions, Criteria: options}
}

func Score(instructions string, levels ...string) Question {
	return Question{Type: "score", Instructions: instructions, Criteria: levels}
}

// Answer is one typed answer. Noul answers carry only Noul; choice and score
// answers carry Probabilities and Confidence.
type Answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

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
