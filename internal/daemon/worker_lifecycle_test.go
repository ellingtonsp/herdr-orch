package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/herdr"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

type restartFake struct {
	*fakeHerdr
	ack         string
	args        []string
	fail        string
	onQuit      func()
	afterPrompt func()
	scriptPath  string
}

func (f *restartFake) SendText(ctx context.Context, pane, text string) error {
	if text == "/exit" || text == "/quit" {
		if f.onQuit != nil {
			f.onQuit()
		}
		if f.fail != "exit" {
			f.mu.Lock()
			f.panes[pane].Agent = nil
			f.mu.Unlock()
		}
		return nil
	}
	if f.fail == "send" {
		return fmt.Errorf("sensitive-test-value")
	}
	// The pane transport receives only the private script path.
	path := strings.TrimSuffix(strings.TrimPrefix(text, " . '"), "'")
	f.scriptPath = path
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm() != 0600 {
		return fmt.Errorf("script permissions must be 0600")
	}
	script, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if f.fail != "verify" {
		f.ack = "HORCH_RESTART_" + regexp.MustCompile(`'([a-f0-9]{32})'$`).FindStringSubmatch(strings.TrimSpace(string(script)))[1]
	}
	return f.fakeHerdr.SendText(ctx, pane, text)
}
func (f *restartFake) ProcessInfo(_ context.Context, _ string) (herdr.ProcessInfo, error) {
	if f.fail == "process_info" {
		return herdr.ProcessInfo{}, fmt.Errorf("unavailable")
	}
	p := herdr.ProcessInfo{ShellPID: 99999999}
	if f.fail == "foreground" {
		p.ForegroundProcesses = append(p.ForegroundProcesses, struct {
			PID  int    `json:"pid"`
			Name string `json:"name"`
		}{PID: 99999998})
	}
	return p, nil
}
func (f *restartFake) PaneRead(context.Context, string, int) (string, error) { return f.ack, nil }
func (f *restartFake) AgentStart(ctx context.Context, name, kind, pane string, args []string) (herdr.Pane, error) {
	f.args = append([]string{}, args...)
	if f.fail == "start" {
		return herdr.Pane{}, fmt.Errorf("sensitive-test-value")
	}
	p, err := f.fakeHerdr.AgentStart(ctx, name, kind, pane, args)
	if f.fail == "blocked" {
		p.AgentStatus = "blocked"
	}
	return p, err
}
func (f *restartFake) AgentPrompt(ctx context.Context, pane, text string) error {
	if f.fail == "prompt" {
		return fmt.Errorf("sensitive-test-value")
	}
	if f.afterPrompt != nil {
		f.afterPrompt()
	}
	return f.fakeHerdr.AgentPrompt(ctx, pane, text)
}

func restartHarness(t *testing.T) (*harness, *restartFake, store.Worker, store.Task, store.Dispatch) {
	x := newHarness(t)
	r := setupRun(x)
	x.h.addPane(coord, "codex", "idle", true)
	args := []string{"--model", "acme-test-model", "--reasoning", "high"}
	var w store.Worker
	x.mustOp(coord, "worker.start", WorkerStartArgs{Run: r.ID, Agent: "codex", Name: "acme-worker", Args: args}, &w)
	task, err := x.st.CreateTask(r.ID, "acme task", "resume acme task", nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := x.e.dispatchTo(context.Background(), task.ID, w.PaneID)
	if err != nil {
		t.Fatal(err)
	}
	f := &restartFake{fakeHerdr: x.h}
	x.e.h = f
	return x, f, w, task, d
}

func TestWorkerRestartPreservesArgsTaskAndAttempt(t *testing.T) {
	x, f, w, task, old := restartHarness(t)
	before := x.task(task.ID).Attempts
	f.onQuit = func() { x.e.setStatus(w.PaneID, "idle"); x.e.paneGone(w.PaneID, "exited"); x.e.Tick() }
	_, err := x.e.opWorkerRestart(context.Background(), coord, mustJSON(WorkerRestartArgs{Worker: w.PaneID, Env: []string{"ACME_TOKEN=sensitive-test-value"}, Unset: []string{"ACME_OLD"}}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.args, []string{"--model", "acme-test-model", "--reasoning", "high"}) {
		t.Fatalf("launch args changed: %v", f.args)
	}
	active, err := x.st.ActiveDispatchForPane(w.PaneID)
	if err != nil {
		t.Fatal(err)
	}
	if active.ID == old.ID || active.TaskID != task.ID || active.Status != store.DispatchDispatched || x.task(task.ID).Attempts != before {
		t.Fatalf("incorrect redispatch: %+v task=%+v", active, x.task(task.ID))
	}
	old, err = x.st.GetDispatch(old.ID)
	if err != nil || old.Active() {
		t.Fatalf("old dispatch not fenced: %+v %v", old, err)
	}
	if x.st.WorkerLifecycleHeld(w.PaneID) {
		t.Fatal("successful restart left held")
	}
	if _, err := os.Stat(f.scriptPath); !os.IsNotExist(err) {
		t.Fatalf("private script not removed: %v", err)
	}
	for _, text := range f.typed[w.PaneID] {
		if strings.Contains(text, "sensitive-test-value") {
			t.Fatal("environment value leaked to pane command")
		}
	}
}

func TestWorkerRestartFailuresRemainHeld(t *testing.T) {
	for _, phase := range []string{"exit", "foreground", "process_info", "send", "verify", "start", "blocked", "prompt"} {
		t.Run(phase, func(t *testing.T) {
			x, f, w, task, _ := restartHarness(t)
			f.fail = phase
			before := x.task(task.ID).Attempts
			_, err := x.e.opWorkerRestart(context.Background(), coord, mustJSON(WorkerRestartArgs{Worker: w.PaneID, Env: []string{"ACME_TOKEN=sensitive-test-value"}}))
			wantRefusal(t, err, "restart_failed")
			if strings.Contains(err.Error(), "sensitive-test-value") {
				t.Fatal("failure leaked environment")
			}
			got, _ := x.st.GetWorker(w.PaneID)
			if got.Lifecycle != "restart_failed" || x.task(task.ID).Status != store.TaskDispatched || x.task(task.ID).Attempts != before {
				t.Fatalf("failure lost reservation: %+v %+v", got, x.task(task.ID))
			}
			x.e.paneGone(w.PaneID, "closed")
			x.e.Tick()
			if x.task(task.ID).Status != store.TaskDispatched {
				t.Fatal("held dispatch settled")
			}
		})
	}
}

func TestWorkerRestartConcurrentOperationsRemainFenced(t *testing.T) {
	x, f, w, task, old := restartHarness(t)
	entered := make(chan struct{})
	proceed := make(chan struct{})
	finished := make(chan error, 1)
	f.onQuit = func() { close(entered); <-proceed }
	go func() {
		_, err := x.e.opWorkerRestart(context.Background(), coord, mustJSON(WorkerRestartArgs{Worker: w.PaneID}))
		finished <- err
	}()
	<-entered
	_, err := x.e.opWorkerRestart(context.Background(), coord, mustJSON(WorkerRestartArgs{Worker: w.PaneID}))
	wantRefusal(t, err, "worker_lifecycle_busy")
	_, err = x.e.opWorkerStop(context.Background(), coord, mustJSON(WorkerRef{Worker: w.PaneID, Force: true}))
	wantRefusal(t, err, "worker_lifecycle_busy")
	_, err = x.e.opWorkerAbandon(context.Background(), coord, mustJSON(WorkerRef{Worker: w.PaneID}))
	wantRefusal(t, err, "worker_lifecycle_busy")
	x.e.setStatus(w.PaneID, "idle")
	x.e.Tick()
	x.e.paneGone(w.PaneID, "exited")
	active, err := x.st.ActiveDispatchForPane(w.PaneID)
	if err != nil || active.ID != old.ID || x.task(task.ID).Status != store.TaskDispatched {
		t.Fatalf("concurrent supervision settled task: %+v %v", active, err)
	}
	close(proceed)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestWorkerRestartRetainsNewWorkingEventDuringPrompt(t *testing.T) {
	x, f, w, _, _ := restartHarness(t)
	f.afterPrompt = func() { x.e.setStatus(w.PaneID, "working") }
	out, err := x.e.opWorkerRestart(context.Background(), coord, mustJSON(WorkerRestartArgs{Worker: w.PaneID}))
	if err != nil {
		t.Fatal(err)
	}
	d := out.(map[string]any)["dispatch"].(store.Dispatch)
	if d.Status != store.DispatchDispatched || !d.SawWorking || x.e.status[w.PaneID] != "working" {
		t.Fatalf("working event overwritten: %+v %s", d, x.e.status[w.PaneID])
	}
}

func TestWorkerHandOffRecipientReconcilesHeldStatus(t *testing.T) {
	x, _, w, _, d := restartHarness(t)
	if err := x.st.BeginWorkerLifecycle(w.PaneID, "receiving_hand_off"); err != nil {
		t.Fatal(err)
	}
	x.e.setStatus(w.PaneID, "blocked")
	if _, ok := x.e.blocked[w.PaneID]; ok {
		t.Fatal("held event escalated before release")
	}
	x.e.finishWorkerLifecycle(w.PaneID, "")
	if _, ok := x.e.blocked[w.PaneID]; !ok {
		t.Fatal("recipient blocked episode lost after hold release")
	}
	d, err := x.st.GetDispatch(d.ID)
	if err != nil || !d.SawWorking {
		t.Fatalf("recipient activity lost: %+v %v", d, err)
	}
}

func TestWorkerRestartRefusesUnknownLaunchAndConcurrentLifecycle(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:acme")
	_, err := x.e.opWorkerRestart(context.Background(), coord, mustJSON(WorkerRestartArgs{Worker: "w1:acme"}))
	wantRefusal(t, err, "restart_config_unknown")
	x, _, w, _, _ := restartHarness(t)
	if err := x.st.BeginWorkerLifecycle(w.PaneID, "restart"); err != nil {
		t.Fatal(err)
	}
	_, err = x.e.opWorkerRestart(context.Background(), coord, mustJSON(WorkerRestartArgs{Worker: w.PaneID}))
	wantRefusal(t, err, "worker_lifecycle_busy")
	_, err = x.e.opWorkerRelease(context.Background(), coord, mustJSON(WorkerRef{Worker: w.PaneID, Force: true}))
	wantRefusal(t, err, "worker_lifecycle_busy")
}

func TestRestartEnvironmentAppliesLiteralValuesAndUnsets(t *testing.T) {
	value := "acme '$() `quoted`\nline\n\n"
	command, err := restartEnvironment(WorkerRestartArgs{Env: []string{"ACME_VALUE=" + value}, Unset: []string{"ACME_OLD"}}, "acme-nonce")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", command+"; /bin/sh -c 'printf \"%s\" \"$ACME_VALUE\"; [ \"${ACME_OLD+x}\" != x ]'")
	cmd.Env = append(os.Environ(), "ACME_OLD=old")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "HORCH_RESTART_acme-nonce\n"+value {
		t.Fatalf("literal value changed: %q", out)
	}
	command, err = restartEnvironment(WorkerRestartArgs{Env: []string{"ACME_EMPTY="}, Unset: []string{"ACME_OLD"}}, "acme-nonce")
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("/bin/sh", "-c", command+"; /bin/sh -c '[ \"${ACME_EMPTY+x}\" = x ] && [ -z \"$ACME_EMPTY\" ] && [ \"${ACME_OLD+x}\" != x ]'")
	cmd.Env = append(os.Environ(), "ACME_OLD=old")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("empty and unset not exported: %v %s", err, out)
	}
	for _, a := range []WorkerRestartArgs{{Env: []string{"BAD-KEY=value"}}, {Env: []string{"ACME=one", "ACME=two"}}, {Env: []string{"ACME=one"}, Unset: []string{"ACME"}}, {Env: []string{"ACME=x\x00y"}}, {Unset: []string{"BAD;KEY"}}} {
		_, err := restartEnvironment(a, "acme")
		wantRefusal(t, err, "bad_args")
	}
	// A failed mutation must not print the ACK.
	command, err = restartEnvironment(WorkerRestartArgs{Env: []string{"ACME_READONLY=new"}}, "acme-nonce")
	if err != nil {
		t.Fatal(err)
	}
	out, _ = exec.Command("/bin/sh", "-c", "readonly ACME_READONLY=old; "+command).CombinedOutput()
	if strings.Contains(string(out), "HORCH_RESTART_acme-nonce") {
		t.Fatal("failed mutation acknowledged")
	}
}

func gitWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "-C", dir, "init", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	return dir
}
func TestWorkerHandOffReleasesDirtyHelperToLiveActualWorktree(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:acme-helper", "w1:acme-owner")
	dir := gitWorktree(t)
	if err := os.WriteFile(filepath.Join(dir, "acme.txt"), []byte("dirty"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, pane := range []string{"w1:acme-helper", "w1:acme-owner"} {
		x.h.panes[pane].Cwd = &dir
		w, _ := x.st.GetWorker(pane)
		w.Worktree = dir
		if _, err := x.st.RegisterWorker(w); err != nil {
			t.Fatal(err)
		}
	}
	_, err := x.e.opWorkerRelease(context.Background(), coord, mustJSON(WorkerRef{Worker: "w1:acme-helper", HandOffTo: "w1:acme-owner"}))
	if err != nil {
		t.Fatal(err)
	}
	w, _ := x.st.GetWorker("w1:acme-helper")
	if w.State != store.WorkerReleased || w.HandOffTo != "w1:acme-owner" || w.HandOffAt == 0 || w.HandOffWorktree == "" {
		t.Fatalf("no responsibility transfer: %+v", w)
	}
	if x.st.WorkerLifecycleHeld("w1:acme-owner") {
		t.Fatal("recipient stayed held")
	}
	if _, err := os.Stat(filepath.Join(dir, "acme.txt")); err != nil {
		t.Fatal("dirty work lost")
	}
}

func TestWorkerHandOffRejectsInvalidRecipients(t *testing.T) {
	for _, kind := range []string{"same", "gone", "shell", "other_worktree", "held", "recorded_only", "force"} {
		t.Run(kind, func(t *testing.T) {
			x := newHarness(t)
			setupRun(x, "w1:acme-helper", "w1:acme-owner")
			dir := gitWorktree(t)
			other := gitWorktree(t)
			x.h.panes["w1:acme-helper"].Cwd = &dir
			x.h.panes["w1:acme-owner"].Cwd = &dir
			a := WorkerRef{Worker: "w1:acme-helper", HandOffTo: "w1:acme-owner"}
			switch kind {
			case "same":
				a.HandOffTo = a.Worker
			case "gone":
				delete(x.h.panes, "w1:acme-owner")
			case "shell":
				x.h.panes["w1:acme-owner"].Agent = nil
			case "other_worktree":
				x.h.panes["w1:acme-owner"].Cwd = &other
			case "held":
				_ = x.st.BeginWorkerLifecycle("w1:acme-owner", "restart")
			case "recorded_only":
				x.h.panes["w1:acme-owner"].Cwd = nil
			case "force":
				a.Force = true
			}
			_, err := x.e.opWorkerRelease(context.Background(), coord, mustJSON(a))
			if err == nil {
				t.Fatal("invalid hand-off accepted")
			}
			if len(x.h.closed) > 0 {
				t.Fatal("refusal closed helper")
			}
			w, _ := x.st.GetWorker(a.Worker)
			if w.HandOffTo != "" || w.Lifecycle != "" {
				t.Fatalf("refusal changed responsibility: %+v", w)
			}
		})
	}
}

func TestWorkerRestartRejectsReplacementWhileWaitingForProcessLock(t *testing.T) {
	cases := []struct {
		name, phase string
		change      func(*store.Worker)
	}{
		{name: "registration", phase: "receiving_hand_off", change: func(w *store.Worker) {
			w.Name = "replacement-worker"
			w.Worktree = t.TempDir()
			w.AgentArgs = []string{"--model", "replacement-model"}
		}},
		{name: "account", phase: "restart", change: func(w *store.Worker) { w.Account = "replacement-account" }},
		{name: "profile", phase: "restart", change: func(w *store.Worker) { w.Profile = "replacement-profile" }},
		{name: "model", phase: "restart", change: func(w *store.Worker) { w.Model = "replacement-model" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x, f, w, task, old := restartHarness(t)
			// Launch args are intentionally absent from RPC JSON. Read the stored
			// registration so metadata-only replacements keep identical private args.
			stored, readErr := x.st.GetWorker(w.PaneID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			w = stored
			x.e.processMu.Lock()
			locked := true
			defer func() {
				if locked {
					x.e.processMu.Unlock()
				}
			}()
			finished := make(chan error, 1)
			go func() {
				_, err := x.e.opWorkerRestart(context.Background(), coord, mustJSON(WorkerRestartArgs{Worker: w.PaneID}))
				finished <- err
			}()
			deadline := time.Now().Add(time.Second)
			for {
				current, err := x.st.GetWorker(w.PaneID)
				if err != nil {
					t.Fatal(err)
				}
				if current.Lifecycle == "restart" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("restart did not reserve lifecycle before process lock")
				}
				time.Sleep(time.Millisecond)
			}
			// A competing process owner replaced the registration while it held processMu.
			// Preserve that replacement's lifecycle when the older restart finally gets the lock.
			replacement := w
			tc.change(&replacement)
			x.e.mu.Lock()
			err := x.st.SetWorkerLifecycle(w.PaneID, "")
			if err == nil {
				_, err = x.st.RegisterWorker(replacement)
			}
			if err == nil {
				err = x.st.BeginWorkerLifecycle(w.PaneID, tc.phase)
			}
			x.e.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			x.e.processMu.Unlock()
			locked = false
			select {
			case err := <-finished:
				wantRefusal(t, err, "worker_changed")
			case <-time.After(2 * time.Second):
				t.Fatal("stale restart did not return")
			}
			current, err := x.st.GetWorker(w.PaneID)
			if err != nil || current.Name != replacement.Name || current.Lifecycle != tc.phase || !reflect.DeepEqual(current.AgentArgs, replacement.AgentArgs) || current.Account != replacement.Account || current.Profile != replacement.Profile || current.Model != replacement.Model {
				t.Fatalf("replacement registration damaged: %+v %v", current, err)
			}
			active, err := x.st.ActiveDispatchForPane(w.PaneID)
			if err != nil || active.ID != old.ID || x.task(task.ID).Attempts != 1 {
				t.Fatalf("stale restart changed dispatch: %+v %v", active, err)
			}
			if f.scriptPath != "" || len(f.args) != 0 || len(f.typed[w.PaneID]) != 0 {
				t.Fatalf("stale restart mutated pane: script=%q args=%v typed=%v", f.scriptPath, f.args, f.typed[w.PaneID])
			}
			if len(x.inbox(coord)) != 0 {
				t.Fatal("stale restart escalated as replacement failure")
			}

		})
	}
}
