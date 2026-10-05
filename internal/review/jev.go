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
	Client       *jev.Client
	Verification *Verification
}

// Verification is the latest successful check of the current change.
type Verification struct {
	Command string `json:"command"`
	Output  string `json:"output"`
}

var needsReviewQuestion = map[string]jev.Question{"needs_review": jev.Noul(
	"Does this change need a correctness review by a software engineer?",
	"It changes behaviour: executable logic, build or CI configuration, schemas, queries, infrastructure, or data a program consumes in a way that can break it.",
	"It is prose, documentation, comments, formatting, assets, fixtures, or data that no program's behaviour depends on.",
)}

var fileBugQuestion = map[string]jev.Question{"introduces_bug": jev.Noul(
	"Does this file's change introduce a correctness bug?",
	"The change contains a logic error, a broken edge case, wrong API use, a race, or a regression that a careful reviewer would flag.",
	"The change appears correct for its task; any remaining issues are style or preference.",
)}

var riskQuestions = map[string]jev.Question{
	"introduces_bug": jev.Noul(
		"Does this patch introduce a correctness bug?",
		"The patch contains a logic error, a broken edge case, wrong API use, a race, or a regression that a careful reviewer would flag.",
		"The patch appears correct for its task; any remaining issues are style or preference.",
	),
	"risk": jev.Score("How risky is this change to merge without an independent review?",
		"Low: a small, mechanical, or well-contained change.",
		"Medium: changes behaviour in a contained area.",
		"High: a broad or subtle behavioural change, or one touching concurrency, security, or data handling.",
	),
}

var findingQuestion = map[string]jev.Question{"real_bug": jev.Noul(
	"Is this reviewer finding a real correctness bug introduced by the patch?",
	"The finding identifies behaviour the patch gets wrong, such as a logic error, a broken edge case, wrong API use, a race, a security flaw, or a regression, and the patch supports it.",
	"The finding is about style, naming, documentation, missing tests, or preference; describes a problem that predates the patch; or is not supported by the patch.",
)}

func (j JevClassifier) NeedsReview(ctx context.Context, task string, files []File) ([]float64, error) {
	return j.each(ctx, len(files), needsReviewQuestion, "needs_review", func(i int) any {
		return fileState(task, files[i], needsReviewChars)
	})
}

func (j JevClassifier) FileBugs(ctx context.Context, task string, files []File) ([]float64, error) {
	return j.each(ctx, len(files), fileBugQuestion, "introduces_bug", func(i int) any {
		return fileState(task, files[i], fileBugChars)
	})
}

func (j JevClassifier) Findings(ctx context.Context, task string, findings []Finding) ([]float64, error) {
	return j.each(ctx, len(findings), findingQuestion, "real_bug", func(i int) any {
		state := map[string]any{"task": clip(task, 4_000), "finding": findings[i].Text, "patch": clip(findings[i].Patch, fileBugChars)}
		if j.Verification != nil {
			state["verification"] = j.Verification
		}
		return state
	})
}

func (j JevClassifier) Risk(ctx context.Context, task string, files []File) (float64, float64, error) {
	summaries := make([]string, len(files))
	patch := ""
	for i, f := range files {
		summaries[i] = f.Summary()
		if len(patch) < riskChars {
			patch += f.Patch
		}
	}
	resp, err := j.Client.Ask(ctx, map[string]any{"task": clip(task, 4_000), "files": summaries, "patch": clip(patch, riskChars)}, riskQuestions)
	if err != nil {
		return 0, 0, err
	}
	bug, risk := resp.Answers["introduces_bug"], resp.Answers["risk"]
	if bug.Noul == nil || risk.Score == nil || len(risk.Legend) < 2 {
		return 0, 0, fmt.Errorf("jev: incomplete risk answer")
	}
	return *bug.Noul, *risk.Score / float64(len(risk.Legend)-1), nil
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
