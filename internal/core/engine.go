package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/mcp"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
	"github.com/ductone/orrey/internal/store"
	"github.com/ductone/orrey/internal/webtools"
)

type EmitFunc func(agentproto.AgentEvent)

type Engine struct {
	cfg       config.Config
	store     *store.Store
	providers *provider.Registry
	policy    router.Policy
	mcp       *mcp.Manager
	web       *webtools.Client
	mu        sync.Mutex
	cancels   map[string]context.CancelFunc
}

func New(cfg config.Config, s *store.Store, p *provider.Registry, mc *mcp.Manager) *Engine {
	return &Engine{cfg: cfg, store: s, providers: p, policy: router.NewV1(cfg.Router, s), mcp: mc, web: webtools.New(cfg.WebSearch.APIKey), cancels: map[string]context.CancelFunc{}}
}
func (e *Engine) Store() *store.Store { return e.store }

func (e *Engine) Start(ctx context.Context, req agentproto.TaskRequest, emit EmitFunc) (string, <-chan agentproto.TaskResult, error) {
	if req.Spec == "" {
		return "", nil, errors.New("task spec required")
	}
	applyBudgetDefaults(&req, e.cfg)
	id := uuid.NewString()
	if err := e.store.CreateSession(ctx, store.Session{ID: id, Spec: req.Spec, Phase: "plan", BudgetUSD: req.Budget.MaxUSD}); err != nil {
		return "", nil, err
	}
	return id, e.startExisting(ctx, id, req, emit), nil
}

func (e *Engine) startExisting(parent context.Context, id string, req agentproto.TaskRequest, emit EmitFunc) <-chan agentproto.TaskResult {
	out := make(chan agentproto.TaskResult, 1)
	ctx, cancel := context.WithTimeout(parent, req.Budget.MaxWallClock)
	e.mu.Lock()
	e.cancels[id] = cancel
	e.mu.Unlock()
	go func() {
		defer close(out)
		defer cancel()
		out <- e.run(ctx, id, "", req, emit)
		e.mu.Lock()
		delete(e.cancels, id)
		e.mu.Unlock()
	}()
	return out
}

func (e *Engine) Run(ctx context.Context, req agentproto.TaskRequest, emit EmitFunc) (agentproto.TaskResult, error) {
	_, ch, err := e.Start(ctx, req, emit)
	if err != nil {
		return agentproto.TaskResult{}, err
	}
	return <-ch, nil
}

func (e *Engine) Continue(ctx context.Context, id, instruction string, emit EmitFunc) (<-chan agentproto.TaskResult, error) {
	s, err := e.store.Session(ctx, id)
	if err != nil {
		return nil, err
	}
	req := agentproto.TaskRequest{Spec: s.Spec, Budget: agentproto.Budget{MaxUSD: s.BudgetUSD - s.SpentUSD, MaxTokens: 1_000_000, MaxWallClock: 2 * time.Hour, MaxDepth: 4}, Workspace: agentproto.Workspace{Path: e.cfg.WorkspaceRoot, Isolation: "shared"}, Depth: 4}
	if err := e.store.AddMessage(ctx, id, "user", provider.Message{Role: "user", Content: instruction}); err != nil {
		return nil, err
	}
	return e.startExisting(ctx, id, req, emit), nil
}

func (e *Engine) Cancel(id string) bool {
	e.mu.Lock()
	cancel := e.cancels[id]
	e.mu.Unlock()
	if cancel != nil {
		cancel()
		return true
	}
	return false
}

func (e *Engine) emit(ctx context.Context, sid, typ string, data any, emit EmitFunc) {
	_, _ = e.store.AddEvent(ctx, sid, typ, data)
	if emit != nil {
		emit(agentproto.AgentEvent{Type: typ, Data: data})
	}
}

func (e *Engine) run(ctx context.Context, sid, parentJob string, req agentproto.TaskRequest, emit EmitFunc) agentproto.TaskResult {
	ctx, span := otel.Tracer("orrery/core").Start(ctx, "session")
	defer span.End()
	span.SetAttributes(attribute.String("session.id", sid))
	started := time.Now()
	outcome := agentproto.Outcome{}
	stall := router.StallSignals{}
	emptyCompletions := 0
	e.emit(ctx, sid, "session.started", map[string]any{"spec": req.Spec}, emit)
	for {
		if err := ctx.Err(); err != nil {
			return e.finish(sid, agentproto.TaskResult{Status: agentproto.Cancelled, Outcome: outcome, Error: err.Error()}, emit)
		}
		s, err := e.store.Session(ctx, sid)
		if err != nil {
			return agentproto.TaskResult{Status: agentproto.Fail, Error: err.Error()}
		}
		reserved, _ := e.store.ReservedJobUSD(ctx, sid)
		if s.SpentUSD+reserved >= s.BudgetUSD || outcome.Tokens >= req.Budget.MaxTokens {
			return e.finish(sid, agentproto.TaskResult{Status: agentproto.BudgetExhausted, Outcome: outcome, Error: "budget exhausted"}, emit)
		}
		stored, _ := e.store.Messages(ctx, sid)
		inputTokens := estimate(s.Spec + s.DurableSummary + messagesText(stored))
		point := router.TurnStart
		if stall.FailedCommands >= 2 || stall.TestFailStreak >= 2 || stall.RepeatedEdits >= 3 {
			point = router.Escalation
		}
		newInstruction := len(stored) > 0 && stored[len(stored)-1].Role == "user"
		state := router.RoutingState{SessionID: sid, Turn: s.Turn + 1, Point: point, Phase: router.Phase(s.Phase), CurrentModel: s.Model, InputTokens: inputTokens, EstimatedOutput: 4000, ToolContinuation: len(stored) > 0 && stored[len(stored)-1].Role == "tool", NewInstruction: newInstruction, Stall: stall, AvailableModels: e.providers.AvailableIDs()}
		if newInstruction {
			state.Phase = router.Plan
		}
		applyHints(&state, req.Hints)
		decision, why, err := e.policy.Decide(ctx, state)
		if err != nil {
			return e.finish(sid, agentproto.TaskResult{Status: agentproto.Fail, Outcome: outcome, Error: err.Error()}, emit)
		}
		e.emit(ctx, sid, "routing.decision", map[string]any{"decision": decision, "explanation": why}, emit)
		reg := e.toolRegistry(sid, parentJob, req, emit)
		build := func(m model.ModelSpec, d router.Decision) (provider.Request, error) {
			history, err := e.providerMessages(ctx, sid)
			if err != nil {
				return provider.Request{}, err
			}
			return provider.Request{System: systemPrompt(d, req.Depth), DurableSpec: "TASK\n" + s.Spec + "\n\nDURABLE SUMMARY\n" + s.DurableSummary, Plan: "The live todo is carried in tool-result history; its phase-boundary snapshot is in the durable summary.", CacheKey: sid + ":" + m.ID, Messages: history, Tools: reg.Definitions(), MaxOutput: min(8000, m.MaxOutput), Effort: d.Effort, Strict: d.ToolsetVariant == "strict"}, nil
		}
		var resp provider.Response
		failed := []string{}
		for {
			resp, err = e.providers.CompleteOne(ctx, decision, build)
			if err == nil {
				break
			}
			e.emit(ctx, sid, "provider.error", map[string]any{"model": decision.Model.ID, "error": err.Error()}, emit)
			if !provider.IsRetryable(err) {
				return e.finish(sid, agentproto.TaskResult{Status: agentproto.Fail, Outcome: outcome, Error: err.Error()}, emit)
			}
			failed = append(failed, decision.Model.ID)
			state.ExcludeModels = failed
			state.CurrentModel = decision.Model.ID
			decision, why, err = e.policy.Decide(ctx, state)
			if err != nil {
				return e.finish(sid, agentproto.TaskResult{Status: agentproto.Fail, Outcome: outcome, Error: err.Error()}, emit)
			}
			e.emit(ctx, sid, "routing.fallback", map[string]any{"decision": decision, "explanation": why}, emit)
		}
		cost := decision.Model.Pricing.EstimateDetailed(resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.CacheReadTokens, resp.Usage.CacheWriteTokens)
		outcome.Tokens += resp.Usage.InputTokens + resp.Usage.OutputTokens
		outcome.CostUSD += cost
		outcome.Latency += resp.Latency
		s.Turn++
		s.Model = decision.Model.ID
		s.SpentUSD += cost
		_ = e.store.UpdateSession(ctx, s)
		_ = e.store.AddSpend(ctx, sid, cost)
		ttl := 5 * time.Minute
		if decision.Model.Family == model.Anthropic {
			ttl = time.Hour
		}
		_ = e.store.WarmCache(ctx, sid, decision.Model.ID, max(inputTokens, resp.Usage.CacheReadTokens+resp.Usage.CacheWriteTokens), ttl)
		_ = e.store.AddMessage(ctx, sid, "assistant", resp.Message)
		e.emit(ctx, sid, "assistant.message", map[string]any{"message": resp.Message, "usage": resp.Usage, "cost_usd": cost, "model": decision.Model.ID}, emit)
		if len(resp.Message.ToolCalls) == 0 {
			if emptyFinalResponse(resp.Message) {
				emptyCompletions++
				e.emit(ctx, sid, "completion.rejected", map[string]any{"reason": "empty assistant response", "attempt": emptyCompletions}, emit)
				if emptyCompletions >= 3 {
					return e.finish(sid, agentproto.TaskResult{Status: agentproto.Fail, Outcome: outcome, Error: "model returned three empty final responses"}, emit)
				}
				_ = e.store.AddMessage(ctx, sid, "user", provider.Message{Role: "user", Content: "Your last response was empty and cannot complete the task. Continue working, or provide a non-empty final result only after the task is actually complete."})
				stall.HumanInterrupt = true
				continue
			}
			result := parseResult(resp.Message.Content)
			if err := validateSchema(req.ResultSchema, result); err != nil {
				return e.finish(sid, agentproto.TaskResult{Status: agentproto.Fail, Outcome: outcome, Error: "result schema: " + err.Error()}, emit)
			}
			outcome.Latency = time.Since(started)
			return e.finish(sid, agentproto.TaskResult{Status: agentproto.Pass, Result: result, Outcome: outcome}, emit)
		}
		emptyCompletions = 0
		for _, call := range resp.Message.ToolCalls {
			outcome.ToolCalls++
			e.emit(ctx, sid, "tool.started", call, emit)
			value, callErr := reg.Call(ctx, call.Name, call.Arguments)
			if callErr != nil {
				outcome.ToolErrors++
				stall.ToolErrorRate = float64(outcome.ToolErrors) / float64(outcome.ToolCalls)
				if call.Name == "exec" {
					stall.FailedCommands++
					if strings.Contains(strings.ToLower(fmt.Sprint(call.Arguments["command"])), "test") {
						stall.TestFailStreak++
					}
				}
				if call.Name == "edit" {
					stall.RepeatedEdits++
					outcome.EditRetries++
				}
				value = map[string]any{"error": callErr.Error()}
			} else {
				if call.Name == "exec" {
					stall.FailedCommands = 0
					stall.TestFailStreak = 0
				}
				if call.Name == "edit" {
					stall.RepeatedEdits = 0
				}
			}
			toolMsg := provider.Message{Role: "tool", ToolCallID: call.ID, Content: store.JSON(value)}
			_ = e.store.AddMessage(ctx, sid, "tool", toolMsg)
			e.emit(ctx, sid, "tool.finished", map[string]any{"call": call, "result": value}, emit)
		}
		current, _ := e.store.Session(ctx, sid)
		if current.Phase != s.Phase || inputTokens > decision.Model.ContextWindow*3/4 {
			e.compact(ctx, sid, emit)
		}
	}
}

func (e *Engine) finish(sid string, result agentproto.TaskResult, emit EmitFunc) agentproto.TaskResult {
	if s, err := e.store.Session(context.Background(), sid); err == nil {
		if s.SpentUSD > result.Outcome.CostUSD {
			result.Outcome.CostUSD = s.SpentUSD
		}
		s.Status = string(result.Status)
		_ = e.store.UpdateSession(context.Background(), s)
	}
	e.emit(context.Background(), sid, "session.terminal", result, emit)
	if emit != nil {
		emit(agentproto.AgentEvent{Type: "terminal", Data: result, Terminal: &result})
	}
	return result
}

func (e *Engine) providerMessages(ctx context.Context, sid string) ([]provider.Message, error) {
	stored, err := e.store.Messages(ctx, sid)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Message, 0, len(stored))
	for _, m := range stored {
		var p provider.Message
		if err = json.Unmarshal([]byte(m.ContentJSON), &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		out = append(out, provider.Message{Role: "user", Content: "Begin the task. Establish a todo plan, then execute it to completion."})
	}
	return out, nil
}
func applyBudgetDefaults(req *agentproto.TaskRequest, cfg config.Config) {
	if req.Budget.MaxUSD <= 0 {
		req.Budget.MaxUSD = cfg.Budget.SessionUSD
	}
	if req.Budget.MaxTokens <= 0 {
		req.Budget.MaxTokens = 1_000_000
	}
	if req.Budget.MaxWallClock <= 0 {
		req.Budget.MaxWallClock = 2 * time.Hour
	}
	if req.Budget.MaxDepth == 0 {
		req.Budget.MaxDepth = 4
	}
	if req.Depth == 0 {
		req.Depth = req.Budget.MaxDepth
	}
	if req.Workspace.Path == "" {
		req.Workspace.Path = cfg.WorkspaceRoot
	}
}
func applyHints(s *router.RoutingState, h agentproto.RoutingHints) {
	s.TierPin = model.Tier(h.TierPin)
	for _, f := range h.FamilyExcludes {
		s.ExcludeFamilies = append(s.ExcludeFamilies, model.Family(f))
	}
	if h.Review {
		s.Point = router.ReviewCreation
		s.Phase = router.Review
		s.ImplementerFamily = model.Family(h.ImplementerFamily)
	}
}
