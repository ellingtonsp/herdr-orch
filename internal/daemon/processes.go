package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

// ProcessRecord is a process identity, not just a reusable PID.
type ProcessRecord struct {
	PID     int    `json:"pid"`
	Parent  int    `json:"-"`
	Started string `json:"-"`
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
}
type ProcessOutcome struct {
	ProcessRecord
	Action string `json:"action"`
	Error  string `json:"error,omitempty"`
}
type processHost interface {
	List(context.Context) ([]ProcessRecord, error)
	Inspect(context.Context, int) (ProcessRecord, error)
	Signal(int, syscall.Signal) error
}
type nativeProcesses struct{}

func (nativeProcesses) Signal(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

// Resolve existing ancestors too: a removed worktree under /var must still match
// the kernel's /private/var spelling on macOS. Never accept a relative or root scope.
func processRoot(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("worktree must be absolute: %s", path)
	}
	path = filepath.Clean(path)
	if path == string(filepath.Separator) {
		return "", fmt.Errorf("filesystem root is not a worktree scope")
	}
	parent := path
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			if resolved == string(filepath.Separator) {
				return "", fmt.Errorf("filesystem root is not a worktree scope")
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", err
		}
		tail = append(tail, filepath.Base(parent))
		parent = next
	}
}
func insideWorktree(root, cwd string) bool {
	if root == "" || cwd == "" || !filepath.IsAbs(cwd) {
		return false
	}
	rel, err := filepath.Rel(root, filepath.Clean(cwd))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func protectedProcess(p ProcessRecord, ancestors map[int]bool) bool {
	name := strings.ToLower(filepath.Base(p.Command))
	return p.PID <= 1 || ancestors[p.PID] || strings.HasPrefix(name, "herdr") || strings.HasPrefix(name, "horch")
}
func processAncestors(ps []ProcessRecord) map[int]bool {
	byPID := map[int]int{}
	for _, p := range ps {
		byPID[p.PID] = p.Parent
	}
	out := map[int]bool{}
	for pid := os.Getpid(); pid > 1 && !out[pid]; pid = byPID[pid] {
		out[pid] = true
	}
	return out
}
func (e *Engine) processHost() processHost {
	if e.processes != nil {
		return e.processes
	}
	return nativeProcesses{}
}
func (e *Engine) processWait() time.Duration {
	if e.processGrace > 0 {
		return e.processGrace
	}
	return 2 * time.Second
}

// cleanProcesses rechecks cwd, executable and start identity before each signal.
// A process that moves out of scope or becomes protected is never signalled.
func (e *Engine) cleanProcesses(ctx context.Context, root string, ps []ProcessRecord, dry bool, guards ...func(ProcessRecord) bool) []ProcessOutcome {
	allowed := func(p ProcessRecord) bool {
		for _, guard := range guards {
			if !guard(p) {
				return false
			}
		}
		return true
	}
	host := e.processHost()
	ancestors := processAncestors(ps)
	out := []ProcessOutcome{}
	for _, p := range ps {
		if !insideWorktree(root, p.Cwd) || !allowed(p) {
			continue
		}
		r := ProcessOutcome{ProcessRecord: p, Action: "would_stop"}
		if protectedProcess(p, ancestors) {
			r.Action = "protected"
		}
		out = append(out, r)
	}
	if dry {
		return out
	}
	remaining := func(r *ProcessOutcome) bool {
		p, err := host.Inspect(ctx, r.PID)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			r.Action = "exited"
			return false
		}
		if err != nil {
			r.Action = "unverified"
			r.Error = err.Error()
			return false
		}
		if p.Started != r.Started {
			r.Action = "exited"
			return false
		}
		if !insideWorktree(root, p.Cwd) || !allowed(p) {
			r.Action = "left_worktree"
			return false
		}
		if protectedProcess(p, ancestors) {
			r.Action = "protected"
			return false
		}
		return true
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		for i := range out {
			r := &out[i]
			if r.Action != "would_stop" && r.Action != "term" && r.Action != "kill" {
				continue
			}
			if !remaining(r) {
				continue
			}
			if err := host.Signal(r.PID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
				r.Error = err.Error()
			}
			if sig == syscall.SIGTERM {
				r.Action = "term"
			} else {
				r.Action = "kill"
			}
		}
		deadline := time.Now().Add(e.processWait())
		for {
			alive := false
			for i := range out {
				r := &out[i]
				if r.Action != "term" && r.Action != "kill" {
					continue
				}
				action := r.Action
				if remaining(r) {
					alive = true
				} else if r.Action == "exited" {
					if action == "term" {
						r.Action = "terminated"
					} else {
						r.Action = "killed"
					}
					r.Error = ""
				}
			}
			if !alive || time.Now().After(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				return out
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	return out
}

type SweepArgs struct {
	DryRun bool `json:"dry_run,omitempty"`
}
type SweepResult struct {
	DryRun    bool             `json:"dry_run"`
	Processes []ProcessOutcome `json:"processes"`
}

func (e *Engine) opProcessSweep(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
	e.processMu.Lock()
	defer e.processMu.Unlock()
	a, err := decode[SweepArgs](raw)
	if err != nil {
		return nil, err
	}
	// Serialize with acquisition/dispatch and worker registration so a released
	// path cannot be reassigned while we are cleaning it.
	e.mu.Lock()
	defer e.mu.Unlock()
	ws, err := e.st.ListWorkers("", false)
	if err != nil {
		return nil, err
	}
	roots := map[string]bool{}
	active := []string{}
	for root, n := range e.starting {
		if n > 0 {
			active = append(active, root)
		}
	}
	for _, w := range ws {
		root, err := processRoot(w.Worktree)
		if err != nil {
			return nil, refusal("process_scan_failed", "%v", err)
		}
		if root == "" {
			continue
		}
		_, statErr := os.Stat(root)
		removed := os.IsNotExist(statErr)
		if statErr != nil && !removed {
			return nil, refusal("process_scan_failed", "%v", statErr)
		}
		if w.State == store.WorkerReleased || w.State == store.WorkerStopped || removed {
			roots[root] = true
		} else if w.State == store.WorkerLive || w.State == store.WorkerReleaseFailed {
			active = append(active, root)
		}
	}
	ps, err := e.processHost().List(ctx)
	if err != nil {
		return nil, refusal("process_scan_failed", "%v", err)
	}
	res := SweepResult{DryRun: a.DryRun, Processes: []ProcessOutcome{}}
	ordered := []string{}
	for r := range roots {
		ordered = append(ordered, r)
	}
	sort.Strings(ordered)
	owner := func(p ProcessRecord) string {
		for _, r := range active {
			if insideWorktree(r, p.Cwd) {
				return ""
			}
		}
		for _, r := range ordered {
			if insideWorktree(r, p.Cwd) {
				return r
			}
		}
		return ""
	}
	for _, root := range ordered {
		guard := func(p ProcessRecord) bool { return owner(p) == root }
		outcomes, err := e.cleanProcessScope(ctx, root, ps, a.DryRun, guard)
		res.Processes = append(res.Processes, outcomes...)
		if err != nil {
			return res, refusal("process_scan_failed", "%v", err)
		}
	}

	for _, p := range res.Processes {
		if p.Action == "term" || p.Action == "kill" || p.Action == "unverified" {
			return res, refusal("sweep_failed", "process %d: %s %s", p.PID, p.Action, p.Error)
		}
	}
	return res, nil
}

// reserveStarting protects a newly launched pane until its worker row is registered.
func (e *Engine) reserveStarting(root string) (func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for pane := range e.closing {
		w, err := e.st.GetWorker(pane)
		if err != nil {
			return nil, err
		}
		r, err := processRoot(w.Worktree)
		if err != nil {
			return nil, err
		}
		if r != "" && (insideWorktree(r, root) || insideWorktree(root, r)) {
			return nil, refusal("worktree_in_use", "worker %s is closing in %s", pane, r)
		}
	}
	if e.starting == nil {
		e.starting = map[string]int{}
	}
	e.starting[root]++
	return func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.starting[root]--
		if e.starting[root] == 0 {
			delete(e.starting, root)
		}
	}, nil
}

// cleanProcessScope follows late children and processes that return into scope.
// It keeps each process's latest outcome and bounds continuously respawning work.
func (e *Engine) cleanProcessScope(ctx context.Context, root string, ps []ProcessRecord, dry bool, guards ...func(ProcessRecord) bool) ([]ProcessOutcome, error) {
	out := e.cleanProcesses(ctx, root, ps, dry, guards...)
	if dry {
		return out, nil
	}
	allowed := func(p ProcessRecord) bool {
		for _, g := range guards {
			if !g(p) {
				return false
			}
		}
		return true
	}
	for round := 0; round < 3; round++ {
		scan, err := e.processHost().List(ctx)
		if err != nil {
			return out, err
		}
		ancestors := processAncestors(scan)
		live := map[string]ProcessRecord{}
		key := func(p ProcessRecord) string { return fmt.Sprintf("%d:%s", p.PID, p.Started) }
		for _, p := range scan {
			live[key(p)] = p
		}
		for i := range out {
			r := &out[i]
			if _, ok := live[key(r.ProcessRecord)]; !ok && (r.Action == "unverified" || r.Action == "term" || r.Action == "kill") {
				r.Action = "exited"
				r.Error = ""
			}
		}
		todo := map[string]bool{}
		for _, p := range scan {
			if !insideWorktree(root, p.Cwd) || !allowed(p) || protectedProcess(p, ancestors) {
				continue
			}
			// Persisting TERM/KILL/unreadable survivors already fail the operation.
			pending := false
			for _, r := range out {
				if key(r.ProcessRecord) == key(p) && (r.Action == "term" || r.Action == "kill" || r.Action == "unverified") {
					pending = true
					break
				}
			}
			if !pending {
				todo[key(p)] = true
			}
		}
		if len(todo) == 0 {
			return out, nil
		}
		if round == 2 {
			for _, p := range scan {
				if todo[key(p)] {
					out = replaceOutcome(out, ProcessOutcome{ProcessRecord: p, Action: "unverified", Error: "process remains after bounded cleanup; retry"})
				}
			}
			return out, nil
		}
		retryGuard := func(p ProcessRecord) bool { return todo[key(p)] && allowed(p) }
		for _, r := range e.cleanProcesses(ctx, root, scan, false, retryGuard) {
			out = replaceOutcome(out, r)
		}
	}
	return out, nil
}
func replaceOutcome(out []ProcessOutcome, r ProcessOutcome) []ProcessOutcome {
	for i, old := range out {
		if old.PID == r.PID && old.Started == r.Started {
			out[i] = r
			return out
		}
	}
	return append(out, r)
}
