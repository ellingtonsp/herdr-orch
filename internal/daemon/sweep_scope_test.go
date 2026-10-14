package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

func TestSweepRejectsBroadRecordedHome(t *testing.T) {
	x := newHarness(t)
	setupRun(x)
	home, _ := processRoot(t.TempDir())
	t.Setenv("HOME", home)
	_, err := x.st.RegisterWorker(store.Worker{PaneID: "w1:old", Worktree: home})
	if err != nil {
		t.Fatal(err)
	}
	if err := x.st.SetWorkerState("w1:old", store.WorkerReleased, "", ""); err != nil {
		t.Fatal(err)
	}
	f := &fakeProcesses{ps: []ProcessRecord{
		{UID: os.Geteuid(), UIDKnown: true, PID: 100001, Started: "1", Command: "/System/Library/CoreServices/MenuBarAgent", Cwd: "/"},
		{UID: os.Geteuid(), UIDKnown: true, PID: 100002, Started: "2", Command: "/System/Library/CoreServices/WallpaperAgent", Cwd: filepath.Join(home, "Library")},
		{UID: os.Geteuid(), UIDKnown: true, PID: 100003, Started: "3", Command: "shell", Cwd: home},
		{UID: os.Geteuid(), UIDKnown: true, PID: 100004, Started: "4", Command: "server", Cwd: filepath.Join(home, "unrelated")},
	}}
	x.e.processes = f
	var got SweepResult
	x.mustOp(coord, "sweep", SweepArgs{DryRun: true}, &got)
	if len(got.Processes) != 0 || len(f.signals) != 0 {
		t.Fatalf("unsafe candidates: %+v", got.Processes)
	}
}

func TestSweepStoppedExistingRootIsNotReleased(t *testing.T) {
	x := newHarness(t)
	setupRun(x)
	root, _ := processRoot(t.TempDir())
	x.st.RegisterWorker(store.Worker{PaneID: "w1:old", Worktree: root})
	x.st.SetWorkerState("w1:old", store.WorkerStopped, "", "")
	x.e.processes = &fakeProcesses{ps: []ProcessRecord{{UID: os.Geteuid(), UIDKnown: true, PID: 100001, Started: "1", Command: "server", Cwd: root}}}
	var got SweepResult
	x.mustOp(coord, "sweep", SweepArgs{DryRun: true}, &got)
	if len(got.Processes) != 0 {
		t.Fatalf("stopped worker authorized sweep: %+v", got.Processes)
	}
}

func TestCleanupRejectsHomeScope(t *testing.T) {
	home, _ := processRoot(t.TempDir())
	t.Setenv("HOME", home)
	f := &fakeProcesses{ps: []ProcessRecord{{UID: os.Geteuid(), UIDKnown: true, PID: 100001, Started: "1", Command: "shell", Cwd: home}}}
	e := &Engine{processes: f}
	got, err := e.cleanProcessScope(context.Background(), home, f.ps, true)
	if err == nil || len(got) != 0 {
		t.Fatalf("unsafe release scope accepted: %+v %v", got, err)
	}
}

func TestCleanupRejectsSymlinkEscape(t *testing.T) {
	root, _ := processRoot(t.TempDir())
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	f := &fakeProcesses{ps: []ProcessRecord{{UID: os.Geteuid(), UIDKnown: true, PID: 100001, Started: "1", Command: "server", Cwd: link}}}
	e := &Engine{processes: f}
	got, err := e.cleanProcessScope(context.Background(), root, f.ps, true)
	if err != nil || len(got) != 0 {
		t.Fatalf("symlink escape selected: %+v %v", got, err)
	}
}

func TestCwdParserPreservesLiteralDeletedSuffix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acme (deleted)")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	got := parseCwdTable([]byte("p42\x00\nfcwd\x00n" + path + "\x00\n"))
	if got[42] != path {
		t.Fatalf("literal sibling rewritten into worktree: %q", got[42])
	}
}

func TestSweepOnlyRecordedReleasedOrMissingWorktrees(t *testing.T) {
	for _, mode := range []string{"none", "live", "released", "missing"} {
		t.Run(mode, func(t *testing.T) {
			x := newHarness(t)
			setupRun(x)
			root, _ := processRoot(t.TempDir())
			home, _ := processRoot(t.TempDir())
			t.Setenv("HOME", home)
			unrelated, _ := processRoot(t.TempDir())
			if mode != "none" {
				x.st.RegisterWorker(store.Worker{PaneID: "w1:old", Worktree: root})
				if mode == "released" {
					x.st.SetWorkerState("w1:old", store.WorkerReleased, "", "")
				}
				if mode == "missing" {
					if err := os.Remove(root); err != nil {
						t.Fatal(err)
					}
				}
			}
			f := &fakeProcesses{ps: []ProcessRecord{
				{UID: os.Geteuid(), UIDKnown: true, PID: 100001, Started: "1", Command: "/System/Library/CoreServices/MenuBarAgent", Cwd: "/System/Library/CoreServices"},
				{UID: os.Geteuid(), UIDKnown: true, PID: 100002, Started: "2", Command: "/System/Library/CoreServices/WallpaperAgent", Cwd: "/"},
				{UID: os.Geteuid(), UIDKnown: true, PID: 100003, Started: "3", Command: "shell", Cwd: home},
				{UID: os.Geteuid(), UIDKnown: true, PID: 100004, Started: "4", Command: "server", Cwd: unrelated},
				{UID: os.Geteuid(), UIDKnown: true, PID: 100005, Started: "5", Command: "server", Cwd: filepath.Join(root, "nested")},
				{UID: os.Geteuid() + 1, UIDKnown: true, PID: 100006, Started: "6", Command: "other-user", Cwd: root},
				{PID: 100007, Started: "7", Command: "unknown-user", Cwd: root},
			}}
			x.e.processes = f
			x.e.processGrace = time.Millisecond
			for _, dry := range []bool{true, false} {
				var got SweepResult
				x.mustOp(coord, "sweep", SweepArgs{DryRun: dry}, &got)
				want := 0
				if mode == "released" || mode == "missing" {
					want = 1
				}
				if len(got.Processes) != want {
					t.Fatalf("mode=%s dry=%v candidates=%+v", mode, dry, got.Processes)
				}
				if want == 1 && (got.Processes[0].PID != 100005 || got.Processes[0].Worktree != root) {
					t.Fatalf("wrong candidate: %+v", got.Processes)
				}
				for _, pid := range f.signalled {
					if pid != 100005 {
						t.Fatalf("outside process signalled: %d", pid)
					}
				}
			}
		})
	}
}

func TestProtectedRootsCannotAuthorizeCleanup(t *testing.T) {
	home, _ := processRoot(t.TempDir())
	t.Setenv("HOME", home)
	for _, root := range []string{"", "/", "/private", "/var", "/tmp", "/Volumes", "/opt", home, filepath.Dir(home), "/System", "/System/Library", "/Library", "/usr", "/usr/local/acme", "/Applications", filepath.Join(home, "Library/acme")} {
		t.Run(root, func(t *testing.T) {
			if _, err := newCleanupScope(root); err == nil {
				t.Fatalf("unsafe root allowed: %q", root)
			}
		})
	}
}

func TestReleaseUnsafeScopeNeverClosesPane(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:worker")
	home, _ := processRoot(t.TempDir())
	t.Setenv("HOME", home)
	w, _ := x.st.GetWorker("w1:worker")
	w.Worktree = home
	x.st.RegisterWorker(w)
	f := &fakeProcesses{}
	x.e.processes = f
	_, err := x.op(coord, "worker.release", WorkerRef{Worker: w.PaneID})
	wantRefusal(t, err, "unsafe_worktree")
	if len(x.h.closed) > 0 || len(f.signals) > 0 {
		t.Fatal("unsafe release mutated processes")
	}
}

func TestCleanupRechecksOwnerAndScopeBeforeKill(t *testing.T) {
	for _, mode := range []string{"other_user", "home", "system", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			root, _ := processRoot(t.TempDir())
			home, _ := processRoot(t.TempDir())
			t.Setenv("HOME", home)
			p := ProcessRecord{UID: os.Geteuid(), UIDKnown: true, PID: 100001, Started: "1", Command: "server", Cwd: root}
			f := &fakeProcesses{ps: []ProcessRecord{p}}
			f.inspect = func(int) (ProcessRecord, error) {
				q := p
				if len(f.signals) > 0 {
					switch mode {
					case "other_user":
						q.UID++
					case "home":
						q.Cwd = home
					case "system":
						q.Cwd = "/System/Library"
					case "symlink":
						q.Cwd = filepath.Join(root, "escape")
					}
				}
				return q, nil
			}
			if err := os.Symlink(home, filepath.Join(root, "escape")); err != nil {
				t.Fatal(err)
			}
			e := &Engine{processes: f, processGrace: time.Millisecond}
			got := e.cleanProcesses(context.Background(), root, f.ps, false)
			if len(f.signals) != 1 || f.signals[0] != syscall.SIGTERM || len(got) != 1 || got[0].Action != "left_worktree" {
				t.Fatalf("unsafe repeat signal: %+v %+v", f.signals, got)
			}
		})
	}
}

func TestSweepWithoutEligibleRootsDoesNotInspectHost(t *testing.T) {
	x := newHarness(t)
	setupRun(x)
	x.e.processes = &fakeProcesses{listErr: errors.New("unexpected host inventory")}
	var got SweepResult
	x.mustOp(coord, "sweep", SweepArgs{DryRun: true}, &got)
	if len(got.Processes) != 0 {
		t.Fatal(got.Processes)
	}
}

func TestUnsafeLiveRootStillProtectsReleasedChild(t *testing.T) {
	x := newHarness(t)
	setupRun(x)
	home, _ := processRoot(t.TempDir())
	t.Setenv("HOME", home)
	root := filepath.Join(home, "acme")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	x.st.RegisterWorker(store.Worker{PaneID: "w1:live", Worktree: home})
	x.st.RegisterWorker(store.Worker{PaneID: "w1:released", Worktree: root})
	x.st.SetWorkerState("w1:released", store.WorkerReleased, "", "")
	f := &fakeProcesses{ps: []ProcessRecord{{UID: os.Geteuid(), UIDKnown: true, PID: 100001, Started: "1", Command: "server", Cwd: root}}}
	x.e.processes = f
	for _, dry := range []bool{true, false} {
		var got SweepResult
		x.mustOp(coord, "sweep", SweepArgs{DryRun: dry}, &got)
		if len(got.Processes) != 0 || len(f.signals) != 0 {
			t.Fatalf("lost live exclusion: %+v", got.Processes)
		}
	}
}

func TestSystemExecutableInWorktreeIsExcluded(t *testing.T) {
	root, _ := processRoot(t.TempDir())
	for _, command := range []string{"/System/Library/CoreServices/MenuBarAgent", "/System/Library/CoreServices/WallpaperAgent", "/Library/acme/agent", "/Applications/acme/agent", "/usr/libexec/agent"} {
		p := ProcessRecord{UID: os.Geteuid(), UIDKnown: true, PID: 100001, Started: "1", Command: command, Cwd: root}
		f := &fakeProcesses{ps: []ProcessRecord{p}}
		e := &Engine{processes: f}
		got, err := e.cleanProcessScope(context.Background(), root, f.ps, false)
		if err != nil || len(got) != 0 || len(f.signals) != 0 {
			t.Fatalf("system executable selected: %+v %v", got, err)
		}
	}
}

func TestCleanupRechecksScopeBeforeTerm(t *testing.T) {
	for _, mode := range []string{"owner", "cwd", "root_symlink"} {
		t.Run(mode, func(t *testing.T) {
			root, _ := processRoot(t.TempDir())
			outside, _ := processRoot(t.TempDir())
			p := ProcessRecord{UID: os.Geteuid(), UIDKnown: true, PID: 100001, Started: "1", Command: "server", Cwd: root}
			f := &fakeProcesses{ps: []ProcessRecord{p}}
			f.inspect = func(int) (ProcessRecord, error) {
				q := p
				switch mode {
				case "owner":
					q.UID++
				case "cwd":
					q.Cwd = outside
				case "root_symlink":
					if err := os.Remove(root); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, root); err != nil {
						t.Fatal(err)
					}
				}
				return q, nil
			}
			e := &Engine{processes: f, processGrace: time.Millisecond}
			got := e.cleanProcesses(context.Background(), root, f.ps, false)
			if len(f.signals) != 0 || len(got) != 1 || got[0].Action != "left_worktree" {
				t.Fatalf("signalled changed scope: %+v %+v", got, f.signals)
			}
		})
	}
}

func TestSweepRechecksMissingRootBeforeTerm(t *testing.T) {
	x := newHarness(t)
	setupRun(x)
	root, _ := processRoot(t.TempDir())
	x.st.RegisterWorker(store.Worker{PaneID: "w1:old", Worktree: root})
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	p := ProcessRecord{UID: os.Geteuid(), UIDKnown: true, PID: 100001, Started: "1", Command: "server", Cwd: root}
	f := &fakeProcesses{ps: []ProcessRecord{p}}
	f.inspect = func(int) (ProcessRecord, error) {
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		return p, nil
	}
	x.e.processes = f
	var got SweepResult
	x.mustOp(coord, "sweep", SweepArgs{}, &got)
	if len(f.signals) != 0 {
		t.Fatalf("recreated root authorized signals: %+v", f.signals)
	}
}
