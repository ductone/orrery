package review

import (
	"context"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
)

// Classifier answers the judgement calls in a review. Every method returns one
// probability per input, in input order. A nil Classifier, or an error from
// one, falls back to the conservative choice: review in full, keep findings,
// and re-run inconclusive reviews.
type Classifier interface {
	// NeedsReview scores whether each non-code file's change needs a
	// correctness review, as opposed to prose, data, or assets.
	NeedsReview(ctx context.Context, task string, files []File) ([]float64, error)
	// Risk scores the change as a whole: the probability it introduces a bug,
	// and its risk level in [0,1].
	Risk(ctx context.Context, task string, files []File) (bug, risk float64, err error)
	// FileBugs scores, per file, whether that file's change introduces a bug.
	FileBugs(ctx context.Context, task string, files []File) ([]float64, error)
	// Findings scores whether each reviewer finding is a real correctness bug
	// introduced by the patch, given the patch text it concerns.
	Findings(ctx context.Context, task string, findings []Finding) ([]float64, error)
}

// Finding is one reviewer finding with the patch text it most likely concerns.
type Finding struct {
	Text  string
	Patch string
}

// Options tune a plan. Zero values select the defaults.
type Options struct {
	// ShardChars is the patch budget of one reviewer.
	ShardChars int
	// MaxShards bounds how many reviewers one review may use.
	MaxShards int
	// NeedsReviewCutoff is the NeedsReview score at or above which an Other
	// file is reviewed in full.
	NeedsReviewCutoff float64
}

const (
	defaultShardChars        = 60_000
	defaultMaxShards         = 4
	defaultNeedsReviewCutoff = 0.35
	// Reviewer turn bounds. A reviewer's turn limit grows with its patch.
	baseTurns    = 6
	charsPerTurn = 15_000
	maxTurns     = 16
)

func (o Options) withDefaults() Options {
	if o.ShardChars <= 0 {
		o.ShardChars = defaultShardChars
	}
	if o.MaxShards <= 0 {
		o.MaxShards = defaultMaxShards
	}
	if o.NeedsReviewCutoff <= 0 {
		o.NeedsReviewCutoff = defaultNeedsReviewCutoff
	}
	return o
}

// Decision records why a file is or is not shown in full.
type Decision struct {
	File     File     `json:"file"`
	Included bool     `json:"included"`
	Reason   string   `json:"reason"`
	Score    *float64 `json:"needs_review,omitempty"`
}

// Shard is one reviewer's share of a review.
type Shard struct {
	Files []File `json:"files"`
	Chars int    `json:"chars"`
	// Truncated names files whose patch was cut to fit the shard.
	Truncated []string `json:"truncated,omitempty"`
}

// Plan is the review of one diff.
type Plan struct {
	Decisions []Decision `json:"decisions"`
	Shards    []Shard    `json:"shards"`
	// Skip is set when nothing needs review; SkipReason says why.
	Skip       bool   `json:"skip,omitempty"`
	SkipReason string `json:"skip_reason,omitempty"`
	// Bug and Risk are the classifier's view of the whole change, when known.
	Bug  *float64 `json:"bug,omitempty"`
	Risk *float64 `json:"risk,omitempty"`
	// Turns bounds each reviewer.
	Turns int `json:"turns"`
	// ClassifierError records a classifier failure the plan fell back from.
	ClassifierError string `json:"classifier_error,omitempty"`
}

// Included returns the files shown in full, in diff order.
func (p Plan) Included() []File {
	var out []File
	for _, d := range p.Decisions {
		if d.Included {
			out = append(out, d.File)
		}
	}
	return out
}

// Summarized returns the files listed without their patch.
func (p Plan) Summarized() []Decision {
	var out []Decision
	for _, d := range p.Decisions {
		if !d.Included {
			out = append(out, d)
		}
	}
	return out
}

// Build plans a review. Code is always shown in full; assets, lockfiles,
// generated, and binary files are summarised; everything else is shown in full
// when the classifier thinks it needs review, or when there is no classifier.
func Build(ctx context.Context, files []File, task string, c Classifier, opts Options) Plan {
	opts = opts.withDefaults()
	var plan Plan
	var other []int
	for i := range files {
		Classify(&files[i])
		f := files[i]
		d := Decision{File: f}
		switch f.Class {
		case Code:
			d.Included, d.Reason = true, "code is always reviewed"
		case Asset, Lockfile, Generated, BinaryC:
			d.Reason = string(f.Class) + " files are summarised"
		default:
			if f.Status == Deleted {
				d.Reason = "deleted non-code file"
			} else {
				d.Included, d.Reason = true, "reviewed without a classifier"
				other = append(other, len(plan.Decisions))
			}
		}
		plan.Decisions = append(plan.Decisions, d)
	}
	if c != nil && len(other) > 0 {
		batch := make([]File, len(other))
		for i, idx := range other {
			batch[i] = plan.Decisions[idx].File
		}
		scores, err := c.NeedsReview(ctx, task, batch)
		if err != nil {
			plan.ClassifierError = "needs-review: " + err.Error()
		} else {
			for i, idx := range other {
				score := scores[i]
				d := &plan.Decisions[idx]
				d.Score = &score
				d.Included = score >= opts.NeedsReviewCutoff
				if d.Included {
					d.Reason = fmt.Sprintf("classifier: needs review (%.2f)", score)
				} else {
					d.Reason = fmt.Sprintf("classifier: prose, data, or configuration without behavioural risk (%.2f)", score)
				}
			}
		}
	}
	included := plan.Included()
	if len(included) == 0 {
		plan.Skip = true
		plan.SkipReason = fmt.Sprintf("no reviewable changes: all %d changed files are summarised", len(files))
		return plan
	}
	plan.Shards, plan.Decisions = pack(included, plan.Decisions, opts)
	chars := 0
	for _, s := range plan.Shards {
		chars = max(chars, s.Chars)
	}
	if c != nil {
		bug, risk, err := c.Risk(ctx, task, plan.Included())
		if err != nil {
			if plan.ClassifierError == "" {
				plan.ClassifierError = "risk: " + err.Error()
			}
		} else {
			plan.Bug, plan.Risk = &bug, &risk
		}
	}
	// The risk scores are recorded for calibration but drive nothing: in
	// practice they predicted review outcomes worse than chance (low bug
	// probability on changes reviews then rejected), so they no longer
	// choose a cheaper reviewer, add turns, or accept inconclusive reviews.
	plan.Turns = min(maxTurns, baseTurns+chars/charsPerTurn)
	return plan
}

// pack groups included files into shards by top-level directory, so related
// changes stay together, and keeps within the shard budget. When the review
// would need more shards than allowed, the lowest-priority non-code files are
// summarised instead, and oversized patches are truncated rather than dropped.
func pack(included []File, decisions []Decision, opts Options) ([]Shard, []Decision) {
	groups := map[string][]File{}
	var order []string
	for _, f := range included {
		key := topDir(f.Path)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], f)
	}
	var shards []Shard
	add := func(f File) {
		patch := f.Patch
		truncated := false
		if len(patch) > opts.ShardChars {
			patch = patch[:opts.ShardChars] + "\n[patch truncated to fit the review; read the file for the rest]\n"
			truncated = true
		}
		f.Patch = patch
		if n := len(shards); n == 0 || shards[n-1].Chars+len(patch) > opts.ShardChars {
			shards = append(shards, Shard{})
		}
		s := &shards[len(shards)-1]
		s.Files = append(s.Files, f)
		s.Chars += len(patch)
		if truncated {
			s.Truncated = append(s.Truncated, f.Path)
		}
	}
	for _, key := range order {
		// Start a directory in a fresh shard when it will not fit the current one.
		size := 0
		for _, f := range groups[key] {
			size += min(len(f.Patch), opts.ShardChars)
		}
		if n := len(shards); n > 0 && shards[n-1].Chars+size > opts.ShardChars && size <= opts.ShardChars {
			shards = append(shards, Shard{})
		}
		for _, f := range groups[key] {
			add(f)
		}
	}
	shards = slices.DeleteFunc(shards, func(s Shard) bool { return len(s.Files) == 0 })
	if len(shards) <= opts.MaxShards {
		return shards, decisions
	}
	// Over budget: summarise non-code files, least likely to need review first,
	// and re-pack. Code is never dropped; if code alone exceeds the budget the
	// last shards are merged and truncated.
	type candidate struct {
		idx   int
		score float64
	}
	var cands []candidate
	for i, d := range decisions {
		if d.Included && d.File.Class != Code {
			score := 1.0
			if d.Score != nil {
				score = *d.Score
			}
			cands = append(cands, candidate{i, score})
		}
	}
	sort.SliceStable(cands, func(a, b int) bool { return cands[a].score < cands[b].score })
	if len(cands) > 0 {
		decisions[cands[0].idx].Included = false
		decisions[cands[0].idx].Reason = "summarised to keep the review within its reviewer budget"
		var still []File
		for _, d := range decisions {
			if d.Included {
				still = append(still, d.File)
			}
		}
		return pack(still, decisions, opts)
	}
	merged := shards[:opts.MaxShards]
	last := &merged[opts.MaxShards-1]
	for _, s := range shards[opts.MaxShards:] {
		for _, f := range s.Files {
			f.Patch = "[patch omitted: the review exceeds its reviewer budget; read the file directly]\n"
			last.Files = append(last.Files, f)
			last.Truncated = append(last.Truncated, f.Path)
		}
	}
	return merged, decisions
}

func topDir(p string) string {
	if i := strings.Index(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
}

// Spec renders one shard's reviewer instructions.
func (p Plan) Spec(shard int) string {
	s := p.Shards[shard]
	var b strings.Builder
	b.WriteString("Review this proposed workspace diff. Report only correctness bugs introduced by the patch. Return JSON with pass=true only if there are no correctness findings.\n")
	if len(p.Shards) > 1 {
		fmt.Fprintf(&b, "\nThis is part %d of %d of one review; other reviewers cover the other parts. Focus on the files below, and read other changed files only when a finding depends on them.\n", shard+1, len(p.Shards))
	}
	b.WriteString("\nFILES IN THIS REVIEW\n")
	for _, f := range s.Files {
		b.WriteString("- " + f.Summary() + "\n")
	}
	if summarized := p.Summarized(); len(summarized) > 0 {
		b.WriteString("\nALSO CHANGED, NOT SHOWN (read a file directly if a finding depends on it)\n")
		for _, d := range summarized {
			b.WriteString("- " + d.File.Summary() + ": " + d.Reason + "\n")
		}
	}
	if len(s.Truncated) > 0 {
		b.WriteString("\nTRUNCATED PATCHES (read these files for the full change): " + strings.Join(s.Truncated, ", ") + "\n")
	}
	b.WriteString("\nDIFF\n")
	for _, f := range s.Files {
		b.WriteString(f.Patch)
		if !strings.HasSuffix(f.Patch, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// Verdict is one reviewer's conclusion.
type Verdict struct {
	Shard      int      `json:"shard"`
	Conclusive bool     `json:"conclusive"`
	Pass       bool     `json:"pass"`
	Findings   []string `json:"findings,omitempty"`
	// Accepted marks an inconclusive shard accepted on classifier evidence.
	Accepted string `json:"accepted,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Outcome merges shard verdicts. The review passes only when every shard
// passed or was accepted; it is inconclusive when any shard has no verdict.
type Outcome struct {
	Pass         bool      `json:"pass"`
	Inconclusive bool      `json:"inconclusive"`
	Findings     []string  `json:"findings"`
	Notes        []string  `json:"notes,omitempty"`
	Verdicts     []Verdict `json:"verdicts"`
}

func Merge(plan Plan, verdicts []Verdict) Outcome {
	out := Outcome{Pass: true, Verdicts: verdicts, Findings: []string{}}
	for _, v := range verdicts {
		switch {
		case v.Accepted != "":
			out.Notes = append(out.Notes, v.Accepted)
		case !v.Conclusive:
			out.Inconclusive = true
			out.Pass = false
		case !v.Pass:
			out.Pass = false
			for _, f := range v.Findings {
				if len(plan.Shards) > 1 {
					f = fmt.Sprintf("[part %d] %s", v.Shard+1, f)
				}
				out.Findings = append(out.Findings, f)
			}
		}
	}
	if out.Inconclusive {
		out.Pass = false
	}
	return out
}

// FindingNoteCutoff is the score under which a finding is downgraded to a
// note: only when the classifier is fairly sure it is not a real bug.
const FindingNoteCutoff = 0.2

// FilterFindings splits a failing outcome's findings into those that stand and
// those downgraded to notes. A review whose findings are all downgraded
// passes. Without a classifier, or on error, every finding stands.
func FilterFindings(ctx context.Context, c Classifier, task string, plan Plan, out Outcome) (Outcome, []float64, error) {
	if c == nil || out.Pass || out.Inconclusive || len(out.Findings) == 0 {
		return out, nil, nil
	}
	findings := make([]Finding, len(out.Findings))
	for i, text := range out.Findings {
		findings[i] = Finding{Text: text, Patch: patchFor(text, plan)}
	}
	scores, err := c.Findings(ctx, task, findings)
	if err != nil {
		return out, nil, err
	}
	var kept []string
	for i, text := range out.Findings {
		if scores[i] < FindingNoteCutoff {
			out.Notes = append(out.Notes, fmt.Sprintf("downgraded finding (%.2f): %s", scores[i], text))
		} else {
			kept = append(kept, text)
		}
	}
	out.Findings = kept
	if len(kept) == 0 {
		out.Pass = true
		out.Findings = []string{}
	}
	return out, scores, nil
}

// patchFor finds the patch a finding refers to: the files it names, else the
// whole shard its label points at, else every included file, bounded.
func patchFor(finding string, plan Plan) string {
	const limit = 20_000
	var b strings.Builder
	for _, f := range plan.Included() {
		if strings.Contains(finding, f.Path) || strings.Contains(finding, path.Base(f.Path)) {
			b.WriteString(f.Patch)
		}
	}
	if b.Len() == 0 {
		for _, f := range plan.Included() {
			b.WriteString(f.Patch)
			if b.Len() >= limit {
				break
			}
		}
	}
	text := b.String()
	if len(text) > limit {
		text = text[:limit]
	}
	return text
}
