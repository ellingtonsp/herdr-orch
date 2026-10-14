package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

type fakeProcesses struct {
	ps        []ProcessRecord
	signals   []syscall.Signal
	signalled []int
	inspect   func(int) (ProcessRecord, error)
	listErr   error
	stubborn  bool
	onSignal  func(int, syscall.Signal)
}

func (f *fakeProcesses) List(context.Context) ([]ProcessRecord, error) {
	return append([]ProcessRecord{}, f.ps...), f.listErr
}
func (f *fakeProcesses) Inspect(_ context.Context, pid int) (ProcessRecord, error) {
	if f.inspect != nil {
		return f.inspect(pid)
	}
	for _, p := range f.ps {
		if p.PID == pid {
			return p, nil
		}
	}
	return ProcessRecord{}, os.ErrNotExist
}
func (f *fakeProcesses) Signal(pid int, sig syscall.Signal) error {
	f.signalled = append(f.signalled, pid)
	f.signals = append(f.signals, sig)
	if !f.stubborn && sig == syscall.SIGKILL {
		for i, p := range f.ps {
			if p.PID == pid {
				f.ps = append(f.ps[:i], f.ps[i+1:]...)
				break
			}
		}
	}
	if f.onSignal != nil {
		f.onSignal(pid, sig)
	}
	return nil
}
func TestProcessSafetyBoundaries(t *testing.T) {
	root := t.TempDir()
	root, _ = processRoot(root)
	ps := []ProcessRecord{{PID: 100001, Started: "1", Cwd: root, Command: "helper"}, {PID: 100002, Started: "2", Cwd: root + "-other", Command: "helper"}, {PID: 100003, Started: "3", Cwd: root, Command: "/bin/horch"}, {PID: 100004, Started: "4", Cwd: root, Command: "/bin/herdr"}, {PID: os.Getpid(), Started: "5", Cwd: root, Command: "test"}}
	f := &fakeProcesses{ps: ps}
	e := &Engine{processes: f, processGrace: time.Millisecond}
	dry := e.cleanProcesses(context.Background(), root, ps, true)
	if len(dry) != 4 || len(f.signals) != 0 {
		t.Fatalf("dry run: %+v %+v", dry, f.signals)
	}
	got := e.cleanProcesses(context.Background(), root, ps, false)
	if len(f.signals) != 2 || f.signals[0] != syscall.SIGTERM || f.signals[1] != syscall.SIGKILL || f.signalled[0] != 100001 || f.signalled[1] != 100001 || got[0].Action != "killed" {
		t.Fatalf("cleanup: %+v %+v", got, f)
	}
}
func TestCleanupRevalidatesEverySignal(t *testing.T) {
	for _, mode := range []string{"reused", "moved", "protected", "unreadable"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			p := ProcessRecord{PID: 100001, Started: "1", Cwd: root, Command: "helper"}
			f := &fakeProcesses{ps: []ProcessRecord{p}}
			f.inspect = func(int) (ProcessRecord, error) {
				q := p
				if len(f.signals) > 0 {
					switch mode {
					case "reused":
						q.Started = "2"
					case "moved":
						q.Cwd = root + "-other"
					case "protected":
						q.Command = "herdr-orch"
					case "unreadable":
						return q, errors.New("unreadable")
					}
				}
				return q, nil
			}
			e := &Engine{processes: f, processGrace: time.Millisecond}
			got := e.cleanProcesses(context.Background(), root, f.ps, false)
			if len(f.signals) != 1 || f.signals[0] != syscall.SIGTERM {
				t.Fatalf("unsafe escalation %+v %+v", got, f.signals)
			}
		})
	}
}
func TestProcessParsers(t *testing.T) {
	ps := parseProcessTable("12 1 501 Tue Oct 13 16:00:01 2026 S /bin/a helper\n13 1 502 Tue Oct 13 16:00:01 2026 S /bin/other\n14 1 501 Tue Oct 13 16:00:01 2026 Z /bin/zombie", 501)
	if len(ps) != 1 || ps[0].Command != "/bin/a helper" || ps[0].Started != "Tue Oct 13 16:00:01 2026" {
		t.Fatalf("ps %+v", ps)
	}
	cwd := parseCwdTable([]byte("p12\x00\nfcwd\x00n/tmp/a b\x00\np13\x00\nfcwd\x00n/tmp/gone (deleted)\x00\n"))
	if cwd[12] != "/tmp/a b" || cwd[13] != "/tmp/gone" {
		t.Fatalf("cwd %+v", cwd)
	}
}
func TestProcessRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	got, err := processRoot(filepath.Join(alias, "removed"))
	want, _ := filepath.EvalSymlinks(real)
	if err != nil || got != filepath.Join(want, "removed") {
		t.Fatalf("root %s %v", got, err)
	}
	for _, bad := range []string{"/", "relative"} {
		if _, err := processRoot(bad); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}
func TestSweepReleasedRemovedAndReused(t *testing.T) {
	x := newHarness(t)
	r := setupRun(x)
	base := t.TempDir()
	base, _ = processRoot(base)
	released := filepath.Join(base, "released")
	reused := filepath.Join(base, "reused")
	removed := filepath.Join(base, "removed")
	for _, dir := range []string{released, reused} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for i, w := range []store.Worker{{PaneID: "w1:p2", Worktree: released}, {PaneID: "w1:p3", Worktree: reused}, {PaneID: "w1:p4", Worktree: reused}, {PaneID: "w1:p5", Worktree: removed}} {
		w.RunID = r.ID
		x.st.RegisterWorker(w)
		if i < 2 {
			x.st.SetWorkerState(w.PaneID, store.WorkerReleased, "", "")
		}
	}
	f := &fakeProcesses{ps: []ProcessRecord{{PID: 100001, Started: "1", Cwd: released, Command: "helper"}, {PID: 100002, Started: "2", Cwd: reused, Command: "helper"}, {PID: 100003, Started: "3", Cwd: removed, Command: "helper"}}}
	x.e.processes = f
	x.e.processGrace = time.Millisecond
	var got SweepResult
	x.mustOp(coord, "sweep", SweepArgs{DryRun: true}, &got)
	if len(got.Processes) != 2 || len(f.signals) != 0 {
		t.Fatalf("sweep %+v", got)
	}
	x.mustOp(coord, "sweep", SweepArgs{}, &got)
	if len(got.Processes) != 2 || len(f.signals) != 4 {
		t.Fatalf("sweep %+v %+v", got, f.signals)
	}
}
func TestReleaseDiscoversDetachedProcesses(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:p2")
	dir := t.TempDir()
	dir, _ = processRoot(dir)
	w, _ := x.st.GetWorker("w1:p2")
	w.Worktree = dir
	x.st.RegisterWorker(w)
	f := &fakeProcesses{ps: []ProcessRecord{{PID: 100001, Started: "1", Cwd: dir, Command: "helper"}}}
	x.e.processes = f
	x.e.processGrace = time.Millisecond
	var got CloseResult
	x.mustOp(coord, "worker.release", WorkerRef{Worker: "w1:p2"}, &got)
	if len(f.signals) != 2 || len(got.Processes) != 1 || got.Processes[0].Action != "killed" {
		t.Fatalf("release %+v signals %+v", got, f.signals)
	}
}
func TestNativeProcessInventory(t *testing.T) {
	dir := t.TempDir()
	dir, _ = processRoot(dir)
	cmd := exec.Command("sleep", "60")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	ps, err := (nativeProcesses{}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range ps {
		if p.PID == cmd.Process.Pid {
			found = true
			if p.Cwd != dir || p.Started == "" || !strings.Contains(p.Command, "sleep") {
				t.Fatalf("identity %+v", p)
			}
		}
	}
	if !found {
		t.Fatal("child missing from inventory")
	}
}

func TestRootSymlinkCannotBroadenCleanup(t *testing.T) {
	link := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink("/", link); err != nil {
		t.Fatal(err)
	}
	if _, err := processRoot(link); err == nil {
		t.Fatal("accepted root symlink")
	}
}
func TestReleaseScanFailureKeepsWorkerAndLease(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:p2")
	w, _ := x.st.GetWorker("w1:p2")
	w.Worktree = t.TempDir()
	x.st.RegisterWorker(w)
	if _, err := x.st.AcquireLease("port:3000", w.PaneID, time.Hour); err != nil {
		t.Fatal(err)
	}
	x.e.processes = &fakeProcesses{listErr: errors.New("inventory unavailable")}
	_, err := x.op(coord, "worker.release", WorkerRef{Worker: w.PaneID})
	wantRefusal(t, err, "process_scan_failed")
	current, _ := x.st.GetWorker(w.PaneID)
	leases, _ := x.st.ListLeases()
	if current.State != store.WorkerLive || len(leases) != 1 || len(x.h.closed) != 0 {
		t.Fatalf("lost ownership: %+v %+v", current, leases)
	}
}
func TestReleaseSurvivorRetainsLease(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:p2")
	w, _ := x.st.GetWorker("w1:p2")
	w.Worktree = t.TempDir()
	w.Worktree, _ = processRoot(w.Worktree)
	x.st.RegisterWorker(w)
	x.st.AcquireLease("db:acme", w.PaneID, time.Hour)
	x.e.processes = &fakeProcesses{ps: []ProcessRecord{{PID: 100001, Started: "1", Cwd: w.Worktree, Command: "helper"}}, stubborn: true}
	x.e.processGrace = time.Millisecond
	_, err := x.op(coord, "worker.release", WorkerRef{Worker: w.PaneID})
	wantRefusal(t, err, "release_failed")
	leases, _ := x.st.ListLeases()
	current, _ := x.st.GetWorker(w.PaneID)
	if len(leases) != 1 || current.State != store.WorkerReleaseFailed {
		t.Fatalf("survivor lost lease: %+v %+v", current, leases)
	}
}
func TestCleanupExcludesStartingAndSharedWorktrees(t *testing.T) {
	x := newHarness(t)
	r := setupRun(x, "w1:p2")
	root := t.TempDir()
	root, _ = processRoot(root)
	w, _ := x.st.GetWorker("w1:p2")
	w.Worktree = root
	x.st.RegisterWorker(w)
	release, err := x.e.reserveStarting(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = x.op(coord, "worker.release", WorkerRef{Worker: w.PaneID})
	wantRefusal(t, err, "worktree_in_use")
	release()
	x.st.RegisterWorker(store.Worker{PaneID: "w1:p3", RunID: r.ID, Worktree: root, Name: "other"})
	_, err = x.op(coord, "worker.release", WorkerRef{Worker: w.PaneID})
	wantRefusal(t, err, "worktree_in_use")
	if len(x.h.closed) != 0 {
		t.Fatal("closed shared worktree")
	}
}
func TestClosingBlocksRegistrationAndStartupReservation(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:p2")
	root := t.TempDir()
	root, _ = processRoot(root)
	w, _ := x.st.GetWorker("w1:p2")
	w.Worktree = root
	x.st.RegisterWorker(w)
	x.e.closing[w.PaneID] = true
	_, err := x.op(coord, "worker.register", WorkerRegisterArgs{Pane: w.PaneID, Worktree: root})
	wantRefusal(t, err, "worker_closing")
	_, err = x.e.reserveStarting(root)
	wantRefusal(t, err, "worktree_in_use")
}
func TestSweepStartingScopeIsExcluded(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:p2")
	root := t.TempDir()
	root, _ = processRoot(root)
	w, _ := x.st.GetWorker("w1:p2")
	w.Worktree = root
	x.st.RegisterWorker(w)
	x.st.SetWorkerState(w.PaneID, store.WorkerReleased, "", "")
	release, err := x.e.reserveStarting(root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	f := &fakeProcesses{ps: []ProcessRecord{{PID: 100001, Cwd: root, Command: "helper", Started: "1"}}}
	x.e.processes = f
	var got SweepResult
	x.mustOp(coord, "sweep", SweepArgs{}, &got)
	if len(got.Processes) != 0 || len(f.signals) != 0 {
		t.Fatalf("starting worker swept %+v", got)
	}
}
func TestNativeCleanupOutsideSiblingSurvives(t *testing.T) {
	root := t.TempDir()
	root, _ = processRoot(root)
	dir := filepath.Join(root, "acme")
	outside := filepath.Join(root, "acme-other")
	for _, p := range []string{dir, outside} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	spawn := func(dir string) *exec.Cmd {
		cmd := exec.Command("sleep", "60")
		cmd.Dir = dir
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go cmd.Wait()
		t.Cleanup(func() { cmd.Process.Kill() })
		return cmd
	}
	inner := spawn(dir)
	outer := spawn(outside)
	ps, err := (nativeProcesses{}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{processGrace: time.Second}
	got := e.cleanProcesses(context.Background(), dir, ps, false)
	if len(got) != 1 || got[0].PID != inner.Process.Pid || got[0].Action != "terminated" || !pidAlive(outer.Process.Pid) {
		t.Fatalf("cleanup boundary %+v", got)
	}
}
func TestNativeRemovedWorktreeDiscovery(t *testing.T) {
	root := filepath.Join(t.TempDir(), "removed")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	root, _ = processRoot(root)
	cmd := exec.Command("sleep", "60")
	cmd.Dir = root
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	ps, err := (nativeProcesses{}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.PID == cmd.Process.Pid {
			if p.Cwd != root {
				t.Fatalf("removed cwd %+v", p)
			}
			return
		}
	}
	t.Fatal("removed cwd not discovered")
}

func TestProtectedPaneIsNeverClosed(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:p2")
	x.h.pids["w1:p2"] = []int{100001}
	f := &fakeProcesses{ps: []ProcessRecord{{PID: 100001, Command: "/bin/herdr", Started: "1", Cwd: "/tmp"}}}
	x.e.processes = f
	_, err := x.op(coord, "worker.release", WorkerRef{Worker: "w1:p2"})
	wantRefusal(t, err, "protected_process")
	if len(x.h.closed) != 0 || len(f.signals) != 0 {
		t.Fatal("protected pane was closed")
	}
}
func TestNativeCanceledInspectionIsNotExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (nativeProcesses{}).Inspect(ctx, os.Getpid())
	if err == nil || os.IsNotExist(err) {
		t.Fatalf("cancelled inventory claimed exit: %v", err)
	}
}
func TestReleaseRetainsOutsidePaneProcess(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:p2")
	w, _ := x.st.GetWorker("w1:p2")
	w.Worktree = t.TempDir()
	w.Worktree, _ = processRoot(w.Worktree)
	x.st.RegisterWorker(w)
	x.h.pids[w.PaneID] = []int{100001}
	x.h.killOnClose = false
	f := &fakeProcesses{ps: []ProcessRecord{{PID: 100001, Command: "helper", Started: "1", Cwd: w.Worktree + "-outside"}}}
	x.e.processes = f
	_, err := x.op(coord, "worker.release", WorkerRef{Worker: w.PaneID})
	wantRefusal(t, err, "release_failed")
	if len(f.signals) != 0 {
		t.Fatalf("signalled outside process: %+v", f.signals)
	}
}

func TestScopeCleanupFollowsLateChild(t *testing.T) {
	root := t.TempDir()
	root, _ = processRoot(root)
	f := &fakeProcesses{ps: []ProcessRecord{{PID: 100001, Started: "1", Cwd: root, Command: "helper"}}}
	f.onSignal = func(pid int, sig syscall.Signal) {
		if pid == 100001 && sig == syscall.SIGKILL {
			f.ps = append(f.ps, ProcessRecord{PID: 100002, Started: "2", Cwd: root, Command: "child"})
		}
	}
	e := &Engine{processes: f, processGrace: time.Millisecond}
	got, err := e.cleanProcessScope(context.Background(), root, append([]ProcessRecord{}, f.ps...), false)
	if err != nil || len(got) != 2 || len(f.signals) != 4 || got[1].Action != "killed" {
		t.Fatalf("late child: %+v %v signals=%v", got, err, f.signals)
	}
}
func TestScopeCleanupRetriesProcessReturningInside(t *testing.T) {
	root := t.TempDir()
	p := ProcessRecord{PID: 100001, Started: "1", Cwd: root, Command: "helper"}
	f := &fakeProcesses{ps: []ProcessRecord{p}}
	first := true
	f.inspect = func(pid int) (ProcessRecord, error) {
		if first {
			first = false
			q := p
			q.Cwd = root + "-outside"
			return q, nil
		}
		for _, q := range f.ps {
			if q.PID == pid {
				return q, nil
			}
		}
		return ProcessRecord{}, os.ErrNotExist
	}
	e := &Engine{processes: f, processGrace: time.Millisecond}
	got, err := e.cleanProcessScope(context.Background(), root, append([]ProcessRecord{}, f.ps...), false)
	if err != nil || len(got) != 1 || got[0].Action != "killed" || len(f.signals) != 2 {
		t.Fatalf("reentry: %+v %v %v", got, err, f.signals)
	}
}
func TestScopeGuardRecheckedBeforeSignal(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live")
	p := ProcessRecord{PID: 100001, Started: "1", Cwd: root, Command: "helper"}
	f := &fakeProcesses{ps: []ProcessRecord{p}}
	f.inspect = func(int) (ProcessRecord, error) { q := p; q.Cwd = live; return q, nil }
	e := &Engine{processes: f, processGrace: time.Millisecond}
	got := e.cleanProcesses(context.Background(), root, f.ps, false, func(p ProcessRecord) bool { return !insideWorktree(live, p.Cwd) })
	if len(f.signals) != 0 || got[0].Action != "left_worktree" {
		t.Fatalf("live scope signalled: %+v %v", got, f.signals)
	}
}
func TestCloseRejectsStaleRegistration(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:p2")
	old, _ := x.st.GetWorker("w1:p2")
	current := old
	current.Worktree = t.TempDir()
	x.st.RegisterWorker(current)
	_, err := x.e.closeWorker(context.Background(), old, store.WorkerReleased, false)
	wantRefusal(t, err, "worker_changed")
	if len(x.h.closed) != 0 {
		t.Fatal("closed replacement registration")
	}
}
func TestStartExistingPaneRejectsMismatchedCwd(t *testing.T) {
	x := newHarness(t)
	setupRun(x)
	x.h.addPane("w1:p2", "", "unknown", false)
	x.h.panes["w1:p2"].Agent = nil
	actual := t.TempDir()
	x.h.panes["w1:p2"].Cwd = &actual
	_, err := x.op(coord, "worker.start", WorkerStartArgs{Pane: "w1:p2", Agent: "codex", Cwd: t.TempDir(), Name: "builder"})
	wantRefusal(t, err, "cwd_mismatch")
	if x.h.panes["w1:p2"].Agent != nil {
		t.Fatal("started agent in unverified cwd")
	}
}

func TestPaneInspectionFailureNeverClosesOrFreesLease(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:p2")
	if _, err := x.st.AcquireLease("port:3000", "w1:p2", time.Hour); err != nil {
		t.Fatal(err)
	}
	x.h.processInfoErr = errors.New("transport unavailable")
	f := &fakeProcesses{}
	x.e.processes = f
	_, err := x.op(coord, "worker.release", WorkerRef{Worker: "w1:p2"})
	wantRefusal(t, err, "process_scan_failed")
	leases, _ := x.st.ListLeases()
	if len(x.h.closed) != 0 || len(f.signals) != 0 || len(leases) != 1 {
		t.Fatalf("unsafe close: closed=%v signals=%v leases=%v", x.h.closed, f.signals, leases)
	}
}
