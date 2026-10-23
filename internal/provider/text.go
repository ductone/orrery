package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ductone/orrery/internal/model"
	"github.com/ductone/orrery/internal/router"
)

// TextRequest is one completion outside any session: a system prompt and
// one user message, no tools.
type TextRequest struct {
	// Model is a catalog model ID, such as anthropic/claude-haiku-5-5.
	Model        string
	System, User string
	// MaxOutput caps the output tokens; zero, or more than the model's cap,
	// means the model's cap.
	MaxOutput int
	// Effort is low, medium, high or xhigh; "" or one the model does not
	// support means the model's lowest.
	Effort string
}

// TextResponse is a TextRequest's answer, its usage and its cost at the
// model's catalog pricing.
type TextResponse struct {
	Text      string
	Model     string
	Usage     Usage
	CostUSD   float64
	Truncated bool
}

// CompleteText makes one completion with req.Model, not retried.
func (r *Registry) CompleteText(ctx context.Context, req TextRequest) (TextResponse, error) {
	m, ok := model.Get(req.Model)
	if !ok {
		return TextResponse{}, fmt.Errorf("model %s is not in the catalog", req.Model)
	}
	effort := model.Effort(strings.ToLower(req.Effort))
	if len(m.Effort) > 0 && !slices.Contains(m.Effort, effort) {
		effort = m.Effort[0]
	}
	maxOut := req.MaxOutput
	if maxOut <= 0 || maxOut > m.MaxOutput {
		maxOut = m.MaxOutput
	}
	d := router.Decision{Model: m, Effort: effort, EditDialect: m.EditDialect}
	resp, err := r.CompleteOne(ctx, d, func(model.ModelSpec, router.Decision) (Request, error) {
		return Request{System: req.System, Messages: []Message{{Role: "user", Content: req.User}}, MaxOutput: maxOut, Effort: effort}, nil
	})
	if err != nil {
		return TextResponse{}, err
	}
	u := resp.Usage
	return TextResponse{Text: resp.Message.Content, Model: m.ID, Usage: u, Truncated: resp.Truncated,
		CostUSD: m.Pricing.EstimateDetailed(u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)}, nil
}
