package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/herdr"
	"github.com/ellingtonsp/herdr-orch/internal/paths"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

type WorkerStartArgs struct {
	Run   string   `json:"run,omitempty"`
	Agent string   `json:"agent"`
	Name  string   `json:"name,omitempty"`
	Cwd   string   `json:"cwd,omitempty"`
	Args  []string `json:"args,omitempty"`
	// Pane starts the agent in an existing shell pane (e.g. the root pane of a workspace
	// made by `herdr worktree create`) instead of opening a new tab.
	Pane string `json:"pane,omitempty"`
}

var agentName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func (e *Engine) opWorkerStart(ctx context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[WorkerStartArgs](raw)
	if err != nil {
		return nil, err
	}
	return e.startWorker(ctx, caller, a)
}

// startWorker opens a new tab in the caller's workspace, starts the agent there and
// registers it. An agent that comes up blocked (e.g. a folder-trust prompt) is registered
// and escalated; it is never answered automatically.
func (e *Engine) startWorker(ctx context.Context, caller string, a WorkerStartArgs) (store.Worker, error) {
	if a.Agent == "" {
		return store.Worker{}, refusal("bad_args", "--agent is required (claude|pi|codex|...)")
	}
	run, err := e.runFor(caller, a.Run)
	if err != nil {
		return store.Worker{}, err
	}
	if a.Cwd != "" {
		abs, err := filepath.Abs(a.Cwd)
		if err != nil {
			return store.Worker{}, err
		}
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			return store.Worker{}, refusal("bad_cwd", "%s is not a directory", abs)
		}
		a.Cwd = abs
	}
	if a.Name == "" {
		ws, _ := e.st.ListWorkers("", false)
		a.Name = fmt.Sprintf("%s-%s-%d", strings.ToLower(a.Agent), run.ID, len(ws)+1)
	}
	if !agentName.MatchString(a.Name) {
		return store.Worker{}, refusal("bad_name", "worker name must match [a-z][a-z0-9_-]{0,31}")
	}
	var pane herdr.Pane
	ownPane := a.Pane == ""
	if !ownPane {
		p, err := e.h.PaneGet(ctx, a.Pane)
		if err != nil {
			return store.Worker{}, refusal("unknown_pane", "pane %s: %v", a.Pane, err)
		}
		if p.Agent != nil && *p.Agent != "" {
			return store.Worker{}, refusal("pane_busy", "pane %s already runs %s", a.Pane, *p.Agent)
		}
		if a.Cwd == "" && p.Cwd != nil {
			a.Cwd = *p.Cwd
		}
		pane = p
	} else {
		ws := ""
		if isPane(caller) {
			if p, err := e.h.PaneGet(ctx, caller); err == nil {
				ws = p.WorkspaceID
			}
		}
		var env map[string]string
		if e.cfg.HorchBin != "" {
			// Workers must be able to run `horch done` whatever their shell profile says.
			env = map[string]string{"PATH": filepath.Dir(e.cfg.HorchBin) + ":" + os.Getenv("PATH")}
		}
		var err error
		pane, err = e.h.NewTab(ctx, ws, a.Cwd, a.Name, env)
		if err != nil {
			return store.Worker{}, fmt.Errorf("create tab: %w", err)
		}
	}
	// A fresh tab's shell is still starting (profile, prompt) for a moment; herdr
	// refuses to launch into it until it is an available shell.
	var ag herdr.Pane
	var startErr error
	for i := 0; i < 40; i++ {
		ag, startErr = e.h.AgentStart(ctx, a.Name, a.Agent, pane.PaneID, a.Args)
		if herdr.ErrCode(startErr) != "agent_pane_busy" {
			break
		}
		time.Sleep(e.promptRetry)
	}
	if herdr.ErrCode(startErr) == "timeout" {
		// herdr's start wait (60s) expired, but slow agents (many MCP servers, plugins)
		// often come up moments later. Look before declaring the start failed.
		if late, ok := e.awaitLateStart(ctx, pane.PaneID, a.Name); ok {
			e.logf("worker %s: agent came up after herdr's start timeout (%s)", a.Name, late.AgentStatus)
			ag, startErr = late, nil
			if late.AgentStatus == "blocked" {
				startErr = &herdr.Error{Code: "agent_not_ready", Message: "agent " + a.Name + " is blocked during startup"}
			}
		}
	}
	blocked := false
	if startErr != nil {
		if herdr.ErrCode(startErr) == "agent_not_ready" && strings.Contains(startErr.Error(), "blocked") {
			blocked = true
		} else {
			if ownPane {
				cctx, cancel := bg()
				_ = e.h.PaneClose(cctx, pane.PaneID)
				cancel()
			}
			return store.Worker{}, fmt.Errorf("start %s: %w", a.Agent, startErr)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w, err := e.st.RegisterWorker(store.Worker{PaneID: pane.PaneID, RunID: run.ID, Name: a.Name, Agent: a.Agent, Worktree: a.Cwd})
	if err != nil {
		return w, err
	}
	if blocked {
		e.status[pane.PaneID] = "blocked"
		e.escalateLocked(run.ID, fmt.Sprintf("worker %s blocked at startup", a.Name),
			fmt.Sprintf("Worker %s (%s, pane %s) is waiting at a startup prompt (often a folder-trust dialog). Answer it in the pane; horch does not answer it for you.", a.Name, a.Agent, pane.PaneID), "")
	} else if ag.AgentStatus != "" {
		e.status[pane.PaneID] = ag.AgentStatus
	}
	e.recomputeWatch()
	e.notify.broadcast()
	return w, nil
}

type WorkerRegisterArgs struct {
	Run      string `json:"run,omitempty"`
	Pane     string `json:"pane"`
	Name     string `json:"name,omitempty"`
	Worktree string `json:"worktree,omitempty"`
}

// opWorkerRegister adopts an existing agent pane as a worker of the run.
func (e *Engine) opWorkerRegister(ctx context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[WorkerRegisterArgs](raw)
	if err != nil {
		return nil, err
	}
	if a.Pane == "" || a.Pane == "me" {
		a.Pane = caller
	}
	run, err := e.runFor(caller, a.Run)
	if err != nil {
		return nil, err
	}
	p, err := e.h.PaneGet(ctx, a.Pane)
	if err != nil {
		return nil, refusal("unknown_pane", "pane %s: %v", a.Pane, err)
	}
	agent := ""
	if p.Agent != nil {
		agent = *p.Agent
	}
	if a.Worktree == "" && p.Cwd != nil {
		a.Worktree = *p.Cwd
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w, err := e.st.RegisterWorker(store.Worker{PaneID: a.Pane, RunID: run.ID, Name: a.Name, Agent: agent, Worktree: a.Worktree})
	if err == nil {
		e.status[a.Pane] = p.AgentStatus
		e.recomputeWatch()
		e.notify.broadcast()
	}
	return w, err
}

type WorkerRef struct {
	Worker string `json:"worker"`
	Force  bool   `json:"force,omitempty"`
	Lines  int    `json:"lines,omitempty"`
	Off    bool   `json:"off,omitempty"`
}

func (e *Engine) findWorker(caller, ref string) (store.Worker, error) {
	if ref == "" || ref == "me" {
		ref = caller
	}
	return e.st.FindWorker(ref)
}

func (e *Engine) opWorkerShow(ctx context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[WorkerRef](raw)
	if err != nil {
		return nil, err
	}
	w, err := e.findWorker(caller, a.Worker)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"worker": w}
	if d, err := e.st.ActiveDispatchForPane(w.PaneID); err == nil {
		out["dispatch"] = d
	}
	if p, err := e.h.PaneGet(ctx, w.PaneID); err == nil {
		out["live_status"] = p.AgentStatus
	} else {
		out["live_status"] = "gone"
	}
	return out, nil
}

func (e *Engine) opWorkerRead(ctx context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[WorkerRef](raw)
	if err != nil {
		return nil, err
	}
	pane := a.Worker
	if w, err := e.findWorker(caller, a.Worker); err == nil {
		pane = w.PaneID
	} else if !isPane(pane) {
		return nil, err
	}
	if a.Lines <= 0 {
		a.Lines = 120
	}
	text, err := e.h.PaneRead(ctx, pane, a.Lines)
	if err != nil {
		return nil, err
	}
	return map[string]any{"pane_id": pane, "text": text}, nil
}

type WorkerListArgs struct {
	Run string `json:"run,omitempty"`
	All bool   `json:"all,omitempty"`
}

func (e *Engine) opWorkerList(_ context.Context, _ string, raw json.RawMessage) (any, error) {
	a, err := decode[WorkerListArgs](raw)
	if err != nil {
		return nil, err
	}
	return e.st.ListWorkers(a.Run, !a.All)
}

func (e *Engine) opWorkerRetain(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[WorkerRef](raw)
	if err != nil {
		return nil, err
	}
	w, err := e.findWorker(caller, a.Worker)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.st.SetRetained(w.PaneID, !a.Off); err != nil {
		return nil, err
	}
	_ = e.st.SetFlagged(w.PaneID, 0)
	return e.st.GetWorker(w.PaneID)
}

// opWorkerAbandon fences the worker's dispatch and stops supervising it, without claiming
// anything about its process: the pane is left as it is.
func (e *Engine) opWorkerAbandon(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[WorkerRef](raw)
	if err != nil {
		return nil, err
	}
	w, err := e.findWorker(caller, a.Worker)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if d, err := e.st.ActiveDispatchForPane(w.PaneID); err == nil {
		st, err := e.st.Fence(d.ID, "worker abandoned")
		if err != nil {
			return nil, err
		}
		e.afterFenceLocked(st)
	}
	if err := e.st.SetWorkerState(w.PaneID, store.WorkerAbandoned, "abandoned; pane left running", ""); err != nil {
		return nil, err
	}
	e.badgeTask(w.PaneID, "")
	e.recomputeWatch()
	e.notify.broadcast()
	return map[string]any{"worker": mustWorker(e.st.GetWorker(w.PaneID)), "process": "not checked; the pane was left open"}, nil
}

func mustWorker(w store.Worker, _ error) store.Worker { return w }

func (e *Engine) afterFenceLocked(st store.Settlement) {
	e.messageCoordinatorLocked(st.Dispatch.RunID, store.KindNote, "dispatch "+st.Dispatch.ID+" fenced",
		fmt.Sprintf("Dispatch %s of task %s was fenced (%s); the task is %s again.", st.Dispatch.ID, st.Task.ID, st.Dispatch.Outcome, st.Task.Status), st.Task.ID, "")
	e.autoDispatchLocked(st.Dispatch.RunID)
}

// opWorkerStop fences the active dispatch and closes the pane (verified).
func (e *Engine) opWorkerStop(ctx context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[WorkerRef](raw)
	if err != nil {
		return nil, err
	}
	w, err := e.findWorker(caller, a.Worker)
	if err != nil {
		return nil, err
	}
	if !a.Force {
		if reasons := worktreeUnsaved(w.Worktree); len(reasons) > 0 {
			return nil, e.refuseUnsaved(w, "stop", reasons)
		}
	}
	e.mu.Lock()
	if d, err := e.st.ActiveDispatchForPane(w.PaneID); err == nil {
		if st, err := e.st.Fence(d.ID, "worker stopped"); err == nil {
			e.afterFenceLocked(st)
		}
	}
	e.mu.Unlock()
	return e.closeWorker(ctx, w, store.WorkerStopped, false)
}

// opWorkerRelease archives the worker's output, closes its pane and verifies every process
// in it is gone. Refuses while a dispatch is active or the worktree has unsaved work.
func (e *Engine) opWorkerRelease(ctx context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[WorkerRef](raw)
	if err != nil {
		return nil, err
	}
	w, err := e.findWorker(caller, a.Worker)
	if err != nil {
		return nil, err
	}
	if w.State != store.WorkerLive && w.State != store.WorkerReleaseFailed {
		return nil, refusal("worker_not_live", "worker %s is %s", w.PaneID, w.State)
	}
	if d, err := e.st.ActiveDispatchForPane(w.PaneID); err == nil && !a.Force {
		return nil, refusal("worker_busy", "worker %s has active dispatch %s (task %s); wait for it or use `horch worker stop`", w.PaneID, d.ID, d.TaskID)
	}
	if !a.Force {
		if reasons := worktreeUnsaved(w.Worktree); len(reasons) > 0 {
			return nil, e.refuseUnsaved(w, "release", reasons)
		}
	}
	if d, err := e.st.ActiveDispatchForPane(w.PaneID); err == nil {
		e.mu.Lock()
		if st, err := e.st.Fence(d.ID, "worker released"); err == nil {
			e.afterFenceLocked(st)
		}
		e.mu.Unlock()
	}
	return e.closeWorker(ctx, w, store.WorkerReleased, true)
}

func (e *Engine) refuseUnsaved(w store.Worker, verb string, reasons []string) error {
	e.mu.Lock()
	e.escalateLocked(w.RunID, fmt.Sprintf("refused to %s %s: unsaved work", verb, w.PaneID),
		fmt.Sprintf("Worker %s (%s) has unsaved work in %s:\n- %s\nCommit and push it, or pass --force to discard.", w.PaneID, w.Name, w.Worktree, strings.Join(reasons, "\n- ")), "")
	e.mu.Unlock()
	return refusal("unsaved_work", "%s has unsaved work: %s (use --force to override)", w.Worktree, strings.Join(reasons, "; "))
}

type CloseResult struct {
	Worker      store.Worker `json:"worker"`
	ArchivePath string       `json:"archive_path,omitempty"`
	PIDs        []int        `json:"pids"`
	Killed      []int        `json:"killed,omitempty"`
	Survivors   []int        `json:"survivors,omitempty"`
}

// closeWorker optionally archives the transcript, closes the pane and verifies every
// recorded process is gone, escalating to SIGTERM/SIGKILL for stragglers. It never reports
// success while a process from the pane is still alive.
func (e *Engine) closeWorker(ctx context.Context, w store.Worker, final string, archive bool) (CloseResult, error) {
	res := CloseResult{}
	if archive {
		if text, err := e.h.PaneRead(ctx, w.PaneID, 10000); err == nil && e.cfg.ArchiveDir != "" {
			p := filepath.Join(e.cfg.ArchiveDir, fmt.Sprintf("%s-%s.txt", paths.Safe(w.PaneID), time.Now().Format("20060102-150405")))
			if err := os.WriteFile(p, []byte(text), 0o644); err == nil {
				res.ArchivePath = p
			}
		}
	}
	info, infoErr := e.h.ProcessInfo(ctx, w.PaneID)
	if infoErr == nil {
		res.PIDs = info.PIDs()
	}
	e.mu.Lock()
	e.closing[w.PaneID] = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.closing, w.PaneID)
		e.mu.Unlock()
	}()
	closeErr := e.h.PaneClose(ctx, w.PaneID)
	if closeErr != nil && infoErr != nil {
		// Pane already gone and we never saw its processes: nothing left to verify.
		res.PIDs = nil
	}
	res.Survivors = waitGone(res.PIDs, 5*time.Second)
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if len(res.Survivors) == 0 {
			break
		}
		for _, pid := range res.Survivors {
			if syscall.Kill(pid, sig) == nil {
				res.Killed = append(res.Killed, pid)
			}
		}
		res.Survivors = waitGone(res.Survivors, 2*time.Second)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.status, w.PaneID)
	if len(res.Survivors) > 0 {
		_ = e.st.SetWorkerState(w.PaneID, store.WorkerReleaseFailed, fmt.Sprintf("processes still alive: %v", res.Survivors), res.ArchivePath)
		res.Worker, _ = e.st.GetWorker(w.PaneID)
		e.escalateLocked(w.RunID, fmt.Sprintf("worker %s: processes survived close", w.PaneID),
			fmt.Sprintf("After closing pane %s, processes %v are still alive even after SIGKILL. Check them with `ps -p`.", w.PaneID, res.Survivors), "")
		e.notify.broadcast()
		return res, refusal("release_failed", "pane %s closed but processes %v are still alive", w.PaneID, res.Survivors)
	}
	if closeErr != nil && infoErr == nil {
		// Close failed but the processes we saw are gone.
		e.logf("close %s: %v (processes gone)", w.PaneID, closeErr)
	}
	note := fmt.Sprintf("closed; %d process(es) verified gone", len(res.PIDs))
	if infoErr != nil && closeErr != nil {
		note = "pane already gone"
	}
	_ = e.st.SetWorkerState(w.PaneID, final, note, res.ArchivePath)
	res.Worker, _ = e.st.GetWorker(w.PaneID)
	e.badgeTask(w.PaneID, "")
	e.recomputeWatch()
	e.notify.broadcast()
	return res, nil
}

// waitGone polls until every pid has exited or the timeout passes; it returns survivors.
func waitGone(pids []int, timeout time.Duration) []int {
	deadline := time.Now().Add(timeout)
	for {
		var alive []int
		for _, pid := range pids {
			if pidAlive(pid) {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 || time.Now().After(deadline) {
			return alive
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// worktreeUnsaved reports uncommitted changes and commits not on any remote. Paths that
// are not git checkouts (or no longer exist) report nothing.
func worktreeUnsaved(dir string) []string {
	if dir == "" {
		return nil
	}
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	git := func(args ...string) (string, bool) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err == nil
	}
	if out, ok := git("rev-parse", "--is-inside-work-tree"); !ok || out != "true" {
		return nil
	}
	var reasons []string
	if out, ok := git("status", "--porcelain"); ok && out != "" {
		n := len(strings.Split(out, "\n"))
		reasons = append(reasons, fmt.Sprintf("%d uncommitted change(s)", n))
	}
	if remotes, _ := git("remote"); remotes != "" {
		if out, ok := git("rev-list", "--count", "HEAD", "--not", "--remotes"); ok && out != "" && out != "0" {
			reasons = append(reasons, out+" commit(s) not pushed to any remote")
		}
	}
	return reasons
}

// Sweep reconciles registered workers with herdr: workers whose pane disappeared are
// marked exited (their dispatch fails), idle workers without a dispatch are flagged once,
// release_failed workers are retried, and released workers whose pane still exists are
// closed again.
func (e *Engine) Sweep(ctx context.Context) {
	ws, err := e.st.ListWorkers("", false)
	if err != nil {
		return
	}
	now := e.st.Now()
	for _, w := range ws {
		switch w.State {
		case store.WorkerLive:
			p, err := e.h.PaneGet(ctx, w.PaneID)
			if err != nil {
				if herdr.ErrCode(err) != "" {
					e.paneGone(w.PaneID, "gone")
				}
				continue
			}
			e.setStatus(w.PaneID, p.AgentStatus)
			if w.Worktree != "" && w.FlaggedAt == 0 {
				if _, err := os.Stat(w.Worktree); os.IsNotExist(err) {
					e.mu.Lock()
					_ = e.st.SetFlagged(w.PaneID, now.UnixMilli())
					e.escalateLocked(w.RunID, fmt.Sprintf("worktree missing under live worker %s", w.PaneID),
						fmt.Sprintf("Worker %s (%s) runs in %s, which no longer exists.", w.PaneID, w.Name, w.Worktree), "")
					e.mu.Unlock()
					continue
				}
			}
			if w.Retained || w.FlaggedAt != 0 {
				continue
			}
			// Opt-in per run (`horch run update --flag-idle 30m`).
			r, err := e.st.GetRun(w.RunID)
			if err != nil || r.IdleFlagMS <= 0 {
				continue
			}
			flagAfter := time.Duration(r.IdleFlagMS) * time.Millisecond
			if _, err := e.st.ActiveDispatchForPane(w.PaneID); err == nil {
				continue
			}
			if now.Sub(time.UnixMilli(w.StartedAt)) < flagAfter {
				continue
			}
			if last := e.lastSettled(w.PaneID); last > 0 && now.Sub(time.UnixMilli(last)) < flagAfter {
				continue
			}
			e.mu.Lock()
			_ = e.st.SetFlagged(w.PaneID, now.UnixMilli())
			e.messageCoordinatorLocked(w.RunID, store.KindNote, fmt.Sprintf("worker %s idle without a dispatch", w.PaneID),
				fmt.Sprintf("Worker %s (%s) has had no dispatch for over %s. Dispatch to it, `horch worker release --worker %s`, or `horch worker retain --worker %s`.", w.PaneID, w.Name, flagAfter, w.PaneID, w.PaneID), "", "")
			e.notify.broadcast()
			e.mu.Unlock()
		case store.WorkerReleaseFailed:
			if _, err := e.closeWorker(ctx, w, store.WorkerReleased, false); err != nil {
				e.logf("sweep: retry release %s: %v", w.PaneID, err)
			}
		case store.WorkerReleased, store.WorkerStopped:
			if now.Sub(time.UnixMilli(w.ReleasedAt)) > 24*time.Hour {
				continue
			}
			if _, err := e.h.PaneGet(ctx, w.PaneID); err == nil {
				e.logf("sweep: %s is %s but its pane is still open; closing", w.PaneID, w.State)
				if _, err := e.closeWorker(ctx, w, w.State, false); err != nil {
					e.logf("sweep: close %s: %v", w.PaneID, err)
				}
			}
		}
	}
}

func (e *Engine) lastSettled(pane string) int64 {
	t, _ := e.st.LastSettledForPane(pane)
	return t
}

// awaitLateStart polls for an agent named name in pane to become interactive or blocked,
// for up to startGrace after herdr's own start wait timed out.
func (e *Engine) awaitLateStart(ctx context.Context, pane, name string) (herdr.Pane, bool) {
	deadline := time.Now().Add(e.startGrace)
	for {
		if a, err := e.h.AgentGet(ctx, pane); err == nil && a.Name != nil && *a.Name == name {
			ready := a.InteractiveReady != nil && *a.InteractiveReady
			if ready || a.AgentStatus == "blocked" {
				return a, true
			}
		}
		if time.Now().After(deadline) {
			return herdr.Pane{}, false
		}
		select {
		case <-ctx.Done():
			return herdr.Pane{}, false
		case <-time.After(e.promptRetry):
		}
	}
}
