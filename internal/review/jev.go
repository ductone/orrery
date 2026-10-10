package review

import (
	"context"
	"fmt"
	"sync"

	"github.com/ductone/orrey/internal/jev"
)

// QuestionVersion identifies the review questions below. Bump it when their
// wording or criteria change, so recorded plans stay interpretable.
const QuestionVersion = "review/v3"

// Per-question patch budgets. Jev is billed per input token and scores one
// file or finding per call, so each call carries only what it is asked about.
const (
	needsReviewChars = 8_000
	fileBugChars     = 12_000
	riskChars        = 40_000
	jevConcurrency   = 16
)

// JevClassifier answers review questions with Jev, one noul per item, which is
// the pattern TypeSafe documents for comparable independent scores.
type JevClassifier struct {
	Client *jev.Client
}

// Verification is one command run since the last edit and its output.
type Verification struct {
	Command string `json:"command"`
	Output  string `json:"output"`
}

var needsReviewQuestion = map[string]jev.Question{"needs_review": jev.Noul(
	"Does this change need a correctness review by a software engineer?",
	"It changes behaviour: executable logic, build or CI configuration, schemas, queries, infrastructure, or data a program consumes in a way that can break it.",
	"It is prose, documentation, comments, formatting, assets, fixtures, or data that no program's behaviour depends on.",
)}

func (j JevClassifier) NeedsReview(ctx context.Context, task string, files []File) ([]float64, error) {
	return j.each(ctx, len(files), needsReviewQuestion, "needs_review", func(i int) any {
		return fileState(task, files[i], needsReviewChars)
	})
}

// each asks one noul per item concurrently. Any failure fails the batch: a
// partial answer would silently treat unanswered items as scored zero.
func (j JevClassifier) each(ctx context.Context, n int, question map[string]jev.Question, name string, state func(int) any) ([]float64, error) {
	scores := make([]float64, n)
	errs := make([]error, n)
	sem := make(chan struct{}, jevConcurrency)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()
			resp, err := j.Client.Ask(ctx, state(i), question)
			if err != nil {
				errs[i] = err
				return
			}
			a := resp.Answers[name]
			if a.Noul == nil {
				errs[i] = fmt.Errorf("jev: answer %s has no probability", name)
				return
			}
			scores[i] = *a.Noul
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return scores, nil
}

func fileState(task string, f File, limit int) map[string]any {
	return map[string]any{"task": clip(task, 4_000), "file": f.Path, "change": string(f.Status), "kind": string(f.Class), "lines_added": f.Added, "lines_removed": f.Removed, "patch": clip(f.Patch, limit)}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
