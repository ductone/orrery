package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/core"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed static/*
var assets embed.FS

type Server struct {
	engine *core.Engine
	http   *http.Server
}

func New(addr string, e *core.Engine) *Server {
	s := &Server{engine: e}
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /", http.FileServer(http.FS(static)))
	mux.HandleFunc("GET /api/sessions", s.sessions)
	mux.HandleFunc("POST /sessions", s.create)
	mux.HandleFunc("GET /sessions/{id}/events", s.events)
	mux.HandleFunc("POST /sessions/{id}/messages", s.message)
	mux.HandleFunc("DELETE /sessions/{id}", s.cancel)
	mux.HandleFunc("GET /sessions/{id}/jobs", s.jobs)
	s.http = &http.Server{Addr: addr, Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second}
	return s
}
func (s *Server) ListenAndServe() error              { return s.http.ListenAndServe() }
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }
func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	xs, err := s.engine.Store().Sessions(r.Context())
	write(w, xs, err)
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
	w.WriteHeader(http.StatusAccepted)
	write(w, map[string]any{"id": id}, nil)
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
	w.WriteHeader(http.StatusAccepted)
	write(w, map[string]bool{"accepted": true}, nil)
}
func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	write(w, map[string]bool{"cancelled": s.engine.Cancel(r.PathValue("id"))}, nil)
}
func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	xs, err := s.engine.Store().Jobs(r.Context(), r.PathValue("id"))
	write(w, xs, err)
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
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, b)
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
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}
