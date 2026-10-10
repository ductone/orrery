package review

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/ductone/orrey/internal/jev"
)

// The review cascade: changes beyond these sizes go straight to the full
// agent review; smaller ones are first offered to the Jev gate (which may
// approve), then a light reviewer (which may approve, reject a concrete bug,
// or escalate), and only then the full review.
const (
	CascadeMaxLines = 400
	CascadeMaxFiles = 10
	// GateThreshold is the probability at or above which the Jev gate
	// approves a change without a reviewer.
	GateThreshold = 0.7
	// LightReviewTurns bounds the light reviewer: enough to judge the inline
	// diff and read a file or two for context.
	LightReviewTurns = 3
	// LightReviewEffort is the light reviewer's reasoning effort: a quick look
	// at a small diff with its evidence, not the review phase's default high.
	LightReviewEffort = "low"
	// LightMarker opens the light reviewer's spec, so it is recognisable in
	// events and tests.
	LightMarker = "Review this proposed workspace diff (light review)."
)

// ReviewedSize is the changed lines and files a reviewer would read. Assets,
// lockfiles, generated and binary files do not count.
func ReviewedSize(files []File) (lines, count int) {
	for _, f := range files {
		switch f.Class {
		case Asset, Lockfile, Generated, BinaryC:
			continue
		}
		lines += f.Added + f.Removed
		count++
	}
	return lines, count
}

// Large reports whether a change goes straight to the full review.
func Large(files []File) bool {
	lines, count := ReviewedSize(files)
	return lines > CascadeMaxLines || count > CascadeMaxFiles
}

// TouchesTests reports whether a change deletes test files or removes lines
// from them. The Jev gate may not approve such a change on its own; a
// reviewer judges whether the test change is legitimate.
func TouchesTests(files []File) bool {
	for _, f := range files {
		if isTestFile(f.Path) && (f.Status == Deleted || f.Removed > 0) {
			return true
		}
		if f.OldPath != "" && isTestFile(f.OldPath) && !isTestFile(f.Path) {
			return true
		}
	}
	return false
}

func isTestFile(p string) bool {
	lower := strings.ToLower(p)
	name := path.Base(lower)
	switch {
	case strings.HasSuffix(name, "_test.go"), strings.HasSuffix(name, "_test.py"), strings.HasPrefix(name, "test_"),
		strings.Contains(name, ".test."), strings.Contains(name, ".spec."), strings.HasSuffix(name, "_spec.rb"):
		return true
	}
	for _, dir := range []string{"test/", "tests/", "__tests__/", "spec/", "testdata/"} {
		if strings.HasPrefix(lower, dir) || strings.Contains(lower, "/"+dir) {
			return true
		}
	}
	return false
}

// Gater approves small changes without a reviewer.
type Gater interface {
	// Gate scores the probability that the change can be approved without
	// a further code review.
	Gate(ctx context.Context, task string, files []File, evidence []Verification) (float64, error)
}

var gateQuestion = map[string]jev.Question{"approve": jev.Noul(
	"Can this change be approved without a further code review?",
	"It is simple and clearly correct for its task: documentation, comments, formatting, configuration with no behavioural risk, or a small, contained code change that the commands run since the last edit support.",
	"It may contain a bug, is too large or subtle to judge from the patch, changes behaviour the commands run do not cover, or its correctness is unclear.",
)}

// gateEvidenceChars bounds each command's output in the gate's state.
const gateEvidenceChars = 2_000

// Gate asks Jev whether the change can be approved as it stands.
func (j JevClassifier) Gate(ctx context.Context, task string, files []File, evidence []Verification) (float64, error) {
	summaries := make([]string, len(files))
	patch := ""
	for i, f := range files {
		summaries[i] = f.Summary()
		if len(patch) < riskChars {
			patch += f.Patch
		}
	}
	commands := make([]Verification, len(evidence))
	for i, v := range evidence {
		commands[i] = Verification{Command: v.Command, Output: clip(v.Output, gateEvidenceChars)}
	}
	resp, err := j.Client.Ask(ctx, map[string]any{"task": clip(task, 4_000), "files": summaries, "patch": clip(patch, riskChars), "commands_since_last_edit": commands}, gateQuestion)
	if err != nil {
		return 0, err
	}
	a := resp.Answers["approve"]
	if a.Noul == nil {
		return 0, fmt.Errorf("jev: answer approve has no probability")
	}
	return *a.Noul, nil
}

// LightSpec is the light reviewer's task: the request, the commands run since
// the last edit, and the whole diff.
func LightSpec(task string, files []File, evidence []Verification) string {
	var b strings.Builder
	b.WriteString(LightMarker + " Decide quickly from the request, the commands already run, and the diff below; read a file only when the diff alone is not enough.\n\n")
	b.WriteString("Return JSON with one of three outcomes:\n")
	b.WriteString("- approve: pass=true, findings=[]. The change is correct for its request.\n")
	b.WriteString("- reject: pass=false, findings listing each concrete correctness bug the patch introduces, with the file, the line, and the input or case that breaks. Reject only for a bug you can point to.\n")
	b.WriteString("- escalate: escalate=true, pass=false, findings=[], and escalate_reason saying what needs checking. Use it when you suspect a problem but cannot confirm it, or when judging the change needs more code than this review allows. A deeper review follows.\n")
	b.WriteString("Style, naming, and missing tests are not correctness bugs. Deleting or weakening tests is fine when the request calls for it or the tests were wrong; reject or escalate when it hides a regression.\n")
	b.WriteString("\nREQUEST\n" + clip(task, 4_000) + "\n")
	if len(evidence) > 0 {
		b.WriteString("\nCOMMANDS RUN SINCE THE LAST EDIT\n")
		for _, v := range evidence {
			b.WriteString("$ " + v.Command + "\n" + clip(v.Output, gateEvidenceChars) + "\n")
		}
	}
	b.WriteString("\nFILES\n")
	for _, f := range files {
		b.WriteString("- " + f.Summary() + "\n")
	}
	b.WriteString("\nDIFF\n")
	for _, f := range files {
		b.WriteString(f.Patch)
		if !strings.HasSuffix(f.Patch, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ProseOnly reports whether every changed file is prose or an asset, so the
// Jev gate may approve the change. A file classed as code by its name never
// is; lockfiles, generated files, binaries and assets don't count; every other
// file (docs, configuration, data) must be judged prose by c's needs-review
// question, below the plan's cutoff. It returns the scores it asked for and
// why the change is not prose-only, if it isn't.
func ProseOnly(ctx context.Context, c Classifier, task string, files []File) (bool, map[string]float64, string) {
	var other []File
	for _, f := range files {
		switch f.Class {
		case Code:
			return false, nil, "code changed: " + f.Path
		case Asset, Lockfile, Generated, BinaryC:
		default:
			other = append(other, f)
		}
	}
	if len(other) == 0 {
		return true, nil, ""
	}
	scores, err := c.NeedsReview(ctx, task, other)
	if err != nil {
		return false, nil, "needs-review: " + err.Error()
	}
	byPath := make(map[string]float64, len(other))
	why := ""
	for i, f := range other {
		byPath[f.Path] = scores[i]
		if scores[i] >= defaultNeedsReviewCutoff && why == "" {
			why = fmt.Sprintf("%s changes behaviour (%.2f)", f.Path, scores[i])
		}
	}
	return why == "", byPath, why
}
