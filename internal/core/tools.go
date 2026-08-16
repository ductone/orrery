package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/lsp"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
	"github.com/ductone/orrey/internal/store"
	builtin "github.com/ductone/orrey/internal/tools"
	"github.com/google/uuid"
)

func (e *Engine) toolRegistry(sid, parentJob string, req agentproto.TaskRequest, discovery *instructionDiscovery, emit EmitFunc) *builtin.Registry {
	runtimeCfg, _, _, runtimeMCP, runtimeWeb := e.runtimeSnapshot()
	root := req.Workspace.Path
	if root == "" {
		root = runtimeCfg.WorkspaceRoot
	}
	r := builtin.New(root)
	if req.Workspace.Mode == "read" {
		r = builtin.NewReadOnly(root)
	}
	r.Add("ask", "Pause safely and request information that is genuinely required to continue. Do not use for permission or questions answerable from the workspace.", obj(map[string]any{"question": str(), "choices": map[string]any{"type": "array", "items": str()}, "allow_freeform": map[string]any{"type": "boolean"}}, "question"), func(ctx context.Context, a map[string]any) (any, error) {
		var choices []string
		if raw, ok := a["choices"].([]any); ok {
			for _, choice := range raw {
				choices = append(choices, fmt.Sprint(choice))
			}
		}
		allow, present := a["allow_freeform"].(bool)
		if !present {
			allow = len(choices) == 0
		}
		input := agentproto.InputRequest{ID: uuid.NewString(), Question: strings.TrimSpace(fmt.Sprint(a["question"])), Choices: choices, AllowFreeform: allow}
		if input.Question == "" {
			return nil, errors.New("question required")
		}
		if err := e.store.CreatePendingInput(ctx, store.PendingInput{ID: input.ID, SessionID: sid, Question: input.Question, Choices: input.Choices, AllowFreeform: input.AllowFreeform}); err != nil {
			return nil, err
		}
		return input, nil
	})
	if len(req.Attachments) > 0 {
		attachments := make(map[string]string, len(req.Attachments))
		for _, attachment := range req.Attachments {
			attachments[attachment.ID] = attachment.Path
		}
		r.AddFileScheme("attachment", attachments)
	}
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
			if err := e.mcpBoundary(ctx); err != nil {
				e.emit(ctx, sid, "runtime_config.reload_failed", map[string]any{"error": err.Error()}, emit)
			}
		}
		e.emit(ctx, sid, "todo.changed", ts, emit)
		return ts, nil
	})
	r.Add("artifact", "Register a task artifact for the parent harness and web UI.", obj(map[string]any{"path": str(), "description": str()}, "path"), func(ctx context.Context, a map[string]any) (any, error) {
		path := fmt.Sprint(a["path"])
		if path == "" {
			return nil, errors.New("artifact path required")
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		clean := filepath.Clean(path)
		rel, err := filepath.Rel(filepath.Clean(root), clean)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, errors.New("artifact path must be inside the assigned workspace")
		}
		x := map[string]any{"path": clean, "description": fmt.Sprint(a["description"])}
		e.emit(ctx, sid, "artifact.created", x, emit)
		return x, nil
	})
	r.Add("link", "Register a public task link such as a pull request, commit, issue, or deployment.", obj(map[string]any{"url": str(), "label": str(), "type": str()}, "url"), func(ctx context.Context, a map[string]any) (any, error) {
		raw := fmt.Sprint(a["url"])
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, errors.New("link must be an absolute HTTP(S) URL")
		}
		x := map[string]any{"url": raw, "label": fmt.Sprint(a["label"]), "type": fmt.Sprint(a["type"])}
		e.emit(ctx, sid, "link.created", x, emit)
		return x, nil
	})
	r.Add("spawn", "Create an in-process worker with isolated session state and a hard budget slice. Read workers run asynchronously. Shared-write workers run synchronously so the parent cannot mutate the checkout at the same time.", obj(map[string]any{"spec": str(), "result_schema": map[string]any{"type": "object"}, "budget_fraction": map[string]any{"type": "number"}, "review": map[string]any{"type": "boolean"}, "phase": map[string]any{"type": "string", "enum": []string{"explore", "plan", "implement", "diagnose", "review", "wrap-up"}}, "workspace_mode": map[string]any{"type": "string", "enum": []string{"read", "shared-write"}}}, "spec"), func(ctx context.Context, a map[string]any) (any, error) {
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
	r.Add("skill", "List workspace skills or progressively load one full SKILL.md. Load a skill before acting when the task names it or its catalog description clearly applies.", obj(map[string]any{"action": map[string]any{"type": "string", "enum": []string{"list", "load"}}, "name": str()}, "action"), func(_ context.Context, a map[string]any) (any, error) {
		switch fmt.Sprint(a["action"]) {
		case "list":
			return map[string]any{"skills": discovery.SkillCatalog()}, nil
		case "load":
			doc, loaded, err := discovery.LoadSkill(fmt.Sprint(a["name"]))
			if err != nil {
				return nil, err
			}
			if !loaded {
				return map[string]any{"already_loaded": true, "name": doc.Name, "path": doc.Path, "hint": "Use the skill instructions already present in context; do not load them again."}, nil
			}
			return map[string]any{"skill": instructionPayload([]workspaceInstruction{doc})[0], "hint": "Follow this SKILL.md for the current task. Read only the referenced resources needed to proceed."}, nil
		default:
			return nil, errors.New("action must be list or load")
		}
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
		return runtimeWeb.Search(ctx, fmt.Sprint(a["query"]), count)
	})
	r.Add("fetch", "Fetch a public HTTP(S) URL. Private and link-local addresses are rejected.", obj(map[string]any{"url": str()}, "url"), func(ctx context.Context, a map[string]any) (any, error) {
		return runtimeWeb.Fetch(ctx, fmt.Sprint(a["url"]))
	})
	if e.lsp != nil && e.lsp.Configured() {
		r.Add("lsp", "Query configured language servers for semantic navigation and diagnostics. line is 1-based and character is 0-based. This tool is read-only; use hashline edit for changes.", obj(map[string]any{"operation": map[string]any{"type": "string", "enum": []string{"definition", "references", "hover", "document_symbols", "workspace_symbols", "diagnostics"}}, "path": str(), "line": map[string]any{"type": "integer"}, "character": map[string]any{"type": "integer"}, "query": str(), "server": str()}, "operation"), func(ctx context.Context, a map[string]any) (any, error) {
			line := 0
			if n, ok := a["line"].(float64); ok && n > 0 {
				line = int(n) - 1
			}
			character := 0
			if n, ok := a["character"].(float64); ok && n > 0 {
				character = int(n)
			}
			return e.lsp.Call(ctx, root, lsp.Request{Operation: fmt.Sprint(a["operation"]), Path: fmt.Sprint(a["path"]), Line: line, Character: character, Query: fmt.Sprint(a["query"]), Server: fmt.Sprint(a["server"])})
		})
	}
	if runtimeMCP != nil {
		for _, d := range runtimeMCP.Definitions() {
			def := d
			r.Add(def.Name, def.Description, def.InputSchema, func(ctx context.Context, a map[string]any) (any, error) {
				return runtimeMCP.CallForSession(ctx, sid, def.Name, a)
			})
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
		cfg, _, _, _, _ := e.runtimeSnapshot()
		fraction = cfg.Budget.JobDefaultFraction
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
	phase := router.Implement
	if requested := router.Phase(fmt.Sprint(a["phase"])); requested != "" {
		phase = requested
	}
	if review {
		phase = router.Review
	}
	workspaceMode := fmt.Sprint(a["workspace_mode"])
	if workspaceMode == "" {
		workspaceMode = "shared-write"
		if phase == router.Explore || phase == router.Plan || phase == router.Review {
			workspaceMode = "read"
		}
	}
	if err := validateWorkspaceMode(workspaceMode); err != nil {
		return nil, err
	}
	if parentReq.Workspace.Mode == "read" && workspaceMode != "read" {
		return nil, errors.New("read workers cannot create shared-write descendants")
	}
	childUSD := min(parentReq.Budget.MaxUSD*fraction, available*fraction)
	child := agentproto.TaskRequest{Spec: spec, ResultSchema: schema, Budget: agentproto.Budget{MaxTokens: max(1000, int(float64(parentReq.Budget.MaxTokens)*fraction)), MaxUSD: childUSD, MaxWallClock: parentReq.Budget.MaxWallClock, MaxDepth: parentReq.Budget.MaxDepth}, Workspace: agentproto.Workspace{Path: parentReq.Workspace.Path, Mode: workspaceMode, Ownership: parentReq.Workspace.Ownership}, Depth: parentReq.Depth - 1}
	child.Hints.Review = review
	if review {
		if s, _ := e.store.Session(ctx, sid); s.Model != "" {
			if m, ok := model.Get(s.Model); ok {
				child.Hints.ImplementerFamily = string(m.Family)
			}
		}
	}
	point := router.JobCreation
	if review {
		point = router.ReviewCreation
	}
	_, runtimeProviders, runtimePolicy, _, _ := e.runtimeSnapshot()
	jobState := router.RoutingState{SessionID: sid, Turn: parentSession.Turn, Point: point, Phase: phase, InputTokens: estimate(spec), EstimatedOutput: 4000, AvailableModels: runtimeProviders.AvailableIDs(), ImplementerFamily: model.Family(child.Hints.ImplementerFamily)}
	if phase == router.Explore {
		child.Depth = 0
		child.Budget.MaxDepth = 0
		child.Budget.MaxTokens = min(child.Budget.MaxTokens, 150_000)
		child.Budget.MaxUSD = min(child.Budget.MaxUSD, 0.35)
	}
	jobDecision, jobWhy, err := runtimePolicy.Decide(ctx, jobState)
	if err != nil {
		return nil, err
	}
	child.Hints.TierPin = string(jobDecision.Model.Tier)
	j := store.Job{ID: id, SessionID: sid, ParentJobID: parent, Spec: spec, ResultSchemaJSON: store.JSON(schema), BudgetJSON: store.JSON(child.Budget), WorkspaceJSON: store.JSON(child.Workspace), HintsJSON: store.JSON(child.Hints), Depth: int(child.Depth), Model: jobDecision.Model.ID, Status: "running"}
	if err := e.store.CreateJob(ctx, j); err != nil {
		return nil, err
	}
	_ = os.WriteFile(filepath.Join(jobDir, "spec.json"), []byte(store.JSON(child)), 0600)
	e.emit(ctx, sid, "job.started", map[string]any{"id": id, "parent_session_id": sid, "parent_job_id": parent, "spec": spec, "model": jobDecision.Model.ID, "workspace_mode": workspaceMode, "explanation": jobWhy}, emit)
	runJob := func(runCtx context.Context) agentproto.TaskResult {
		childSID := uuid.NewString()
		if err := e.store.CreateSession(runCtx, store.Session{ID: childSID, Spec: spec, Phase: string(phase), Model: jobDecision.Model.ID, BudgetUSD: child.Budget.MaxUSD, WorkspacePath: child.Workspace.Path, WorkspaceOwnership: child.Workspace.Ownership, RequestJSON: store.JSON(child)}); err != nil {
			return agentproto.TaskResult{Status: agentproto.Fail, Error: "create worker session: " + err.Error()}
		}
		jobCtx, cancel := context.WithTimeout(runCtx, child.Budget.MaxWallClock)
		defer cancel()
		return e.run(jobCtx, childSID, id, child, nil)
	}
	finishJob := func(result agentproto.TaskResult, injectHandoff bool) {
		_ = e.store.FinishJob(context.Background(), id, string(result.Status), result.Result, result.Outcome)
		_ = e.store.AddSpend(context.Background(), sid, result.Outcome.CostUSD)
		_ = e.store.UpdateLatestJobRoutingOutcome(context.Background(), sid, result)
		_ = os.WriteFile(filepath.Join(jobDir, "result.json"), []byte(store.JSON(result)), 0600)
		_ = os.WriteFile(filepath.Join(jobDir, "status"), []byte(string(result.Status)+"\n"), 0600)
		if injectHandoff {
			_ = e.store.AddMessage(context.Background(), sid, "user", provider.Message{Role: "user", Content: "Worker job " + id + " completed. Treat this durable result as a worker handoff and advance the todo without repeating completed work: " + store.JSON(result)})
		}
		e.emit(context.Background(), sid, "job.terminal", map[string]any{"id": id, "parent_session_id": sid, "parent_job_id": parent, "result": result}, emit)
	}
	if workspaceMode == "shared-write" {
		result := runJob(ctx)
		finishJob(result, false)
		return map[string]any{"id": id, "status": result.Status, "result": result, "uri": "job://" + id + "/result"}, nil
	}
	go func() { finishJob(runJob(context.Background()), true) }()
	return map[string]any{"id": id, "status": "running", "uri": "job://" + id + "/result"}, nil
}

func (e *Engine) reviewWorkspace(ctx context.Context, sid, parent string, req agentproto.TaskRequest, emit EmitFunc) (bool, string, error) {
	diff, err := collectWorkspaceDiff(ctx, req.Workspace.Path)
	if err != nil {
		return false, "", fmt.Errorf("collect diff: %w", err)
	}
	if len(diff) == 0 {
		return true, "no diff", nil
	}
	job, err := e.spawn(ctx, sid, parent, req, map[string]any{
		"spec":            "Review this proposed workspace diff. Report only correctness bugs introduced by the patch. Return JSON with pass=true only if there are no correctness findings.\n\nDIFF\n" + string(diff),
		"result_schema":   map[string]any{"type": "object", "properties": map[string]any{"pass": map[string]any{"type": "boolean"}, "findings": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "required": []string{"pass", "findings"}},
		"budget_fraction": 0.10,
		"workspace_mode":  "read",
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
			if j.Status != string(agentproto.Pass) {
				return false, "", fmt.Errorf("review job %s ended with status %s", id, j.Status)
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

const maxReviewDiff = 120_000

func workspaceHasReviewableChanges(ctx context.Context, workspace string) (bool, error) {
	if !isGitWorkspace(ctx, workspace) {
		return false, nil
	}
	diff, err := collectWorkspaceDiff(ctx, workspace)
	return len(diff) > 0, err
}

func collectWorkspaceDiff(ctx context.Context, workspace string) ([]byte, error) {
	if workspace == "" {
		return nil, nil
	}
	if !isGitWorkspace(ctx, workspace) {
		return collectNonGitWorkspace(ctx, workspace)
	}
	diffCmd := exec.CommandContext(ctx, "git", "-C", workspace, "diff", "--no-ext-diff", "--unified=40")
	diff, err := diffCmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("tracked diff: %w: %s", err, diff)
	}
	if len(diff) >= maxReviewDiff {
		return diff[:maxReviewDiff], nil
	}
	untrackedCmd := exec.CommandContext(ctx, "git", "-C", workspace, "ls-files", "--others", "--exclude-standard", "-z")
	untracked, err := untrackedCmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list untracked files: %w: %s", err, untracked)
	}
	for _, rawPath := range bytes.Split(untracked, []byte{0}) {
		rel := filepath.ToSlash(string(rawPath))
		if rel == "" || rel == ".orrery" || strings.HasPrefix(rel, ".orrery/") {
			continue
		}
		diff, err = appendReviewFile(diff, workspace, rel)
		if err != nil {
			return nil, err
		}
		if len(diff) >= maxReviewDiff {
			return diff[:maxReviewDiff], nil
		}
	}
	return diff, nil
}

func isGitWorkspace(ctx context.Context, workspace string) bool {
	if workspace == "" {
		return false
	}
	cmd := exec.CommandContext(ctx, "git", "-C", workspace, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func collectNonGitWorkspace(ctx context.Context, workspace string) ([]byte, error) {
	var diff []byte
	err := filepath.WalkDir(workspace, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if path != workspace && ignoredInstructionDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(diff) >= maxReviewDiff {
			return fs.SkipAll
		}
		rel, err := filepath.Rel(workspace, path)
		if err != nil {
			return nil
		}
		diff, err = appendReviewFile(diff, workspace, filepath.ToSlash(rel))
		return err
	})
	if len(diff) > maxReviewDiff {
		diff = diff[:maxReviewDiff]
	}
	return diff, err
}

func appendReviewFile(diff []byte, workspace, rel string) ([]byte, error) {
	fullPath := filepath.Join(workspace, filepath.FromSlash(rel))
	info, err := os.Lstat(fullPath)
	if err != nil {
		return nil, fmt.Errorf("inspect review file %s: %w", rel, err)
	}
	var content []byte
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		content = []byte("[symlink target omitted]\n")
	case !info.Mode().IsRegular():
		return diff, nil
	default:
		file, openErr := os.Open(fullPath)
		if openErr != nil {
			return nil, fmt.Errorf("read review file %s: %w", rel, openErr)
		}
		content, err = io.ReadAll(io.LimitReader(file, maxReviewDiff+1))
		closeErr := file.Close()
		if err != nil {
			return nil, fmt.Errorf("read review file %s: %w", rel, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close review file %s: %w", rel, closeErr)
		}
	}
	if bytes.IndexByte(content, 0) >= 0 {
		content = []byte("[binary file omitted]\n")
	}
	header := []byte(fmt.Sprintf("\ndiff --git a/%s b/%s\nnew file mode 100644\n--- /dev/null\n+++ b/%s\n@@ new file @@\n", rel, rel, rel))
	diff = append(diff, header...)
	for _, line := range bytes.SplitAfter(content, []byte("\n")) {
		diff = append(diff, '+')
		diff = append(diff, line...)
		if len(diff) >= maxReviewDiff {
			return diff[:maxReviewDiff], nil
		}
	}
	return diff, nil
}

func (e *Engine) mcpBoundary(ctx context.Context) error {
	if e.boundary != nil {
		return e.boundary(ctx)
	}
	_, _, _, runtimeMCP, _ := e.runtimeSnapshot()
	if runtimeMCP != nil {
		return runtimeMCP.PhaseBoundary(ctx)
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
