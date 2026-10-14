package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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
		info, err := e.h.ProcessInfo(ctx, a.Pane)
		if err != nil || !info.AvailableShell() {
			return store.Worker{}, refusal("pane_busy", "pane %s is not an available shell", a.Pane)
		}
		if a.Cwd == "" && p.Cwd != nil {
			a.Cwd = *p.Cwd
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
	w, err := e.st.RegisterWorker(store.Worker{PaneID: pane.PaneID, RunID: run.ID, Name: a.Name, Agent: a.Agent, Worktree: a.Cwd, AgentArgs: a.Args, LaunchKnown: true})
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
	Worker    string `json:"worker"`
	Force     bool   `json:"force,omitempty"`
	Lines     int    `json:"lines,omitempty"`
	Off       bool   `json:"off,omitempty"`
	HandOffTo string `json:"hand_off_to,omitempty"`
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
	w, err = e.st.GetWorker(w.PaneID)
	if err != nil {
		return nil, err
	}
	restorePhase := w.Lifecycle
	if err := e.st.BeginWorkerLifecycle(w.PaneID, "abandoning"); err != nil {
		return nil, err
	}
	defer func() { e.finishWorkerLifecycleLocked(w.PaneID, restorePhase) }()
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
	restorePhase = ""
	e.badgeTask(w.PaneID, "")
	e.recomputeWatch()
	e.notify.broadcast()
	return map[string]any{"worker": mustWorker(e.st.GetWorker(w.PaneID)), "process": "not checked; the pane was left open"}, nil
}

func mustWorker(w store.Worker, _ error) store.Worker { return w }

// finishWorkerLifecycleLocked restores the reservation and replays the latest held
// status through normal bookkeeping without letting a newer event race the replay.
func (e *Engine) finishWorkerLifecycleLocked(pane, restore string) {
	if err := e.st.SetWorkerLifecycle(pane, restore); err != nil {
		return
	}
	if restore == "" {
		status := e.status[pane]
		delete(e.status, pane)
		if status != "" {
			e.setStatusLocked(pane, status)
		}
	}
}

func (e *Engine) finishWorkerLifecycle(pane, restore string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.finishWorkerLifecycleLocked(pane, restore)
}

type WorkerRestartArgs struct {
	Worker string   `json:"worker"`
	Env    []string `json:"env,omitempty"`
	Unset  []string `json:"unset,omitempty"`
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// restartEnvironment builds a private shell script which emits only a nonce after all
// requested mutations are verified. The script is sourced by path, never typed into
// the terminal, so values and their encodings stay out of transcripts and shell history.
func restartEnvironment(a WorkerRestartArgs, nonce string) (string, error) {
	seen := map[string]bool{}
	parts := []string{"set +x"}
	octal := func(s string) string {
		var b strings.Builder
		for _, c := range []byte(s) {
			fmt.Fprintf(&b, "\\%03o", c)
		}
		return b.String()
	}
	for _, pair := range a.Env {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || !envName.MatchString(key) || strings.ContainsRune(value, 0) || seen[key] {
			return "", refusal("bad_args", "--env requires unique valid KEY=VAL entries without NUL bytes")
		}
		seen[key] = true
		// A trailing sentinel preserves newlines stripped by command substitution.
		if key == "_horch_value" {
			return "", refusal("bad_args", "reserved restart environment key")
		}
		parts = append(parts, fmt.Sprintf("_horch_value=$(command printf '%%b_' '%s') && _horch_value=${_horch_value%%_} && export %s=\"$_horch_value\" && [ \"${%s}\" = \"$_horch_value\" ]", octal(value), key, key))
	}
	for _, key := range a.Unset {
		if key == "_horch_value" {
			return "", refusal("bad_args", "reserved restart environment key")
		}
		if !envName.MatchString(key) || seen[key] {
			return "", refusal("bad_args", "--unset requires unique valid keys without --env conflicts")
		}
		seen[key] = true
		parts = append(parts, fmt.Sprintf("unset %s && [ \"${%s+x}\" != x ]", key, key))
	}
	parts = append(parts, "unset _horch_value", fmt.Sprintf("command printf '%%s%%s\\n' 'HORCH_RESTART_' '%s'", nonce))
	return " " + strings.Join(parts, " && "), nil
}

func (e *Engine) opWorkerRestart(ctx context.Context, caller string, raw json.RawMessage) (out any, err error) {
	a, err := decode[WorkerRestartArgs](raw)
	if err != nil {
		return nil, err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	nonce := hex.EncodeToString(token[:])
	command, err := restartEnvironment(a, nonce)
	if err != nil {
		return nil, err
	}
	w, err := e.findWorker(caller, a.Worker)
	if err != nil {
		return nil, err
	}
	if w.State != store.WorkerLive {
		return nil, refusal("worker_not_live", "worker %s is %s", w.PaneID, w.State)
	}
	if !w.LaunchKnown || w.Agent == "" || w.Name == "" {
		return nil, refusal("restart_config_unknown", "worker has no persisted launch configuration; start a managed worker first")
	}
	quit := "/exit"
	switch w.Agent {
	case "codex", "claude":
	case "pi":
		quit = "/quit"
	default:
		return nil, refusal("restart_unsupported", "no clean exit command is known for agent %s", w.Agent)
	}
	e.mu.Lock()
	err = e.st.BeginWorkerLifecycle(w.PaneID, "restart")
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	defer func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if err != nil {
			_ = e.st.SetWorkerLifecycle(w.PaneID, "restart_failed")
			e.escalateLocked(w.RunID, "worker restart needs attention", fmt.Sprintf("Restart of worker %s failed. Its task remains claimed and supervision is held. Inspect the pane and retry worker restart or explicitly stop/abandon it.", w.PaneID), "")
		}
		e.notify.broadcast()
	}()
	p, err := e.h.PaneGet(ctx, w.PaneID)
	if err != nil {
		return nil, refusal("restart_failed", "cannot inspect worker pane")
	}
	if p.Agent != nil && *p.Agent != "" {
		if err = e.h.SendText(ctx, w.PaneID, quit); err != nil {
			return nil, refusal("restart_failed", "could not request clean agent exit")
		}
	}
	if err = e.awaitRestartShell(ctx, w.PaneID); err != nil {
		return nil, err
	}
	script, fileErr := os.CreateTemp("", "horch-restart-*")
	if fileErr != nil {
		return nil, refusal("restart_failed", "could not prepare private environment script")
	}
	defer os.Remove(script.Name())
	_, writeErr := script.WriteString(command + "\n")
	closeErr := script.Close()
	if writeErr != nil || closeErr != nil {
		return nil, refusal("restart_failed", "could not prepare private environment script")
	}
	// CreateTemp uses mode 0600. Only the pathname crosses the pane transport.
	source := " . '" + strings.ReplaceAll(script.Name(), "'", "'\\''") + "'"
	if err = e.h.SendText(ctx, w.PaneID, source); err != nil {
		return nil, refusal("restart_failed", "could not apply shell environment")
	}
	if err = e.awaitRestartAck(ctx, w.PaneID, "HORCH_RESTART_"+nonce); err != nil {
		return nil, err
	}
	ag, err := e.h.AgentStart(ctx, w.Name, w.Agent, w.PaneID, w.AgentArgs)
	if herdr.ErrCode(err) == "timeout" {
		if late, ok := e.awaitLateStart(ctx, w.PaneID, w.Name); ok {
			ag, err = late, nil
		}
	}
	if err != nil {
		return nil, refusal("restart_failed", "agent launch failed; inspect the worker pane")
	}
	if ag.AgentStatus == "blocked" || ag.InteractiveReady == nil || !*ag.InteractiveReady {
		return nil, refusal("restart_failed", "restarted agent is not proven interactive")
	}
	e.mu.Lock()
	e.status[w.PaneID] = ag.AgentStatus
	delete(e.blocked, w.PaneID)
	delete(e.blockedTold, w.PaneID)
	d, dErr := e.st.ActiveDispatchForPane(w.PaneID)
	if dErr == nil {
		d, err = e.st.ReplaceWorkerDispatch(d.ID)
	} else if !errors.Is(dErr, store.ErrNotFound) {
		err = dErr
	}
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if dErr == nil {
		t, tErr := e.st.GetTask(d.TaskID)
		if tErr != nil {
			return nil, tErr
		}
		// A named restarted agent must accept the prompt itself; never type it into
		// a possibly returned shell on an ambiguous delivery failure.
		if err = e.h.AgentPrompt(ctx, w.PaneID, buildPrompt(t, d.ID, e.horchCmd(w.PaneID))); err != nil {
			return nil, refusal("restart_failed", "task delivery is unconfirmed; inspect the worker before retrying")
		}
		e.mu.Lock()
		err = e.st.MarkSent(d.ID, "sent after worker restart")
		e.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	e.mu.Lock()
	err = e.st.SetWorkerLifecycle(w.PaneID, "")
	if err == nil {
		status := e.status[w.PaneID]
		delete(e.status, w.PaneID)
		e.setStatusLocked(w.PaneID, status)
		if dErr == nil {
			d, err = e.st.GetDispatch(d.ID)
		}
		e.recomputeWatch()
		out = map[string]any{"worker": mustWorker(e.st.GetWorker(w.PaneID)), "environment_verified": true}
		if dErr == nil {
			out.(map[string]any)["dispatch"] = d
		}
	}
	e.mu.Unlock()
	return out, err
}

func (e *Engine) awaitRestartShell(ctx context.Context, pane string) error {
	deadline := time.Now().Add(e.startGrace)
	for {
		p, pErr := e.h.PaneGet(ctx, pane)
		info, iErr := e.h.ProcessInfo(ctx, pane)
		if pErr == nil && iErr == nil && (p.Agent == nil || *p.Agent == "") && info.ShellPID > 0 {
			ready := true
			for _, proc := range info.ForegroundProcesses {
				if proc.PID != info.ShellPID {
					ready = false
				}
			}
			if ready {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return refusal("restart_failed", "clean agent exit and available shell could not be proven")
		}
		select {
		case <-ctx.Done():
			return refusal("restart_failed", "restart cancelled while waiting for shell")
		case <-time.After(e.promptRetry):
		}
	}
}

func (e *Engine) awaitRestartAck(ctx context.Context, pane, marker string) error {
	deadline := time.Now().Add(e.startGrace)
	for {
		text, err := e.h.PaneRead(ctx, pane, 40)
		if err == nil {
			for _, line := range strings.Split(text, "\n") {
				if strings.TrimSpace(line) == marker {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return refusal("restart_failed", "shell environment verification was not acknowledged")
		}
		select {
		case <-ctx.Done():
			return refusal("restart_failed", "restart cancelled during environment verification")
		case <-time.After(e.promptRetry):
		}
	}
}

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
	e.mu.Lock()
	w, err = e.st.GetWorker(w.PaneID)
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	restorePhase := w.Lifecycle
	if err := e.st.BeginWorkerLifecycle(w.PaneID, "stopping"); err != nil {
		e.mu.Unlock()
		return nil, err
	}
	e.mu.Unlock()
	defer func() { e.finishWorkerLifecycle(w.PaneID, restorePhase) }()
	if !a.Force {
		if reasons := worktreeUnsaved(w.Worktree); len(reasons) > 0 {
			return nil, e.refuseUnsaved(w, "stop", reasons)
		}
	}
	e.mu.Lock()
	if d, err := e.st.ActiveDispatchForPane(w.PaneID); err == nil {
		st, err := e.st.Fence(d.ID, "worker stopped")
		if err != nil {
			e.mu.Unlock()
			return nil, err
		}
		e.afterFenceLocked(st)
	}
	restorePhase = ""
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
	e.mu.Lock()
	w, err = e.st.GetWorker(w.PaneID)
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	restorePhase := w.Lifecycle
	if err := e.st.BeginWorkerLifecycle(w.PaneID, "releasing"); err != nil {
		e.mu.Unlock()
		return nil, err
	}
	e.mu.Unlock()
	defer func() { e.finishWorkerLifecycle(w.PaneID, restorePhase) }()
	if d, err := e.st.ActiveDispatchForPane(w.PaneID); err == nil && !a.Force {
		return nil, refusal("worker_busy", "worker %s has active dispatch %s (task %s); wait for it or use `horch worker stop`", w.PaneID, d.ID, d.TaskID)
	}
	if a.HandOffTo != "" {
		if a.Force {
			return nil, refusal("bad_args", "--hand-off-to cannot be combined with --force")
		}
		target, err := e.findWorker(caller, a.HandOffTo)
		if err != nil {
			return nil, err
		}
		e.mu.Lock()
		if target.PaneID == w.PaneID || target.State != store.WorkerLive || e.st.WorkerLifecycleHeld(target.PaneID) {
			e.mu.Unlock()
			return nil, refusal("invalid_hand_off", "hand-off target must be another live worker")
		}
		// Hold the recipient until the source pane has closed, so it cannot be released
		// concurrently after accepting responsibility.
		if err := e.st.BeginWorkerLifecycle(target.PaneID, "receiving_hand_off"); err != nil {
			e.mu.Unlock()
			return nil, err
		}
		e.mu.Unlock()
		defer e.finishWorkerLifecycle(target.PaneID, "")
		root, err := e.handOffRoot(ctx, w, target)
		if err != nil {
			return nil, err
		}
		e.mu.Lock()
		err = e.st.RecordWorkerHandOff(w.PaneID, target.PaneID, root)
		if err == nil {
			e.messageCoordinatorLocked(w.RunID, store.KindNote, "worker responsibility transferred", fmt.Sprintf("Worker %s transferred responsibility for %s to live worker %s before release.", w.PaneID, root, target.PaneID), "", "")
		}
		e.mu.Unlock()
		if err != nil {
			return nil, err
		}
	} else if !a.Force {
		if reasons := worktreeUnsaved(w.Worktree); len(reasons) > 0 {
			return nil, e.refuseUnsaved(w, "release", reasons)
		}
	}
	if d, err := e.st.ActiveDispatchForPane(w.PaneID); err == nil {
		e.mu.Lock()
		st, err := e.st.Fence(d.ID, "worker released")
		if err != nil {
			e.mu.Unlock()
			return nil, err
		}
		e.afterFenceLocked(st)
		e.mu.Unlock()
	}
	restorePhase = ""
	return e.closeWorker(ctx, w, store.WorkerReleased, true)
}

func actualWorktree(ctx context.Context, h Herdr, w store.Worker) (string, error) {
	p, err := h.PaneGet(ctx, w.PaneID)
	if err != nil || p.Agent == nil || *p.Agent == "" {
		return "", refusal("invalid_hand_off", "worker %s has no live agent", w.PaneID)
	}
	dir := ""
	if p.ForegroundCwd != nil {
		dir = *p.ForegroundCwd
	}
	if dir == "" && p.Cwd != nil {
		dir = *p.Cwd
	}
	if dir == "" {
		return "", refusal("invalid_hand_off", "cannot prove actual worktree for worker %s", w.PaneID)
	}
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", refusal("invalid_hand_off", "cannot prove actual git worktree for worker %s", w.PaneID)
	}
	root, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		return "", refusal("invalid_hand_off", "cannot resolve actual worktree for worker %s", w.PaneID)
	}
	return root, nil
}

func (e *Engine) handOffRoot(ctx context.Context, source, target store.Worker) (string, error) {
	a, err := actualWorktree(ctx, e.h, source)
	if err != nil {
		return "", err
	}
	b, err := actualWorktree(ctx, e.h, target)
	if err != nil {
		return "", err
	}
	if a != b {
		return "", refusal("invalid_hand_off", "workers must share the same actual git worktree")
	}
	return a, nil
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
		if e.st.WorkerLifecycleHeld(w.PaneID) {
			continue
		}
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
