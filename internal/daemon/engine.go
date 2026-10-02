// Package daemon is the long-lived herdr-orch process: the only writer to the store, the
// herdr event subscriber, and the server behind orch.sock.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/config"
	"github.com/ellingtonsp/herdr-orch/internal/herdr"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

// Herdr is the subset of the herdr API the engine uses (faked in tests).
type Herdr interface {
	AgentPrompt(ctx context.Context, target, text string) error
	SendText(ctx context.Context, paneID, text string) error
	PaneGet(ctx context.Context, paneID string) (herdr.Pane, error)
	AgentGet(ctx context.Context, target string) (herdr.Pane, error)
	Notify(ctx context.Context, title, body string, urgent bool) error
	PaneTokens(ctx context.Context, paneID string, tokens map[string]*string) error
	ProcessInfo(ctx context.Context, paneID string) (herdr.ProcessInfo, error)
	PaneClose(ctx context.Context, paneID string) error
	PaneRead(ctx context.Context, paneID string, lines int) (string, error)
	NewTab(ctx context.Context, workspaceID, cwd, label string, env map[string]string) (herdr.Pane, error)
	AgentStart(ctx context.Context, name, kind, paneID string, args []string) (herdr.Pane, error)
}

type Config struct {
	// IdleReportAfter is how long a dispatched worker may sit idle after working, with no
	// `horch done`, before the coordinator is told (once per idle episode). The dispatch is
	// never failed for this: long loops, CI waits and background jobs look the same.
	IdleReportAfter time.Duration
	// UnobservedAfter is how long a dispatch may go without any observed activity before
	// the coordinator is told the prompt may not have landed.
	UnobservedAfter time.Duration
	// BlockedAfter is how long a worker must stay blocked before it is escalated, so a
	// permission prompt that auto mode or the coordinator clears in seconds pages nobody.
	BlockedAfter time.Duration
	// ArchiveDir receives pane transcripts on release.
	ArchiveDir string
	// HorchBin is the horch binary used by scheduled `horch` actions and named in prompts.
	HorchBin string
	// HerdrSock is the herdr session socket, pinned in worker prompts.
	HerdrSock string
	// UserConfig loads ~/.config/horch/config.toml (owner principal, default project). It
	// is read on each plan write, so edits apply without a daemon restart.
	UserConfig func() (config.Config, error)
}

func DefaultConfig() Config {
	return Config{
		IdleReportAfter: 3 * time.Minute,
		UnobservedAfter: 90 * time.Second,
		BlockedAfter:    20 * time.Second,
		UserConfig:      config.Load,
	}
}

type Engine struct {
	mu     sync.Mutex
	st     *store.Store
	h      Herdr
	cfg    Config
	notify notifier
	hub    planHub
	logf   func(string, ...any)

	status      map[string]string    // pane → last known agent status
	blocked     map[string]time.Time // pane → when its current blocked episode began
	blockedTold map[string]bool      // pane → current blocked episode already escalated
	closing     map[string]bool      // panes we are closing ourselves
	watchMu     sync.Mutex
	watched     map[string]bool
	rewatch     chan struct{}

	promptRetry time.Duration
	startGrace  time.Duration // extra wait for an agent after herdr's start timeout

	// async runs herdr side effects (notifications, badges) off the engine lock.
	async func(func())
}

func NewEngine(st *store.Store, h Herdr, cfg Config) *Engine {
	return &Engine{
		st:          st,
		h:           h,
		cfg:         cfg,
		notify:      notifier{ch: make(chan struct{})},
		logf:        log.Printf,
		status:      map[string]string{},
		blocked:     map[string]time.Time{},
		blockedTold: map[string]bool{},
		closing:     map[string]bool{},
		watched:     map[string]bool{},
		rewatch:     make(chan struct{}, 1),
		promptRetry: 750 * time.Millisecond,
		startGrace:  2 * time.Minute,
		async:       func(f func()) { go f() },
	}
}

// notifier wakes long-polling requests whenever state changes.
type notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func (n *notifier) wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func (n *notifier) broadcast() {
	n.mu.Lock()
	close(n.ch)
	n.ch = make(chan struct{})
	n.mu.Unlock()
}

func bg() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func isPane(id string) bool { return strings.Contains(id, ":") }

// ---------- watch set ----------

// WatchSet returns the panes whose agent status the subscription must follow: every pane
// with an active dispatch and every live worker.
func (e *Engine) WatchSet() []string {
	e.watchMu.Lock()
	defer e.watchMu.Unlock()
	out := make([]string, 0, len(e.watched))
	for p := range e.watched {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// RewatchC fires when the watch set changed and the subscription must be rebuilt.
func (e *Engine) RewatchC() <-chan struct{} { return e.rewatch }

func (e *Engine) recomputeWatch() {
	want := map[string]bool{}
	if ds, err := e.st.ActiveDispatches(); err == nil {
		for _, d := range ds {
			want[d.PaneID] = true
		}
	}
	if ws, err := e.st.ListWorkers("", true); err == nil {
		for _, w := range ws {
			want[w.PaneID] = true
		}
	}
	e.watchMu.Lock()
	changed := len(want) != len(e.watched)
	if !changed {
		for p := range want {
			if !e.watched[p] {
				changed = true
				break
			}
		}
	}
	e.watched = want
	e.watchMu.Unlock()
	if changed {
		select {
		case e.rewatch <- struct{}{}:
		default:
		}
	}
}

// SyncStatuses asks herdr for the current status of watched panes (after a (re)subscribe,
// so transitions missed while disconnected are not lost).
func (e *Engine) SyncStatuses(ctx context.Context) {
	for _, p := range e.WatchSet() {
		pane, err := e.h.PaneGet(ctx, p)
		if err != nil {
			if herdr.ErrCode(err) == "pane_not_found" || strings.Contains(err.Error(), "not found") {
				e.HandleEvent(herdr.Event{Name: "pane.closed", Data: mustJSON(map[string]string{"pane_id": p})})
			}
			continue
		}
		e.setStatus(p, pane.AgentStatus)
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// ---------- events ----------

// HandleEvent applies one herdr event.
func (e *Engine) HandleEvent(ev herdr.Event) {
	switch ev.Name {
	case "pane.agent_status_changed":
		var d struct {
			PaneID      string `json:"pane_id"`
			AgentStatus string `json:"agent_status"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			e.setStatus(d.PaneID, d.AgentStatus)
		}
	case "pane.exited", "pane.closed":
		var d struct {
			PaneID string `json:"pane_id"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			e.paneGone(d.PaneID, strings.TrimPrefix(ev.Name, "pane."))
		}
	case "worktree.removed":
		var d struct {
			Worktree struct {
				Path string `json:"path"`
			} `json:"worktree"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.Worktree.Path != "" {
			e.worktreeRemoved(d.Worktree.Path)
		}
	}
}

func (e *Engine) isWatched(p string) bool {
	e.watchMu.Lock()
	defer e.watchMu.Unlock()
	return e.watched[p]
}

func (e *Engine) setStatus(pane, status string) {
	if pane == "" || status == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	prev := e.status[pane]
	if prev == status {
		return
	}
	e.status[pane] = status
	if status == "blocked" {
		if _, ok := e.blocked[pane]; !ok {
			e.blocked[pane] = e.st.Now()
		}
	} else {
		delete(e.blocked, pane)
		delete(e.blockedTold, pane)
	}
	if e.isWatched(pane) {
		e.logf("status %s: %s → %s", pane, prev, status)
	}
	d, err := e.st.ActiveDispatchForPane(pane)
	if err == nil {
		if status == "working" || status == "blocked" || status == "idle" || status == "done" {
			d, _ = e.st.ObserveStatus(d.ID, status)
		}
		e.evaluateLocked(d)
	} else if w, err := e.st.GetWorker(pane); err == nil && w.State == store.WorkerLive {
		switch status {
		case "idle", "done":
			// A worker just became free: ready tasks may be waiting for it.
			e.autoDispatchLocked(w.RunID)
		}
	}
	e.notify.broadcast()
}

// paneGone handles pane.exited / pane.closed.
func (e *Engine) paneGone(pane, why string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.status, pane)
	delete(e.blocked, pane)
	delete(e.blockedTold, pane)
	if e.closing[pane] {
		return
	}
	if d, err := e.st.ActiveDispatchForPane(pane); err == nil {
		e.settleLocked(d, false, "failed: pane_"+why, "")
	}
	if w, err := e.st.GetWorker(pane); err == nil && w.State == store.WorkerLive {
		_ = e.st.SetWorkerState(pane, store.WorkerExited, "pane "+why, "")
		e.recomputeWatch()
	}
	e.notify.broadcast()
}

func (e *Engine) worktreeRemoved(path string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ws, err := e.st.ListWorkers("", true)
	if err != nil {
		return
	}
	for _, w := range ws {
		if w.Worktree != "" && (w.Worktree == path || strings.HasPrefix(w.Worktree, strings.TrimSuffix(path, "/")+"/")) {
			e.escalateLocked(w.RunID, fmt.Sprintf("worktree removed under live worker %s", w.PaneID),
				fmt.Sprintf("Worktree %s was removed while worker %s (%s) was still running in it. Check the worker before it commits anything.", path, w.PaneID, w.Agent), "")
		}
	}
}

// ---------- settlement ----------

// Tick re-evaluates every active dispatch (silent-grace and unobserved timers) and expires
// gates. Called every second.
func (e *Engine) Tick() {
	e.mu.Lock()
	defer e.mu.Unlock()
	ds, err := e.st.ActiveDispatches()
	if err == nil {
		for _, d := range ds {
			e.evaluateLocked(d)
		}
	}
	e.escalateBlockedLocked()
	if gs, err := e.st.ExpireGates(); err == nil {
		for _, g := range gs {
			e.escalateLocked(g.RunID, "gate "+g.ID+" timed out",
				fmt.Sprintf("Decision gate %s (%q) timed out; task %s stays blocked until someone runs `horch gate resolve --id %s --decision ...`.", g.ID, g.Question, g.TaskID, g.ID), g.TaskID)
		}
		if len(gs) > 0 {
			e.notify.broadcast()
		}
	}
}

// escalateBlockedLocked escalates workers that have stayed blocked for BlockedAfter, once
// per blocked episode.
func (e *Engine) escalateBlockedLocked() {
	now := e.st.Now()
	for pane, since := range e.blocked {
		if e.blockedTold[pane] || now.Sub(since) < e.cfg.BlockedAfter {
			continue
		}
		e.blockedTold[pane] = true
		held := now.Sub(since).Round(time.Second)
		if d, err := e.st.ActiveDispatchForPane(pane); err == nil {
			e.escalateLocked(d.RunID, fmt.Sprintf("worker %s blocked on task %s", pane, d.TaskID),
				fmt.Sprintf("Worker pane %s (dispatch %s, task %s) has been blocked at an approval or question prompt for %s. Inspect it with `horch worker read --worker %s`.", pane, d.ID, d.TaskID, held, pane), d.TaskID)
		} else if w, err := e.st.GetWorker(pane); err == nil && w.State == store.WorkerLive {
			e.escalateLocked(w.RunID, fmt.Sprintf("worker %s blocked", pane),
				fmt.Sprintf("Worker pane %s (no active dispatch) has been blocked at an approval or question prompt for %s.", pane, held), "")
		}
	}
}

// evaluateLocked applies the settlement rule to an active dispatch:
//
//   - done message received and the agent is not working/blocked → settle with its outcome
//   - saw working, now idle for IdleReportAfter, no done message → tell the coordinator
//     once per idle episode (with the screen tail); the dispatch stays open
//   - nothing observed for UnobservedAfter → tell the coordinator once (never resend blindly)
//
// Silence is never failure: only the worker's own --failed report, its pane going away, a
// stop, or the coordinator's `horch dispatch fail` fails a dispatch.
func (e *Engine) evaluateLocked(d store.Dispatch) {
	if !d.Active() || d.Status == store.DispatchPending {
		return
	}
	now := e.st.Now().UnixMilli()
	status := e.status[d.PaneID]
	busy := status == "working" || status == "blocked"
	if d.DoneMessageID != "" && !busy {
		result := ""
		if m, err := e.st.GetMessage(d.DoneMessageID); err == nil {
			result = m.Body
		}
		ok := d.DoneOutcome != "failed"
		outcome := "completed"
		if !ok {
			outcome = "failed: reported by worker"
		}
		e.settleLocked(d, ok, outcome, result)
		return
	}
	if d.DoneMessageID == "" && d.SawWorking && d.IdleSince > 0 && !d.IdleEscalated &&
		(status == "idle" || status == "done") && now-d.IdleSince >= e.idleReportAfter(d.RunID).Milliseconds() {
		_ = e.st.MarkIdleEscalated(d.ID)
		idleFor := time.Duration(now-d.IdleSince) * time.Millisecond
		body := fmt.Sprintf("Dispatch %s (task %s) to %s has been idle for %s without `horch done`. It may have finished without reporting, be waiting on something, or be between loop iterations. The dispatch stays open; nothing was failed or re-dispatched.\n\nNext: read it, then `horch dispatch nudge --task %s` (asks it to report; %d nudge(s) so far) or `horch dispatch fail --task %s --reason \"...\"`.",
			d.ID, d.TaskID, d.PaneID, idleFor.Round(time.Second), d.TaskID, d.Nudges, d.TaskID)
		if tail := e.screenTail(d.PaneID, 30); tail != "" {
			body += "\n\nLast lines of " + d.PaneID + ":\n" + tail
		}
		// Coordinator-only: this is routine for long-running workers, not a human alert.
		e.messageCoordinatorLocked(d.RunID, store.KindEscalation,
			fmt.Sprintf("worker %s idle on task %s without reporting", d.PaneID, d.TaskID), body, d.TaskID, "")
		return
	}
	if !d.SawWorking && d.DoneMessageID == "" && !d.UnobservedEscalated && now-d.StartedAt >= e.cfg.UnobservedAfter.Milliseconds() {
		_ = e.st.MarkUnobservedEscalated(d.ID)
		e.escalateLocked(d.RunID, fmt.Sprintf("dispatch %s: no activity observed", d.ID),
			fmt.Sprintf("Dispatch %s (task %s) to %s has shown no activity for %s. The prompt may not have landed; check with `horch worker read --worker %s` before re-sending.", d.ID, d.TaskID, d.PaneID, e.cfg.UnobservedAfter, d.PaneID), d.TaskID)
	}
}

// idleReportAfter is the run's idle-report delay, else the daemon default.
func (e *Engine) idleReportAfter(run string) time.Duration {
	if r, err := e.st.GetRun(run); err == nil && r.IdleReportMS > 0 {
		return time.Duration(r.IdleReportMS) * time.Millisecond
	}
	return e.cfg.IdleReportAfter
}

// screenTail returns the last n non-blank lines of a pane, or "".
func (e *Engine) screenTail(pane string, n int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	text, err := e.h.PaneRead(ctx, pane, n*3)
	if err != nil {
		return ""
	}
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (e *Engine) settleLocked(d store.Dispatch, ok bool, outcome, result string) {
	st, err := e.st.Settle(d.ID, ok, outcome, result)
	if err != nil {
		e.logf("settle %s: %v", d.ID, err)
		return
	}
	e.logf("dispatch %s settled %s (%s)", d.ID, st.Dispatch.Status, outcome)
	e.afterSettleLocked(st)
}

func (e *Engine) afterSettleLocked(st store.Settlement) {
	d, t := st.Dispatch, st.Task
	switch d.Status {
	case store.DispatchCompleted:
		body := fmt.Sprintf("Task %s (%s) completed by %s (dispatch %s).", t.ID, t.Title, d.PaneID, d.ID)
		if t.Result != "" {
			body += "\n\n" + t.Result
		}
		if len(st.NowReady) > 0 {
			body += "\n\nNow ready: " + strings.Join(st.NowReady, ", ")
		}
		e.messageCoordinatorLocked(d.RunID, store.KindDone, "task "+t.ID+" completed", body, t.ID, "succeeded")
	case store.DispatchFailed:
		e.escalateLocked(d.RunID, fmt.Sprintf("task %s dispatch failed", t.ID),
			fmt.Sprintf("Dispatch %s of task %s (%s) to %s failed: %s. Attempt %d; the task is %s again.", d.ID, t.ID, t.Title, d.PaneID, d.Outcome, t.Attempts, t.Status), t.ID)
	case store.DispatchCircuitBroken:
		e.escalateLocked(d.RunID, fmt.Sprintf("task %s circuit broken", t.ID),
			fmt.Sprintf("Task %s (%s) failed %d times (last: %s on %s). It is now failed; reset with `horch task update --id %s --reset-attempts --status ready`.", t.ID, t.Title, t.Attempts, d.Outcome, d.PaneID, t.ID), t.ID)
	}
	e.badgeTask(d.PaneID, "")
	e.recomputeWatch()
	e.autoDispatchLocked(d.RunID)
	e.notify.broadcast()
}

// ---------- messaging helpers ----------

func (e *Engine) coordinatorOf(run string) string {
	if run == "" {
		return "human"
	}
	r, err := e.st.GetRun(run)
	if err != nil || r.CoordinatorPaneID == "" {
		return "human"
	}
	return r.CoordinatorPaneID
}

func (e *Engine) messageCoordinatorLocked(run, kind, subject, body, task, outcome string) {
	to := e.coordinatorOf(run)
	m, err := e.st.InsertMessage(store.Message{RunID: run, From: store.Daemon, To: to, Kind: kind, Subject: subject, Body: body, TaskID: task, Outcome: outcome})
	if err != nil {
		e.logf("message: %v", err)
		return
	}
	e.badgeMail(m.To)
	e.notify.broadcast() // wake `check --wait` and `ask` long-polls
}

// escalateLocked messages the coordinator and pops a herdr notification for the human.
func (e *Engine) escalateLocked(run, subject, body, task string) {
	e.logf("escalation: %s", subject)
	e.messageCoordinatorLocked(run, store.KindEscalation, subject, body, task, "")
	e.async(func() {
		ctx, cancel := bg()
		defer cancel()
		_ = e.h.Notify(ctx, "horch: "+subject, firstLine(body), true)
	})
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// badgeMail refreshes a pane's unread-count sidebar token.
func (e *Engine) badgeMail(pane string) {
	if !isPane(pane) {
		return
	}
	n, err := e.st.UnreadCount(pane)
	if err != nil {
		return
	}
	var v *string
	if n > 0 {
		s := fmt.Sprintf("✉ %d", n)
		v = &s
	}
	e.async(func() {
		ctx, cancel := bg()
		defer cancel()
		_ = e.h.PaneTokens(ctx, pane, map[string]*string{"horch_mail": v})
	})
}

// badgeTask shows the task a pane is working on (or clears it).
func (e *Engine) badgeTask(pane, task string) {
	if !isPane(pane) {
		return
	}
	var v *string
	if task != "" {
		v = &task
	}
	e.async(func() {
		ctx, cancel := bg()
		defer cancel()
		_ = e.h.PaneTokens(ctx, pane, map[string]*string{"horch_task": v})
	})
}

// ---------- refusals ----------

func refusal(code, format string, a ...any) error {
	return &store.Refusal{Code: code, Message: fmt.Sprintf(format, a...)}
}

// AsRefusal extracts a refusal (store or engine) from err.
func AsRefusal(err error) (*store.Refusal, bool) {
	var r *store.Refusal
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}
