package web

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/core"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

//go:embed static/*
var assets embed.FS

type Server struct {
	engine     *core.Engine
	http       *http.Server
	version    string
	instanceID string
	startedAt  time.Time
	draining   atomic.Bool
}

func New(addr string, e *core.Engine, version ...string) *Server {
	return newServer(addr, e, false, version...)
}

// NewView exposes only static assets and read-only session/event endpoints.
// It is suitable for a separately registered collaborator-grade service; a
// query parameter is never used as the authorization boundary.
func NewView(addr string, e *core.Engine, version ...string) *Server {
	return newServer(addr, e, true, version...)
}

func newServer(addr string, e *core.Engine, viewOnly bool, version ...string) *Server {
	v := "dev"
	if len(version) > 0 && version[0] != "" {
		v = version[0]
	}
	s := &Server{engine: e, version: v, instanceID: uuid.NewString(), startedAt: time.Now().UTC()}
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /", http.FileServer(http.FS(static)))
	mux.HandleFunc("GET /api/sessions", s.sessions)
	mux.HandleFunc("GET /sessions/{id}/events", s.events)
	mux.HandleFunc("GET /sessions/{id}/jobs", s.jobs)
	mux.HandleFunc("GET /api/v1/healthz", s.health)
	mux.HandleFunc("GET /api/v1/readyz", s.ready)
	mux.HandleFunc("GET /api/v1/capabilities", s.capabilities)
	mux.HandleFunc("GET /api/v1/version", s.versionInfo)
	mux.HandleFunc("GET /api/v1/sessions", s.sessions)
	mux.HandleFunc("GET /api/v1/sessions/{id}", s.session)
	mux.HandleFunc("GET /api/v1/sessions/{id}/events", s.events)
	mux.HandleFunc("GET /api/v1/sessions/{id}/log", s.log)
	mux.HandleFunc("GET /api/v1/sessions/{id}/jobs", s.jobs)
	mux.HandleFunc("GET /api/v1/active-turns", s.activeTurns)
	if !viewOnly {
		mux.HandleFunc("POST /sessions", s.create)
		mux.HandleFunc("POST /sessions/{id}/messages", s.message)
		mux.HandleFunc("DELETE /sessions/{id}", s.cancel)
		mux.HandleFunc("POST /api/v1/sessions", s.createV1)
		mux.HandleFunc("POST /api/v1/sessions/{id}/messages", s.messageV1)
		mux.HandleFunc("POST /api/v1/sessions/{id}/cancel", s.cancelTurn)
		mux.HandleFunc("POST /api/v1/sessions/{id}/terminate", s.terminate)
		mux.HandleFunc("POST /api/v1/sessions/{id}/resume", s.resumeV1)
		mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.deleteSession)
		mux.HandleFunc("POST /api/v1/drain", s.drain)
	}
	s.http = &http.Server{Addr: addr, Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second}
	return s
}
func (s *Server) ListenAndServe() error              { return s.http.ListenAndServe() }
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }
func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	xs, err := s.engine.Store().Sessions(r.Context())
	write(w, xs, err)
}
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	write(w, map[string]any{"status": "ok", "instance_id": s.instanceID, "started_at": s.startedAt}, nil)
}
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		writeStatus(w, http.StatusServiceUnavailable, map[string]any{"status": "draining", "instance_id": s.instanceID}, nil)
		return
	}
	if err := s.engine.Store().DB().PingContext(r.Context()); err != nil {
		writeStatus(w, http.StatusServiceUnavailable, nil, err)
		return
	}
	write(w, map[string]any{"status": "ready", "instance_id": s.instanceID}, nil)
}
func (s *Server) capabilities(w http.ResponseWriter, _ *http.Request) {
	write(w, map[string]any{
		"api_version": 1, "event_schema": 1,
		"surfaces": []string{"web", "structured_chat"},
		"resume":   true, "attachments": false, "dynamic_model_routing": true,
		"runtime_config_reload": false, "idempotent_mutations": true,
		"external_workspaces": true, "last_event_id": true,
	}, nil)
}
func (s *Server) versionInfo(w http.ResponseWriter, _ *http.Request) {
	write(w, map[string]any{"version": s.version, "api_version": 1, "event_schema": 1, "instance_id": s.instanceID, "started_at": s.startedAt}, nil)
}
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	x, err := s.engine.Store().Session(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeStatus(w, http.StatusNotFound, nil, err)
		return
	}
	write(w, x, err)
}
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Prompt, Workspace string
		BudgetUSD         float64 `json:"budget_usd"`
	}
	if err := decode(r, &in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	req := agentproto.TaskRequest{Spec: in.Prompt, Budget: agentproto.Budget{MaxUSD: in.BudgetUSD, MaxWallClock: 2 * time.Hour, MaxDepth: 4}, Workspace: agentproto.Workspace{Path: in.Workspace, Isolation: "shared"}, Depth: 4}
	id, _, err := s.engine.Start(context.Background(), req, nil)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeStatus(w, http.StatusAccepted, map[string]any{"id": id}, nil)
}

type budgetInput struct {
	MaxTokens           int     `json:"max_tokens"`
	MaxUSD              float64 `json:"max_usd"`
	MaxWallclockSeconds int64   `json:"max_wallclock_seconds"`
	MaxDepth            uint32  `json:"max_depth"`
}
type workspaceInput struct {
	Path      string `json:"path"`
	Isolation string `json:"isolation"`
	Ownership string `json:"ownership"`
}
type createInput struct {
	Integration         string                  `json:"integration"`
	ExternalID          string                  `json:"external_id"`
	ExternalIncarnation string                  `json:"external_incarnation"`
	RequestID           string                  `json:"request_id"`
	Prompt              string                  `json:"prompt"`
	Workspace           workspaceInput          `json:"workspace"`
	Context             map[string]any          `json:"context"`
	Budget              budgetInput             `json:"budget"`
	Routing             agentproto.RoutingHints `json:"routing"`
}

func (s *Server) createV1(w http.ResponseWriter, r *http.Request) {
	if s.rejectWhileDraining(w) {
		return
	}
	var in createInput
	if err := decode(r, &in); err != nil {
		writeStatus(w, http.StatusBadRequest, nil, err)
		return
	}
	if in.Workspace.Ownership == "" {
		in.Workspace.Ownership = "external"
	}
	if in.Integration == "" {
		in.Integration = "squire"
	}
	req := agentproto.TaskRequest{
		Spec:      in.Prompt,
		Budget:    agentproto.Budget{MaxTokens: in.Budget.MaxTokens, MaxUSD: in.Budget.MaxUSD, MaxWallClock: time.Duration(in.Budget.MaxWallclockSeconds) * time.Second, MaxDepth: in.Budget.MaxDepth},
		Workspace: agentproto.Workspace{Path: in.Workspace.Path, Isolation: in.Workspace.Isolation, Ownership: in.Workspace.Ownership},
		Hints:     in.Routing, Depth: in.Budget.MaxDepth,
	}
	info, err := s.engine.StartIntegrated(context.Background(), req, core.SessionOptions{Integration: in.Integration, ExternalID: in.ExternalID, ExternalIncarnation: in.ExternalIncarnation, RequestID: in.RequestID, WorkspaceOwnership: in.Workspace.Ownership, Context: in.Context}, nil)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, nil, err)
		return
	}
	writeStatus(w, http.StatusAccepted, map[string]any{"id": info.SessionID, "turn_id": info.TurnID, "accepted": info.Accepted, "duplicate": info.Duplicate}, nil)
}
func (s *Server) message(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content string `json:"content"`
	}
	if err := decode(r, &in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	_, err := s.engine.Continue(context.Background(), r.PathValue("id"), in.Content, nil)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeStatus(w, http.StatusAccepted, map[string]bool{"accepted": true}, nil)
}

type messageInput struct {
	RequestID string `json:"request_id"`
	Content   string `json:"content"`
}

func (s *Server) messageV1(w http.ResponseWriter, r *http.Request) {
	if s.rejectWhileDraining(w) {
		return
	}
	var in messageInput
	if err := decode(r, &in); err != nil {
		writeStatus(w, http.StatusBadRequest, nil, err)
		return
	}
	info, err := s.engine.ContinueIntegrated(context.Background(), r.PathValue("id"), in.Content, in.RequestID, "squire", nil)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "active turn") {
			status = http.StatusConflict
		}
		writeStatus(w, status, nil, err)
		return
	}
	writeStatus(w, http.StatusAccepted, map[string]any{"id": info.SessionID, "turn_id": info.TurnID, "accepted": info.Accepted, "duplicate": info.Duplicate}, nil)
}
func (s *Server) resumeV1(w http.ResponseWriter, r *http.Request) { s.messageV1(w, r) }
func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	write(w, map[string]bool{"cancelled": s.engine.Cancel(r.PathValue("id"))}, nil)
}
func (s *Server) cancelTurn(w http.ResponseWriter, r *http.Request) {
	write(w, map[string]bool{"cancelled": s.engine.Cancel(r.PathValue("id"))}, nil)
}
func (s *Server) terminate(w http.ResponseWriter, r *http.Request) {
	err := s.engine.Terminate(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeStatus(w, http.StatusNotFound, nil, err)
		return
	}
	write(w, map[string]any{"terminated": err == nil}, err)
}
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	err := s.engine.Delete(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeStatus(w, http.StatusNotFound, nil, err)
		return
	}
	if err != nil {
		writeStatus(w, http.StatusConflict, nil, err)
		return
	}
	write(w, map[string]bool{"deleted": true}, nil)
}
func (s *Server) activeTurns(w http.ResponseWriter, _ *http.Request) {
	write(w, map[string]any{"instance_id": s.instanceID, "draining": s.draining.Load(), "turns": s.engine.ActiveTurns()}, nil)
}
func (s *Server) drain(w http.ResponseWriter, _ *http.Request) {
	s.draining.Store(true)
	write(w, map[string]any{"status": "draining", "active_turns": s.engine.ActiveTurns()}, nil)
}
func (s *Server) rejectWhileDraining(w http.ResponseWriter) bool {
	if !s.draining.Load() {
		return false
	}
	writeStatus(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"}, nil)
	return true
}
func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	xs, err := s.engine.Store().Jobs(r.Context(), r.PathValue("id"))
	write(w, xs, err)
}
func (s *Server) log(w http.ResponseWriter, r *http.Request) {
	messages, err := s.engine.Store().Messages(r.Context(), r.PathValue("id"))
	if err != nil {
		write(w, nil, err)
		return
	}
	out := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		var content any
		if err := json.Unmarshal([]byte(message.ContentJSON), &content); err != nil {
			content = message.ContentJSON
		}
		out = append(out, map[string]any{"role": message.Role, "content": content, "timestamp": message.CreatedAt})
	}
	write(w, out, nil)
}
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	if after == 0 {
		last := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
		if colon := strings.LastIndexByte(last, ':'); colon >= 0 {
			last = last[colon+1:]
		}
		after, _ = strconv.Atoi(last)
	}
	tick := time.NewTicker(350 * time.Millisecond)
	defer tick.Stop()
	for {
		events, err := s.engine.Store().EventsAfter(r.Context(), r.PathValue("id"), after)
		if err != nil {
			if !strings.Contains(err.Error(), "context canceled") {
				slog.Warn("SSE", "error", err)
			}
			return
		}
		for _, e := range events {
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", e.EventID, e.Type, b)
			after = e.Seq
			fl.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}
func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
func write(w http.ResponseWriter, v any, err error) {
	writeStatus(w, http.StatusOK, v, err)
}
func writeStatus(w http.ResponseWriter, status int, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					http.Error(w, "origin does not match request host", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
