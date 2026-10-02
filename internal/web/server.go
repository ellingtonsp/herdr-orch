// Package web serves the embedded fleet UI and a deliberately small daemon-backed API.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/config"
	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/rpc"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

//go:embed assets/*
var assets embed.FS

// Backend is a Go client, never a subprocess. SocketClient only attaches to an
// existing daemon: a web request cannot start, replace or stop an orchestrator.
type Backend interface {
	Call(context.Context, string, string, any, any) error
	Stream(context.Context, string, string, any, func(json.RawMessage) error) error
}
type SocketClient struct{ Socket string }

func (c SocketClient) Call(ctx context.Context, op, caller string, args, out any) error {
	return rpc.Call(ctx, c.Socket, op, caller, args, out)
}
func (c SocketClient) Stream(ctx context.Context, op, caller string, args any, fn func(json.RawMessage) error) error {
	return rpc.Stream(ctx, c.Socket, op, caller, args, fn)
}

type Server struct {
	backend Backend
	cfg     config.Web
	poll    time.Duration
}

func New(b Backend, c config.Web) *Server { return &Server{backend: b, cfg: c, poll: 5 * time.Second} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/identity", func(w http.ResponseWriter, r *http.Request) {
		principal, allowed := s.identity(r)
		respond(w, http.StatusOK, map[string]any{"principal": principal, "can_write": allowed})
	})
	mux.HandleFunc("GET /api/plan", s.plan)
	mux.HandleFunc("GET /api/activity", s.activity)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("POST /api/plan/items/{action}", s.edit)
	files, _ := fs.Sub(assets, "assets")
	mux.Handle("GET /", http.FileServer(http.FS(files)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		mux.ServeHTTP(w, r)
	})
}

// Header identity is trusted only behind tailscale serve (see README). The
// explicit local exception is limited to loopback peers, even on a wider bind.
func (s *Server) identity(r *http.Request) (string, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	loopback := err == nil && ip != nil && ip.IsLoopback()
	values := r.Header.Values("Tailscale-User-Login")
	if len(values) > 0 {
		if !loopback || len(values) != 1 {
			return "", false
		}
		principal := strings.TrimSpace(values[0])
		return principal, principal != "" && s.cfg.OwnerPrincipal != "" && principal == s.cfg.OwnerPrincipal
	}
	if loopback && s.cfg.AllowLocalWrites && s.cfg.OwnerPrincipal != "" {
		return s.cfg.OwnerPrincipal, true
	}
	return "", false
}
func ref(r *http.Request) daemon.PlanRef {
	return daemon.PlanRef{Project: r.URL.Query().Get("project"), Day: r.URL.Query().Get("day")}
}
func (s *Server) call(ctx context.Context, op, caller string, args, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	return s.backend.Call(ctx, op, caller, args, out)
}
func (s *Server) plan(w http.ResponseWriter, r *http.Request) {
	var out daemon.PlanExport
	err := s.call(r.Context(), "plan.export", "web:reader", daemon.PlanExportArgs{PlanRef: ref(r), Format: "json"}, &out)
	if err != nil {
		apiError(w, err)
		return
	}
	respond(w, http.StatusOK, out)
}
func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	var out daemon.WebActivity
	if err := s.call(r.Context(), "web.activity", "web:reader", nil, &out); err != nil {
		apiError(w, err)
		return
	}
	respond(w, http.StatusOK, out)
}
func (s *Server) edit(w http.ResponseWriter, r *http.Request) {
	principal, allowed := s.identity(r)
	if !allowed {
		problem(w, 403, "forbidden", "Plan edits require the configured owner identity.")
		return
	}
	// JSON plus same-origin checks prevent a foreign page submitting local edits.
	content, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if content != "application/json" {
		problem(w, 415, "content_type", "Use application/json.")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
			problem(w, 403, "origin", "Cross-origin edits are refused.")
			return
		}
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		problem(w, 403, "origin", "Cross-origin edits are refused.")
		return
	}
	action := r.PathValue("action")
	switch action {
	case "add", "move", "hold", "release", "remove":
	default:
		problem(w, 404, "unknown_action", "Unknown plan edit.")
		return
	}
	var args daemon.PlanItemArgs
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		problem(w, 400, "bad_request", "Invalid edit JSON.")
		return
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		problem(w, 400, "bad_request", "Expected one edit.")
		return
	}
	if args.Project == "" || args.Day == "" || args.Item == "" || args.IfPlanVersion <= 0 || (action != "add" && args.IfVersion <= 0) {
		problem(w, 400, "version_required", "Name the project, day and item; include positive if_plan_version and if_version (except add).")
		return
	}
	// Ignore untrusted principal input. Both caller and principal come from auth.
	args.Principal = principal
	var out store.PlanWrite
	if err := s.call(r.Context(), "plan.item."+action, "web:"+principal, args, &out); err != nil {
		apiError(w, err)
		return
	}
	respond(w, http.StatusOK, out)
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, status int, code, message string) {
	respond(w, status, &rpc.Error{Code: code, Message: message})
}
func apiError(w http.ResponseWriter, err error) {
	var re *rpc.Error
	if errors.As(err, &re) {
		status := 400
		switch re.Code {
		case "version_conflict", "plan_final":
			status = 409
		case "no_plan", "unknown_item":
			status = 404
		}
		respond(w, status, re)
		return
	}
	problem(w, 503, "unavailable", "The daemon is unavailable; refresh before retrying an edit.")
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if _, ok := w.(http.Flusher); !ok {
		problem(w, 500, "streaming", "Streaming is unavailable.")
		return
	}
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("since")
	}
	var since int64
	if cursor != "" {
		var err error
		since, err = strconv.ParseInt(cursor, 10, 64)
		if err != nil || since < 0 {
			problem(w, 400, "bad_cursor", "Invalid event cursor.")
			return
		}
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ch := make(chan json.RawMessage, 64)
	finished := make(chan error, 1)
	go func() {
		finished <- s.backend.Stream(ctx, "plan.subscribe", "web:reader", daemon.PlanEventsArgs{All: true, Since: since}, func(raw json.RawMessage) error {
			select {
			case ch <- raw:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(kind string, id int64, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
		if id > 0 {
			if _, err = fmt.Fprintf(w, "id: %d\n", id); err != nil {
				return err
			}
		}
		if _, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b); err != nil {
			return err
		}
		return http.NewResponseController(w).Flush()
	}
	if send("ready", 0, map[string]any{"since": since}) != nil {
		return
	}
	poll := time.NewTicker(s.poll)
	defer poll.Stop()
	for {
		select {
		case raw := <-ch:
			var ev store.PlanEvent
			if json.Unmarshal(raw, &ev) != nil {
				return
			}
			if send("plan", ev.Seq, ev) != nil {
				return
			}
		case <-poll.C:
			var out daemon.WebActivity
			if s.call(ctx, "web.activity", "web:reader", nil, &out) != nil {
				_ = send("unavailable", 0, map[string]string{"message": "Daemon unavailable"})
				return
			}
			if send("activity", 0, out) != nil {
				return
			}
		case <-finished:
			_ = send("unavailable", 0, map[string]string{"message": "Plan stream disconnected"})
			return
		case <-ctx.Done():
			return
		}
	}
}

// Serve runs HTTP until the command's signal context ends. SSE connections use
// that context, so shutdown also closes subscribers without waiting for a poll.
func Serve(ctx context.Context, ln net.Listener, s *Server) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = srv.Close()
		case <-done:
		}
	}()
	err := srv.Serve(ln)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
