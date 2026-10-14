package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

func TestSweepRejectsBroadRecordedHome(t *testing.T) {
	x := newHarness(t)
	setupRun(x)
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, err := x.st.RegisterWorker(store.Worker{PaneID: "w1:old", Worktree: home})
	if err != nil {
		t.Fatal(err)
	}
	if err := x.st.SetWorkerState("w1:old", store.WorkerReleased, "", ""); err != nil {
		t.Fatal(err)
	}
	f := &fakeProcesses{ps: []ProcessRecord{
		{PID: 100001, Started: "1", Command: "/System/Library/CoreServices/MenuBarAgent", Cwd: "/"},
		{PID: 100002, Started: "2", Command: "/System/Library/CoreServices/WallpaperAgent", Cwd: filepath.Join(home, "Library")},
		{PID: 100003, Started: "3", Command: "shell", Cwd: home},
		{PID: 100004, Started: "4", Command: "server", Cwd: filepath.Join(home, "unrelated")},
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
	x.e.processes = &fakeProcesses{ps: []ProcessRecord{{PID: 100001, Started: "1", Command: "server", Cwd: root}}}
	var got SweepResult
	x.mustOp(coord, "sweep", SweepArgs{DryRun: true}, &got)
	if len(got.Processes) != 0 {
		t.Fatalf("stopped worker authorized sweep: %+v", got.Processes)
	}
}

func TestCleanupRejectsHomeScope(t *testing.T) {
	home, _ := processRoot(t.TempDir())
	t.Setenv("HOME", home)
	f := &fakeProcesses{ps: []ProcessRecord{{PID: 100001, Started: "1", Command: "shell", Cwd: home}}}
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
	f := &fakeProcesses{ps: []ProcessRecord{{PID: 100001, Started: "1", Command: "server", Cwd: link}}}
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
