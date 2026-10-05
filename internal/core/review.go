package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/jev"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/review"
	"github.com/ductone/orrey/internal/shadow"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

// reviewPlanTimeout bounds the classifier calls that plan a review.
const reviewPlanTimeout = 30 * time.Second

var reviewResultSchema = map[string]any{"type": "object", "properties": map[string]any{"pass": map[string]any{"type": "boolean"}, "findings": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "required": []string{"pass", "findings"}}

// reviewWorkspace plans and runs the independent review of the workspace diff.
//
// The plan decides what reviewers read: code in full, assets, lockfiles, and
// generated files as a list, and everything else by classifier judgement. It
// sizes each reviewer's turn limit and tier from what remains, and splits large
// reviews into parallel shards. A shard without a verdict is accepted when the
// classifier finds it low risk, and otherwise reviewed once more by another
// model family. Findings the classifier is fairly sure are not correctness
// bugs become notes. It returns ErrReviewInconclusive when some part still has
// no verdict.
func (e *Engine) reviewWorkspace(ctx context.Context, sid, parent string, req agentproto.TaskRequest, emit EmitFunc) (bool, string, error) {
	diff, changed, err := e.reviewDiff(ctx, sid, req.Workspace.Path)
	if err != nil {
		return false, "", fmt.Errorf("collect diff: %w", err)
	}
	if len(diff) == 0 {
		return true, "no diff", nil
	}
	if changed != nil {
		e.emit(ctx, sid, "review.scope", map[string]any{"changed": changed}, emit)
	}
	classifier := e.reviewClassifier()
	task := ""
	if s, err := e.store.Session(ctx, sid); err == nil {
		task = s.Spec
	}
	planCtx, cancel := context.WithTimeout(ctx, reviewPlanTimeout)
	plan := review.Build(planCtx, review.ParseDiff(diff), task, classifier, review.Options{})
	cancel()
	e.emit(ctx, sid, "review.plan", planEvent(plan), emit)
	riskRecord := e.recordReviewRisk(ctx, sid, plan)
	if plan.Skip {
		e.shadowUpdate(riskRecord, e.store.SetShadowOutcome, map[string]any{"skipped": true})
		return true, store.JSON(map[string]any{"pass": true, "findings": []string{}, "skipped": plan.SkipReason}), nil
	}

	shards := make([]int, len(plan.Shards))
	for i := range shards {
		shards[i] = i
	}
	verdicts, families := e.runReviewShards(ctx, sid, parent, req, plan, shards, spawnOptions{workerTurns: plan.Turns}, emit)

	// A shard that returned no verdict is reviewed once more, by another
	// model family and with more room.
	var retry []int
	var retryFamilies []string
	for i, v := range verdicts {
		if v.Conclusive {
			continue
		}
		e.emit(ctx, sid, "review.inconclusive", map[string]any{"shard": i, "error": v.Error}, emit)
		retry = append(retry, i)
		if families[i] != "" {
			retryFamilies = append(retryFamilies, families[i])
		}
	}
	if len(retry) > 0 {
		opts := spawnOptions{workerTurns: plan.Turns + max(2, plan.Turns/2), excludeFamilies: retryFamilies}
		again, _ := e.runReviewShards(ctx, sid, parent, req, plan, retry, opts, emit)
		for j, i := range retry {
			verdicts[i] = again[j]
		}
	}

	out := review.Merge(plan, verdicts)
	if !out.Pass && !out.Inconclusive && classifier != nil {
		filtered, scores, err := review.FilterFindings(ctx, classifier, task, plan, out)
		e.emit(ctx, sid, "review.findings", map[string]any{"findings": out.Findings, "scores": scores, "kept": filtered.Findings, "classifier_error": errString(err)}, emit)
		out = filtered
	}
	e.emit(ctx, sid, "review.outcome", map[string]any{"pass": out.Pass, "inconclusive": out.Inconclusive, "findings": out.Findings, "notes": out.Notes, "verdicts": out.Verdicts}, emit)
	e.shadowUpdate(riskRecord, e.store.SetShadowOutcome, reviewRiskOutcome(out))
	if out.Inconclusive {
		var reasons []string
		for _, v := range out.Verdicts {
			if !v.Conclusive && v.Accepted == "" {
				reasons = append(reasons, fmt.Sprintf("part %d: %s", v.Shard+1, v.Error))
			}
		}
		return false, "", fmt.Errorf("%w: %s", ErrReviewInconclusive, strings.Join(reasons, "; "))
	}
	return out.Pass, store.JSON(map[string]any{"pass": out.Pass, "findings": out.Findings, "notes": out.Notes}), nil
}

func (e *Engine) reviewClassifier() review.Classifier {
	cfg, _, _, _, _ := e.runtimeSnapshot()
	cfg.Jev = cfg.EffectiveJev()
	if !cfg.Jev.Review || cfg.Jev.APIKey == "" {
		return nil
	}
	return review.JevClassifier{Client: jev.New(cfg.Jev.APIKey, cfg.Jev.BaseURL, cfg.Jev.Model, cfg.Jev.Timeout())}
}

// runReviewShards starts one reviewer per shard and waits for all of them. It
// returns each shard's verdict and the model family that reviewed it.
func (e *Engine) runReviewShards(ctx context.Context, sid, parent string, req agentproto.TaskRequest, plan review.Plan, shards []int, opts spawnOptions, emit EmitFunc) ([]review.Verdict, []string) {
	verdicts := make([]review.Verdict, len(shards))
	families := make([]string, len(shards))
	ids := make([]string, len(shards))
	for j, i := range shards {
		verdicts[j] = review.Verdict{Shard: i}
		job, err := e.spawnWith(ctx, sid, parent, req, map[string]any{
			"spec":            plan.Spec(i),
			"result_schema":   reviewResultSchema,
			"budget_fraction": 0.10,
			"workspace_mode":  "read",
			"review":          true,
			"phase":           "review",
		}, opts, emit)
		if err != nil {
			verdicts[j].Error = "spawn review worker: " + err.Error()
			continue
		}
		ids[j] = fmt.Sprint(job.(map[string]any)["id"])
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		pending := 0
		for j, id := range ids {
			if id == "" {
				continue
			}
			job, err := e.store.Job(ctx, id)
			if err != nil || job.Status == "running" {
				pending++
				continue
			}
			verdicts[j] = verdictFromJob(job, shards[j])
			if m, ok := model.Get(job.Model); ok {
				families[j] = string(m.Family)
			}
			ids[j] = ""
		}
		if pending == 0 {
			return verdicts, families
		}
		select {
		case <-ctx.Done():
			for j, id := range ids {
				if id != "" {
					verdicts[j].Error = "review context cancelled: " + ctx.Err().Error()
				}
			}
			return verdicts, families
		case <-ticker.C:
		}
	}
}

func verdictFromJob(j store.Job, shard int) review.Verdict {
	v := review.Verdict{Shard: shard}
	passed, text, err := classifyReviewJob(j)
	if err != nil {
		v.Error = strings.TrimPrefix(err.Error(), ErrReviewInconclusive.Error()+": ")
		if errors.Is(err, ErrReviewInconclusive) {
			return v
		}
	}
	v.Conclusive, v.Pass = true, passed
	var result struct {
		Findings []any `json:"findings"`
	}
	if json.Unmarshal([]byte(text), &result) == nil {
		for _, f := range result.Findings {
			v.Findings = append(v.Findings, fmt.Sprint(f))
		}
	}
	return v
}

// planEvent is the review plan as an event: decisions without patches.
func planEvent(p review.Plan) map[string]any {
	decisions := make([]map[string]any, 0, len(p.Decisions))
	for _, d := range p.Decisions {
		entry := map[string]any{"path": d.File.Path, "class": d.File.Class, "status": d.File.Status, "added": d.File.Added, "removed": d.File.Removed, "included": d.Included, "reason": d.Reason}
		if d.Score != nil {
			entry["needs_review"] = *d.Score
		}
		decisions = append(decisions, entry)
	}
	shards := make([]map[string]any, 0, len(p.Shards))
	for _, s := range p.Shards {
		files := make([]string, 0, len(s.Files))
		for _, f := range s.Files {
			files = append(files, f.Path)
		}
		shards = append(shards, map[string]any{"files": files, "chars": s.Chars, "truncated": s.Truncated})
	}
	return map[string]any{"decisions": decisions, "shards": shards, "skip": p.Skip, "skip_reason": p.SkipReason, "bug": p.Bug, "risk": p.Risk, "turns": p.Turns, "classifier_error": p.ClassifierError, "question_version": review.QuestionVersion}
}

// recordReviewRisk stores the plan's risk score as a review_risk observation
// when review calibration is being recorded, so the shadow report can compare
// it with the review's outcome. It makes no classifier call of its own.
func (e *Engine) recordReviewRisk(ctx context.Context, sid string, p review.Plan) string {
	cfg, _, _, _, _ := e.runtimeSnapshot()
	if !cfg.EffectiveJev().Shadows("review") || p.Bug == nil {
		return ""
	}
	files := make([]string, 0, len(p.Decisions))
	for _, d := range p.Decisions {
		if d.Included {
			files = append(files, d.File.Summary())
		}
	}
	id := uuid.NewString()
	obs := store.ShadowObservation{ID: id, SessionID: sid, TurnID: e.currentTurnID(ctx, sid), Site: shadow.ReviewRisk, QuestionVersion: review.QuestionVersion, Questions: "live review plan risk", State: map[string]any{"files": files}}
	if e.store.CreateShadow(context.WithoutCancel(ctx), obs) != nil {
		return ""
	}
	answers := map[string]jev.Answer{"introduces_bug": {Type: "noul", Noul: p.Bug}}
	_ = e.store.CompleteShadow(context.Background(), id, "", answers, nil, 0, nil)
	return id
}

func reviewRiskOutcome(out review.Outcome) map[string]any {
	if out.Inconclusive {
		return map[string]any{"inconclusive": true}
	}
	return map[string]any{"pass": out.Pass}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// reviewDiffHash identifies the diff a review would cover, or "" when there
// is none or it cannot be read.
func (e *Engine) reviewDiffHash(ctx context.Context, sid, root string) string {
	diff, _, err := e.reviewDiff(ctx, sid, root)
	if err != nil || len(diff) == 0 {
		return ""
	}
	sum := sha256.Sum256(diff)
	return hex.EncodeToString(sum[:])
}
