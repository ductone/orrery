package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/router"
	"github.com/ductone/orrey/internal/store"
	builtin "github.com/ductone/orrey/internal/tools"
	"github.com/google/uuid"
)

func (e *Engine) toolRegistry(sid, parentJob string, req agentproto.TaskRequest, emit EmitFunc) *builtin.Registry {
	root := req.Workspace.Path
	if root == "" {
		root = e.cfg.WorkspaceRoot
	}
	r := builtin.New(root)
	r.Add("todo", "Replace the ordered todo plan. Phase is explore, plan, implement, diagnose, review, or wrap-up.", obj(map[string]any{"items": map[string]any{"type": "array", "items": obj(map[string]any{"text": str(), "phase": str(), "status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}}}, "text", "phase", "status")}}, "items"), func(ctx context.Context, a map[string]any) (any, error) {
		b, _ := json.Marshal(a["items"])
		var ts []store.Todo
		if err := json.Unmarshal(b, &ts); err != nil {
			return nil, err
		}
		old, _ := e.store.Session(ctx, sid)
		if err := e.store.SetTodos(ctx, sid, ts); err != nil {
			return nil, err
		}
		phase := phaseFrom(ts)
		if phase != old.Phase {
			old.Phase = phase
			old.DurableSummary = setPlanSnapshot(old.DurableSummary, store.JSON(ts))
			_ = e.store.UpdateSession(ctx, old)
			_ = e.mcpBoundary(ctx)
		}
		e.emit(ctx, sid, "todo.updated", ts, emit)
		return ts, nil
	})
	r.Add("spawn", "Create an in-process worker with isolated session state and a hard budget slice.", obj(map[string]any{"spec": str(), "result_schema": map[string]any{"type": "object"}, "budget_fraction": map[string]any{"type": "number"}, "review": map[string]any{"type": "boolean"}, "phase": map[string]any{"type": "string", "enum": []string{"explore", "plan", "implement", "diagnose", "review", "wrap-up"}}, "isolation": map[string]any{"type": "string", "enum": []string{"worktree", "copy", "shared-ro"}}}, "spec"), func(ctx context.Context, a map[string]any) (any, error) {
		if req.Depth == 0 {
			return nil, errors.New("spawn forbidden at depth 0")
		}
		return e.spawn(ctx, sid, parentJob, req, a, emit)
	})
	r.Add("job_result", "Read a durable child job result.", obj(map[string]any{"id": str()}, "id"), func(ctx context.Context, a map[string]any) (any, error) {
		j, err := e.store.Job(ctx, fmt.Sprint(a["id"]))
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": j.Status, "result": json.RawMessage(j.ResultJSON), "outcome": json.RawMessage(j.OutcomeJSON)}, nil
	})
	r.AddScheme("job", func(ctx context.Context, a map[string]any) (any, error) {
		parts := strings.SplitN(fmt.Sprint(a["path"]), "/", 2)
		j, err := e.store.Job(ctx, parts[0])
		if err != nil {
			return nil, err
		}
		if len(parts) == 2 && strings.HasPrefix(parts[1], "result.") {
			var fields map[string]any
			if json.Unmarshal([]byte(j.ResultJSON), &fields) == nil {
				return fields[strings.TrimPrefix(parts[1], "result.")], nil
			}
		}
		return map[string]any{"status": j.Status, "result": json.RawMessage(j.ResultJSON), "outcome": json.RawMessage(j.OutcomeJSON)}, nil
	})
	r.Add("web_search", "Search the public web using Brave Search.", obj(map[string]any{"query": str(), "count": map[string]any{"type": "integer"}}, "query"), func(ctx context.Context, a map[string]any) (any, error) {
		count := 8
		if x, ok := a["count"].(float64); ok {
			count = int(x)
		}
		return e.web.Search(ctx, fmt.Sprint(a["query"]), count)
	})
	r.Add("fetch", "Fetch a public HTTP(S) URL. Private and link-local addresses are rejected.", obj(map[string]any{"url": str()}, "url"), func(ctx context.Context, a map[string]any) (any, error) {
		return e.web.Fetch(ctx, fmt.Sprint(a["url"]))
	})
	if e.mcp != nil {
		for _, d := range e.mcp.Definitions() {
			def := d
			r.Add(def.Name, def.Description, def.InputSchema, func(ctx context.Context, a map[string]any) (any, error) { return e.mcp.Call(ctx, def.Name, a) })
		}
	}
	return r
}

func (e *Engine) spawn(ctx context.Context, sid, parent string, parentReq agentproto.TaskRequest, a map[string]any, emit EmitFunc) (any, error) {
	fraction := .0
	if x, ok := a["budget_fraction"].(float64); ok {
		fraction = x
	}
	if fraction <= 0 {
		fraction = e.cfg.Budget.JobDefaultFraction
	}
	if fraction > 1 {
		return nil, errors.New("budget fraction exceeds parent")
	}
	spec := fmt.Sprint(a["spec"])
	schema, _ := a["result_schema"].(map[string]any)
	review, _ := a["review"].(bool)
	id := uuid.NewString()
	jobDir := filepath.Join(parentReq.Workspace.Path, ".orrery", "jobs", id)
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		return nil, err
	}
	parentSession, _ := e.store.Session(ctx, sid)
	reserved, _ := e.store.ReservedJobUSD(ctx, sid)
	available := parentSession.BudgetUSD - parentSession.SpentUSD - reserved
	if available <= 0 {
		return nil, errors.New("no unreserved session budget remains")
	}
	isolation := fmt.Sprint(a["isolation"])
	if isolation == "" {
		isolation = "worktree"
	}
	workspacePath, actualIsolation, err := prepareWorkspace(ctx, parentReq.Workspace.Path, jobDir, isolation)
	if err != nil {
		return nil, err
	}
	childUSD := min(parentReq.Budget.MaxUSD*fraction, available*fraction)
	child := agentproto.TaskRequest{Spec: spec, ResultSchema: schema, Budget: agentproto.Budget{MaxTokens: max(1000, int(float64(parentReq.Budget.MaxTokens)*fraction)), MaxUSD: childUSD, MaxWallClock: parentReq.Budget.MaxWallClock, MaxDepth: parentReq.Budget.MaxDepth}, Workspace: agentproto.Workspace{Path: workspacePath, Isolation: actualIsolation}, Depth: parentReq.Depth - 1}
	child.Hints.Review = review
	if review {
		if s, _ := e.store.Session(ctx, sid); s.Model != "" {
			if m, ok := model.Get(s.Model); ok {
				child.Hints.ImplementerFamily = string(m.Family)
			}
		}
	}
	point := router.JobCreation
	phase := router.Implement
	if requested := router.Phase(fmt.Sprint(a["phase"])); requested != "" {
		phase = requested
	}
	if review {
		point = router.ReviewCreation
		phase = router.Review
	}
	jobState := router.RoutingState{SessionID: sid, Turn: parentSession.Turn, Point: point, Phase: phase, InputTokens: estimate(spec), EstimatedOutput: 4000, AvailableModels: e.providers.AvailableIDs(), ImplementerFamily: model.Family(child.Hints.ImplementerFamily)}
	jobDecision, jobWhy, err := e.policy.Decide(ctx, jobState)
	if err != nil {
		return nil, err
	}
	child.Hints.TierPin = string(jobDecision.Model.Tier)
	j := store.Job{ID: id, SessionID: sid, ParentJobID: parent, Spec: spec, ResultSchemaJSON: store.JSON(schema), BudgetJSON: store.JSON(child.Budget), WorkspaceJSON: store.JSON(child.Workspace), HintsJSON: store.JSON(child.Hints), Depth: int(child.Depth), Model: jobDecision.Model.ID, Status: "running"}
	if err := e.store.CreateJob(ctx, j); err != nil {
		return nil, err
	}
	_ = os.WriteFile(filepath.Join(jobDir, "spec.json"), []byte(store.JSON(child)), 0600)
	e.emit(ctx, sid, "job.started", map[string]any{"id": id, "spec": spec, "model": jobDecision.Model.ID, "explanation": jobWhy}, emit)
	go func() {
		childSID := uuid.NewString()
		_ = e.store.CreateSession(context.Background(), store.Session{ID: childSID, Spec: spec, Phase: string(phase), Model: jobDecision.Model.ID, BudgetUSD: child.Budget.MaxUSD})
		result := e.run(context.Background(), childSID, id, child, nil)
		_ = e.store.FinishJob(context.Background(), id, string(result.Status), result.Result, result.Outcome)
		_ = e.store.AddSpend(context.Background(), sid, result.Outcome.CostUSD)
		_ = e.store.UpdateLatestJobRoutingOutcome(context.Background(), sid, result)
		_ = os.WriteFile(filepath.Join(jobDir, "result.json"), []byte(store.JSON(result)), 0600)
		_ = os.WriteFile(filepath.Join(jobDir, "status"), []byte(string(result.Status)+"\n"), 0600)
		e.emit(context.Background(), sid, "job.terminal", map[string]any{"id": id, "result": result}, emit)
		cleanupWorkspace(parentReq.Workspace.Path, workspacePath, actualIsolation)
	}()
	return map[string]any{"id": id, "status": "running", "uri": "job://" + id + "/result"}, nil
}

func (e *Engine) reviewWorkspace(ctx context.Context, sid, parent string, req agentproto.TaskRequest, emit EmitFunc) (bool, string, error) {
	diffCmd := exec.CommandContext(ctx, "git", "-C", req.Workspace.Path, "diff", "--no-ext-diff", "--unified=40")
	diff, err := diffCmd.CombinedOutput()
	if err != nil {
		return false, "", fmt.Errorf("collect diff: %w: %s", err, diff)
	}
	if len(diff) == 0 {
		return true, "no diff", nil
	}
	if len(diff) > 120_000 {
		diff = diff[:120_000]
	}
	job, err := e.spawn(ctx, sid, parent, req, map[string]any{
		"spec":            "Review this proposed workspace diff. Report only correctness bugs introduced by the patch. Return JSON with pass=true only if there are no correctness findings.\n\nDIFF\n" + string(diff),
		"result_schema":   map[string]any{"type": "object", "properties": map[string]any{"pass": map[string]any{"type": "boolean"}, "findings": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "required": []string{"pass", "findings"}},
		"budget_fraction": 0.10,
		"isolation":       "shared-ro",
		"review":          true,
		"phase":           "review",
	}, emit)
	if err != nil {
		return false, "", err
	}
	id := fmt.Sprint(job.(map[string]any)["id"])
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-ticker.C:
			j, getErr := e.store.Job(ctx, id)
			if getErr != nil || j.Status == "running" {
				continue
			}
			var result map[string]any
			if json.Unmarshal([]byte(j.ResultJSON), &result) != nil {
				return false, j.ResultJSON, nil
			}
			passed, _ := result["pass"].(bool)
			return passed, store.JSON(result), nil
		}
	}
}

func prepareWorkspace(ctx context.Context, src, jobDir, mode string) (string, string, error) {
	dst := filepath.Join(jobDir, "workspace")
	switch mode {
	case "worktree", "shared-ro":
		cmd := exec.CommandContext(ctx, "git", "-C", src, "worktree", "add", "--detach", dst, "HEAD")
		if out, err := cmd.CombinedOutput(); err == nil {
			return dst, mode, nil
		} else if !strings.Contains(string(out), "not a git repository") {
			return "", "", fmt.Errorf("create worktree: %v: %s", err, out)
		}
		mode = "copy"
	case "copy":
	default:
		return "", "", fmt.Errorf("unknown isolation %q", mode)
	}
	if err := copyTree(src, dst); err != nil {
		return "", "", err
	}
	return dst, mode, nil
}

func cleanupWorkspace(parent, workspace, isolation string) {
	if isolation == "worktree" || isolation == "shared-ro" {
		cmd := exec.Command("git", "-C", parent, "worktree", "remove", "--force", workspace)
		if cmd.Run() == nil {
			return
		}
	}
	_ = os.RemoveAll(workspace)
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == ".git" || rel == ".orrery" || strings.HasPrefix(rel, ".git/") || strings.HasPrefix(rel, ".orrery/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "vendor", "local_vendor", "bazel-bin", "bazel-out", "bazel-testlogs", ".cache":
				return filepath.SkipDir
			}
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		info, err := d.Info()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func (e *Engine) mcpBoundary(ctx context.Context) error {
	if e.mcp != nil {
		return e.mcp.PhaseBoundary(ctx)
	}
	return nil
}
func phaseFrom(ts []store.Todo) string {
	for _, t := range ts {
		if t.Status != "completed" && t.Phase != "" {
			return t.Phase
		}
	}
	return string(router.WrapUp)
}
func obj(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}
func str() map[string]any { return map[string]any{"type": "string"} }
