package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/herdr"
	"github.com/ellingtonsp/herdr-orch/internal/paths"
	"github.com/ellingtonsp/herdr-orch/internal/rpc"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

// Version is reported by `horch status` and `herdr-orch version`. Release builds set it
// with -ldflags "-X github.com/ellingtonsp/herdr-orch/internal/daemon.Version=v0.1.0".
var Version = "dev"

type Options struct {
	Paths  paths.Paths
	Config Config
	Logf   func(string, ...any)
	// HerdrGoneAfter: exit when herdr's socket stays unreachable this long.
	HerdrGoneAfter time.Duration
}

// Run is the daemon main loop. It takes over from any older daemon for this session,
// holds the session lock, serves orch.sock and follows herdr events until ctx ends, a
// newer daemon asks it to exit, or herdr goes away.
func Run(ctx context.Context, o Options) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	logf := o.Logf
	p := o.Paths
	if o.HerdrGoneAfter == 0 {
		o.HerdrGoneAfter = 30 * time.Second
	}

	// 1. Ask an older daemon to exit, then take the lock (newest wins: after a live
	//    handoff the new herdr server starts a second copy and the old one must go).
	_ = rpc.Call(ctx, p.Sock, "shutdown", "daemon", nil, nil)
	lock, err := acquireLock(ctx, p.Lock, 10*time.Second)
	if err != nil {
		return err
	}
	defer lock.Close()

	// 2. Refuse to run against an unknown herdr protocol.
	hc := herdr.New(p.HerdrSock)
	pong, err := hc.Ping(ctx)
	if err != nil {
		return fmt.Errorf("herdr unreachable at %s: %w", p.HerdrSock, err)
	}
	if !slices.Contains(herdr.SupportedProtocols, pong.Protocol) {
		msg := fmt.Sprintf("herdr-orch supports herdr protocol %v; server %s speaks %d. Not starting.", herdr.SupportedProtocols, pong.Version, pong.Protocol)
		_ = hc.Notify(ctx, "herdr-orch disabled", msg, true)
		return errors.New(msg)
	}

	st, err := store.Open(p.DB)
	if err != nil {
		return err
	}
	defer st.Close()

	cfg := o.Config
	cfg.ArchiveDir = p.Archive
	cfg.HerdrSock = p.HerdrSock
	e := NewEngine(st, hc, cfg)
	e.logf = logf

	_ = os.Remove(p.Sock)
	ln, err := net.Listen("unix", p.Sock)
	if err != nil {
		return err
	}
	_ = os.Chmod(p.Sock, 0o600)
	ownSock, _ := os.Stat(p.Sock)
	if l, ok := ln.(*net.UnixListener); ok {
		l.SetUnlinkOnClose(false) // removal is decided below
	}
	defer func() {
		ln.Close()
		// Only remove the socket if it is still ours, never a successor's.
		if fi, err := os.Stat(p.Sock); err == nil && ownSock != nil && os.SameFile(ownSock, fi) {
			_ = os.Remove(p.Sock)
		}
	}()
	logf("herdr-orch %s up: session=%s herdr=%s protocol=%d db=%s sock=%s", Version, p.Session, pong.Version, pong.Protocol, p.DB, p.Sock)

	ops := e.Ops()
	ops["ping"] = func(context.Context, string, json.RawMessage) (any, error) {
		return map[string]any{"version": Version, "session": p.Session, "pid": os.Getpid(), "herdr": pong.Version, "protocol": pong.Protocol, "watching": e.WatchSet()}, nil
	}
	ops["shutdown"] = func(context.Context, string, json.RawMessage) (any, error) {
		logf("shutdown requested")
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		return map[string]any{"pid": os.Getpid()}, nil
	}

	go serve(ctx, ln, ops, logf)
	go watchOwnership(ctx, lock, p.Sock, logf, cancel)

	// 3. Rebuild the watch set from the store, then follow herdr.
	e.recomputeWatch()
	go e.subscribeLoop(ctx, hc, o.HerdrGoneAfter, cancel)

	go func() {
		e.Sweep(ctx)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		n := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				n++
				e.Tick()
				if n%15 == 0 {
					e.RunDueSchedules(ctx)
				}
				if n%60 == 0 {
					e.Sweep(ctx)
				}
			}
		}
	}()

	<-ctx.Done()
	logf("herdr-orch exiting")
	return nil
}

// subscribeLoop keeps one events.subscribe connection covering global events plus the
// per-pane status of every watched pane, rebuilding it when the watch set changes. If
// herdr stays unreachable for goneAfter, the daemon exits (herdr never reaps plugin
// startup processes).
func (e *Engine) subscribeLoop(ctx context.Context, hc *herdr.Client, goneAfter time.Duration, exit func()) {
	var downSince time.Time
	for ctx.Err() == nil {
		subs := []herdr.Subscription{
			{"type": "pane.closed"}, {"type": "pane.exited"},
			{"type": "worktree.removed"},
		}
		for _, p := range e.WatchSet() {
			subs = append(subs, herdr.Subscription{"type": "pane.agent_status_changed", "pane_id": p})
		}
		sctx, scancel := context.WithCancel(ctx)
		ch, err := hc.Subscribe(sctx, subs)
		if err != nil {
			scancel()
			if code := herdr.ErrCode(err); code != "" {
				// herdr rejected a pane filter (pane vanished): reconcile and retry.
				e.logf("subscribe: %v", err)
				e.SyncStatuses(ctx)
				e.recomputeWatch()
				sleep(ctx, time.Second)
				continue
			}
			if downSince.IsZero() {
				downSince = time.Now()
				e.logf("herdr unreachable: %v", err)
			}
			if time.Since(downSince) > goneAfter {
				e.logf("herdr gone for %s; exiting", goneAfter)
				exit()
				return
			}
			sleep(ctx, time.Second)
			continue
		}
		downSince = time.Time{}
		e.SyncStatuses(ctx)
		e.Tick()
		dropped := false
		for !dropped {
			select {
			case <-ctx.Done():
				scancel()
				return
			case <-e.RewatchC():
				// Drain the old connection's buffered events before switching.
				dropped = true
			case ev, ok := <-ch:
				if !ok {
					dropped = true
					break
				}
				e.HandleEvent(ev)
			}
		}
		scancel()
		for ev := range ch {
			e.HandleEvent(ev)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func serve(ctx context.Context, ln net.Listener, ops map[string]Handler, logf func(string, ...any)) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logf("accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go handle(ctx, conn, ops, logf)
	}
}

func handle(ctx context.Context, conn net.Conn, ops map[string]Handler, logf func(string, ...any)) {
	defer conn.Close()
	r := bufio.NewReaderSize(conn, 1<<20)
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var req rpc.Request
	resp := rpc.Response{}
	if err := json.Unmarshal(line, &req); err != nil {
		resp.Error = &rpc.Error{Code: "bad_request", Message: err.Error()}
	} else if h, ok := ops[req.Op]; !ok {
		resp.Error = &rpc.Error{Code: "unknown_op", Message: "unknown op " + req.Op}
	} else {
		// Cancel long polls when the client hangs up.
		cctx, cancel := context.WithCancel(ctx)
		go func() {
			buf := make([]byte, 1)
			_, _ = conn.Read(buf)
			cancel()
		}()
		caller := req.Caller
		if caller == "" {
			caller = "human"
		}
		out, err := h(cctx, caller, req.Args)
		cancel()
		if err != nil && ctx.Err() != nil {
			// Shutting down: drop the connection without an answer so the client knows the
			// outcome is unknown (and re-sends only if the op is safe to repeat).
			return
		}
		if err != nil {
			if ref, ok := AsRefusal(err); ok {
				resp.Error = &rpc.Error{Code: ref.Code, Message: ref.Message}
			} else {
				if !errors.Is(err, context.Canceled) { // client hung up on a long poll
					logf("op %s: %v", req.Op, err)
				}
				resp.Error = &rpc.Error{Code: "error", Message: err.Error()}
			}
		} else {
			resp.OK = true
			resp.Result, _ = json.Marshal(out)
		}
	}
	b, _ := json.Marshal(resp)
	_, _ = conn.Write(append(b, '\n'))
}

// acquireLock takes an exclusive flock on path, retrying until timeout.
func acquireLock(ctx context.Context, path string, timeout time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			_ = f.Truncate(0)
			_, _ = f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
			return f, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("another herdr-orch daemon holds %s", path)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// watchOwnership exits the daemon when its lock file or socket is removed or replaced
// (state dir wiped, or another daemon took over): a lock on a deleted file protects nothing.
func watchOwnership(ctx context.Context, lock *os.File, sock string, logf func(string, ...any), exit func()) {
	held, err := lock.Stat()
	if err != nil {
		return
	}
	ownSock, err := os.Stat(sock)
	if err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		cur, err := os.Stat(lock.Name())
		if err != nil || !os.SameFile(held, cur) {
			logf("lock file %s removed or replaced; exiting", lock.Name())
			exit()
			return
		}
		if cur, err := os.Stat(sock); err != nil || !os.SameFile(ownSock, cur) {
			logf("socket %s removed or replaced; exiting", sock)
			exit()
			return
		}
	}
}
