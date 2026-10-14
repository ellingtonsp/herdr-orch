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
	Run   string `json:"run,omitempty"`
	Agent string `json:"agent"`
	Name  string `json:"name,omitempty"`
	// Cwd preserves the legacy new-tab-in-caller-workspace behavior.
	Cwd string `json:"cwd,omitempty"`
	// Worktree finds or opens the checkout's own workspace and selects a shell there.
	Worktree string   `json:"worktree,omitempty"`
	Args     []string `json:"args,omitempty"`
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

// startWorker selects a shell in the requested worktree workspace, adopts an explicit
// pane, or opens a tab in the caller's workspace. An agent that comes up blocked
// (e.g. a folder-trust prompt) is registered and escalated, never answered automatically.
func (e *Engine) startWorker(ctx context.Context, caller string, a WorkerStartArgs) (store.Worker, error) {
	e.processMu.Lock()
	defer e.processMu.Unlock()
	if a.Agent == "" {
		return store.Worker{}, refusal("bad_args", "--agent is required (claude|pi|codex|...)")
	}
	run, err := e.runFor(caller, a.Run)
	if err != nil {
		return store.Worker{}, err
	}
	if a.Worktree != "" {
		if a.Cwd != "" || a.Pane != "" {
			return store.Worker{}, refusal("bad_args", "--worktree cannot be combined with --cwd or --pane")
		}
		a.Cwd = a.Worktree
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
	if a.Cwd != "" {
		a.Cwd, err = processRoot(a.Cwd)
		if err != nil {
			return store.Worker{}, refusal("bad_cwd", "%v", err)
		}
		release, err := e.reserveStarting(a.Cwd)
		if err != nil {
			return store.Worker{}, err
		}
		defer release()
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
		if p.WorkspaceID == "" {
			return store.Worker{}, refusal("unknown_pane", "pane %s has no workspace; choose a shell pane in a workspace", a.Pane)
		}
		if p.Agent != nil && *p.Agent != "" {
			return store.Worker{}, refusal("pane_busy", "pane %s already runs %s", a.Pane, *p.Agent)
		}
		if a.Cwd != "" {
			if p.Cwd == nil {
				return store.Worker{}, refusal("bad_cwd", "cannot verify existing pane cwd")
			}
			actual, err := processRoot(*p.Cwd)
			if err != nil {
				return store.Worker{}, err
			}
			if actual != a.Cwd {
				return store.Worker{}, refusal("cwd_mismatch", "existing pane cwd differs from --worktree; change the pane directory first")
			}
		}
		info, err := e.h.ProcessInfo(ctx, a.Pane)
		if err != nil || !info.AvailableShell() {
			return store.Worker{}, refusal("pane_busy", "pane %s is not an available shell", a.Pane)
		}
		if a.Cwd == "" && p.Cwd != nil {
			a.Cwd, err = processRoot(*p.Cwd)
			if err != nil {
				return store.Worker{}, refusal("bad_cwd", "%v", err)
			}
			release, err := e.reserveStarting(a.Cwd)
			if err != nil {
				return store.Worker{}, err
			}
			defer release()
		}
		pane = p
	} else {
		ws := ""
		if isPane(caller) && a.Worktree == "" {
			if p, err := e.h.PaneGet(ctx, caller); err == nil {
				if p.WorkspaceID == "" {
					return store.Worker{}, refusal("unknown_pane", "caller pane %s has no workspace", caller)
				}
				ws = p.WorkspaceID
			} else {
				return store.Worker{}, refusal("unknown_pane", "caller pane %s: %v", caller, err)
			}
		}
		var env map[string]string
		if e.cfg.HorchBin != "" {
			// Workers must be able to run `horch done` whatever their shell profile says.
			env = map[string]string{"PATH": filepath.Dir(e.cfg.HorchBin) + ":" + os.Getenv("PATH")}
		}
		var err error
		if a.Worktree != "" {
			pane, ownPane, err = e.h.WorktreePane(ctx, a.Cwd, a.Name, env)
		} else {
			pane, err = e.h.NewTab(ctx, ws, a.Cwd, a.Name, env)
		}
		if err != nil {
			if ownPane && pane.PaneID != "" {
				cctx, cancel := bg()
				_ = e.h.PaneClose(cctx, pane.PaneID)
				cancel()
			}
			return store.Worker{}, fmt.Errorf("create tab: %w", err)
		}
	}
	// New tabs may inherit cwd when none was requested. Capture the actual
	// pane cwd while launch/cleanup serialization protects the unregistered pane.
	if ownPane && a.Cwd == "" {
		live := pane
		if live.Cwd == nil {
			if p, err := e.h.PaneGet(ctx, pane.PaneID); err == nil {
				live = p
			}
		}
		if live.Cwd != nil {
			a.Cwd, err = processRoot(*live.Cwd)
			if err != nil {
				return store.Worker{}, refusal("bad_cwd", "%v", err)
			}
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
	if a.Cwd != "" {
		a.Cwd, err = processRoot(a.Cwd)
		if err != nil {
			return store.Worker{}, refusal("bad_cwd", "%v", err)
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
	e.processMu.Lock()
	defer e.processMu.Unlock()
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
	if a.Worktree != "" {
		a.Worktree, err = processRoot(a.Worktree)
		if err != nil {
			return nil, refusal("bad_cwd", "%v", err)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing[a.Pane] {
		return nil, refusal("worker_closing", "worker %s is closing", a.Pane)
	}
	for pane := range e.closing {
		other, err := e.st.GetWorker(pane)
		if err != nil {
			return nil, err
		}
		root, err := processRoot(other.Worktree)
		if err != nil {
			return nil, err
		}
		if root != "" && a.Worktree != "" && (insideWorktree(root, a.Worktree) || insideWorktree(a.Worktree, root)) {
			return nil, refusal("worktree_in_use", "worker %s is closing", pane)
		}
	}
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
	return e.closeWorker(ctx, w, store.WorkerStopped, false, true)
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

	if !a.Force {
		if reasons := worktreeUnsaved(w.Worktree); len(reasons) > 0 {
			return nil, e.refuseUnsaved(w, "release", reasons)
		}
	}
	return e.closeWorker(ctx, w, store.WorkerReleased, true, a.Force)
}

func (e *Engine) refuseUnsaved(w store.Worker, verb string, reasons []string) error {
	e.mu.Lock()
	e.escalateLocked(w.RunID, fmt.Sprintf("refused to %s %s: unsaved work", verb, w.PaneID),
		fmt.Sprintf("Worker %s (%s) has unsaved work in %s:\n- %s\nCommit and push it, or pass --force to discard.", w.PaneID, w.Name, w.Worktree, strings.Join(reasons, "\n- ")), "")
	e.mu.Unlock()
	return refusal("unsaved_work", "%s has unsaved work: %s (use --force to override)", w.Worktree, strings.Join(reasons, "; "))
}

type CloseResult struct {
	Processes   []ProcessOutcome `json:"processes,omitempty"`
	Worker      store.Worker     `json:"worker"`
	ArchivePath string           `json:"archive_path,omitempty"`
	PIDs        []int            `json:"pids"`
	Killed      []int            `json:"killed,omitempty"`
	Survivors   []int            `json:"survivors,omitempty"`
}

// closeWorker optionally archives the transcript, closes the pane and verifies every
// recorded process is gone, escalating to SIGTERM/SIGKILL for stragglers. It never reports
// success while a process from the pane is still alive.
func (e *Engine) closeWorker(ctx context.Context, w store.Worker, final string, archive bool, force ...bool) (CloseResult, error) {
	e.processMu.Lock()
	defer e.processMu.Unlock()
	res := CloseResult{Worker: w}
	root, rootErr := processRoot(w.Worktree)
	if rootErr != nil {
		return res, refusal("process_scan_failed", "%v", rootErr)
	}
	e.mu.Lock()
	current, currentErr := e.st.GetWorker(w.PaneID)
	if currentErr != nil {
		e.mu.Unlock()
		return res, currentErr
	}
	if current.RunID != w.RunID || current.Worktree != w.Worktree || current.Name != w.Name || current.State != w.State || current.StartedAt != w.StartedAt {
		e.mu.Unlock()
		return res, refusal("worker_changed", "worker registration changed; retry cleanup")
	}
	if root != "" {
		for r, n := range e.starting {
			if n > 0 && (insideWorktree(root, r) || insideWorktree(r, root)) {
				e.mu.Unlock()
				return res, refusal("worktree_in_use", "a worker is starting in %s", r)
			}
		}
		ws, err := e.st.ListWorkers("", true)
		if err != nil {
			e.mu.Unlock()
			return res, err
		}
		for _, other := range ws {
			if other.PaneID == w.PaneID {
				continue
			}
			r, err := processRoot(other.Worktree)
			if err != nil {
				e.mu.Unlock()
				return res, err
			}
			if r != "" && (insideWorktree(root, r) || insideWorktree(r, root)) {
				e.mu.Unlock()
				return res, refusal("worktree_in_use", "worktree overlaps live worker %s (%s)", other.Name, other.PaneID)
			}
		}
	}
	if e.closing[w.PaneID] {
		e.mu.Unlock()
		return res, refusal("worker_closing", "worker %s is already closing", w.PaneID)
	}
	if d, err := e.st.ActiveDispatchForPane(w.PaneID); err == nil {
		if d.Status == store.DispatchPending || (final == store.WorkerReleased && (len(force) == 0 || !force[0])) {
			e.mu.Unlock()
			return res, refusal("worker_busy", "worker %s has active dispatch %s", w.PaneID, d.ID)
		}
	}
	e.closing[w.PaneID] = true
	defer func() { e.mu.Lock(); delete(e.closing, w.PaneID); e.mu.Unlock() }()
	if d, err := e.st.ActiveDispatchForPane(w.PaneID); err == nil {
		st, err := e.st.Fence(d.ID, "worker closed")
		if err != nil {
			e.mu.Unlock()
			return res, err
		}
		e.afterFenceLocked(st)
	}
	e.mu.Unlock()
	if archive {
		if text, err := e.h.PaneRead(ctx, w.PaneID, 10000); err == nil && e.cfg.ArchiveDir != "" {
			p := filepath.Join(e.cfg.ArchiveDir, fmt.Sprintf("%s-%s.txt", paths.Safe(w.PaneID), time.Now().Format("20060102-150405")))
			if err := os.WriteFile(p, []byte(text), 0o644); err == nil {
				res.ArchivePath = p
			}
		}
	}
	var scanErr error
	host := e.processHost()
	var snapshot []ProcessRecord
	if scanErr == nil && root != "" {
		snapshot, scanErr = host.List(ctx)
	}
	if scanErr != nil {
		return res, refusal("process_scan_failed", "cannot inspect worktree processes: %v", scanErr)
	}
	info, infoErr := e.h.ProcessInfo(ctx, w.PaneID)
	if infoErr != nil && herdr.ErrCode(infoErr) != "pane_not_found" {
		return res, refusal("process_scan_failed", "cannot inspect pane before closing: %v", infoErr)
	}
	paneProcesses := map[int]ProcessRecord{}
	if infoErr == nil {
		res.PIDs = info.PIDs()
		for _, pid := range res.PIDs {
			if p, err := host.Inspect(ctx, pid); err == nil {
				paneProcesses[pid] = p
			} else if !os.IsNotExist(err) {
				scanErr = err
			}

		}
	}
	for _, p := range paneProcesses {
		if protectedProcess(p, processAncestors(snapshot)) {
			return res, refusal("protected_process", "pane contains protected process %d (%s)", p.PID, p.Command)
		}
	}
	if scanErr != nil {
		return res, refusal("process_scan_failed", "cannot verify pane processes: %v", scanErr)
	}
	closeErr := e.h.PaneClose(ctx, w.PaneID)
	if closeErr != nil && herdr.ErrCode(closeErr) != "pane_not_found" {
		scanErr = fmt.Errorf("pane close was not verified: %w", closeErr)
	}
	if root != "" {
		// Include helpers forked between the first inventory and pane closure.
		after, err := host.List(ctx)
		if err != nil {
			scanErr = err
		} else {
			seen := map[int]bool{}
			for _, p := range snapshot {
				seen[p.PID] = true
			}
			for _, p := range after {
				if !seen[p.PID] {
					snapshot = append(snapshot, p)
				}
			}
		}
		var cleanupErr error
		res.Processes, cleanupErr = e.cleanProcessScope(ctx, root, snapshot, false)
		if cleanupErr != nil {
			scanErr = cleanupErr
		}
	}

	seen := map[int]bool{}
	for _, pid := range res.PIDs {
		seen[pid] = true
	}
	for _, p := range res.Processes {
		if !seen[p.PID] {
			res.PIDs = append(res.PIDs, p.PID)
			seen[p.PID] = true
		}
		switch p.Action {
		case "terminated", "killed":
			res.Killed = append(res.Killed, p.PID)
		case "term", "kill", "unverified":
			res.Survivors = append(res.Survivors, p.PID)
		}
	}
	// Pane processes outside the worktree are verified but never directly signalled.
	// Use their start identity so a recycled PID cannot fail or broaden cleanup.
	for pid, old := range paneProcesses {
		p, err := host.Inspect(ctx, pid)
		if err == nil && p.Started == old.Started && !protectedProcess(p, processAncestors(snapshot)) {
			found := false
			for _, id := range res.Survivors {
				if id == pid {
					found = true
				}
			}
			if !found {
				res.Survivors = append(res.Survivors, pid)
			}
		} else if err != nil && !os.IsNotExist(err) {
			scanErr = err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.status, w.PaneID)
	if len(res.Survivors) > 0 || scanErr != nil {
		_ = e.st.SetWorkerState(w.PaneID, store.WorkerReleaseFailed, fmt.Sprintf("processes still alive: %v; inventory error: %v", res.Survivors, scanErr), res.ArchivePath)
		res.Worker, _ = e.st.GetWorker(w.PaneID)
		e.escalateLocked(w.RunID, fmt.Sprintf("worker %s: processes survived close", w.PaneID),
			fmt.Sprintf("After closing pane %s, processes %v are still alive even after SIGKILL. Check them with `ps -p`.", w.PaneID, res.Survivors), "")
		e.notify.broadcast()
		return res, refusal("release_failed", "pane %s cleanup incomplete; survivors %v; inventory error: %v", w.PaneID, res.Survivors, scanErr)
	}
	if closeErr != nil && infoErr == nil {
		// Close failed but the processes we saw are gone.
		e.logf("close %s: %v (processes gone)", w.PaneID, closeErr)
	}
	note := fmt.Sprintf("closed; %d process(es) observed; cleanup verified for eligible worktree processes", len(res.PIDs))
	if infoErr != nil && closeErr != nil {
		note = "pane already gone"
	}
	var stateErr error
	if final == store.WorkerReleased {
		stateErr = e.st.ReleaseWorkerWithLeases(w.PaneID, note, res.ArchivePath)
	} else {
		stateErr = e.st.SetWorkerState(w.PaneID, final, note, res.ArchivePath)
	}
	if stateErr != nil {
		return res, stateErr
	}
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
