package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
			_ = e.store.UpdateSession(ctx, old)
			_ = e.mcpBoundary(ctx)
			e.compact(ctx, sid, emit)
		}
		e.emit(ctx, sid, "todo.updated", ts, emit)
		return ts, nil
	})
	r.Add("spawn", "Create an in-process worker with isolated session state and a hard budget slice.", obj(map[string]any{"spec": str(), "result_schema": map[string]any{"type": "object"}, "budget_fraction": map[string]any{"type": "number"}, "review": map[string]any{"type": "boolean"}}, "spec"), func(ctx context.Context, a map[string]any) (any, error) {
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
	child := agentproto.TaskRequest{Spec: spec, ResultSchema: schema, Budget: agentproto.Budget{MaxTokens: max(1000, int(float64(parentReq.Budget.MaxTokens)*fraction)), MaxUSD: parentReq.Budget.MaxUSD * fraction, MaxWallClock: parentReq.Budget.MaxWallClock, MaxDepth: parentReq.Budget.MaxDepth}, Workspace: parentReq.Workspace, Depth: parentReq.Depth - 1}
	child.Hints.Review = review
	if review {
		if s, _ := e.store.Session(ctx, sid); s.Model != "" {
			if m, ok := model.Get(s.Model); ok {
				child.Hints.ImplementerFamily = string(m.Family)
			}
		}
	}
	j := store.Job{ID: id, SessionID: sid, ParentJobID: parent, Spec: spec, ResultSchemaJSON: store.JSON(schema), BudgetJSON: store.JSON(child.Budget), WorkspaceJSON: store.JSON(child.Workspace), HintsJSON: store.JSON(child.Hints), Depth: int(child.Depth), Status: "running"}
	if err := e.store.CreateJob(ctx, j); err != nil {
		return nil, err
	}
	_ = os.WriteFile(filepath.Join(jobDir, "spec.json"), []byte(store.JSON(child)), 0600)
	e.emit(ctx, sid, "job.started", map[string]any{"id": id, "spec": spec}, emit)
	go func() {
		childSID := uuid.NewString()
		phase := "plan"
		if review {
			phase = "review"
		}
		_ = e.store.CreateSession(context.Background(), store.Session{ID: childSID, Spec: spec, Phase: phase, BudgetUSD: child.Budget.MaxUSD})
		result := e.run(context.Background(), childSID, id, child, nil)
		_ = e.store.FinishJob(context.Background(), id, string(result.Status), result.Result, result.Outcome)
		_ = os.WriteFile(filepath.Join(jobDir, "result.json"), []byte(store.JSON(result)), 0600)
		_ = os.WriteFile(filepath.Join(jobDir, "status"), []byte(string(result.Status)+"\n"), 0600)
		e.emit(context.Background(), sid, "job.terminal", map[string]any{"id": id, "result": result}, emit)
	}()
	return map[string]any{"id": id, "status": "running", "uri": "job://" + id + "/result"}, nil
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
