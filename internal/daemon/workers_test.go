package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ellingtonsp/herdr-orch/internal/herdr"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

type workerPaneHerdr struct {
	*fakeHerdr
	worktree     string
	created      bool
	selected     herdr.Pane
	selectErr    error
	startErr     error
	startPane    string
	tabWorkspace string
	tabCwd       string
}

func (h *workerPaneHerdr) WorktreePane(_ context.Context, cwd, _ string, _ map[string]string) (herdr.Pane, bool, error) {
	h.worktree = cwd
	return h.selected, h.created, h.selectErr
}

func (h *workerPaneHerdr) NewTab(ctx context.Context, ws, cwd, label string, env map[string]string) (herdr.Pane, error) {
	h.tabWorkspace, h.tabCwd = ws, cwd
	return h.fakeHerdr.NewTab(ctx, ws, cwd, label, env)
}

func (h *workerPaneHerdr) AgentStart(ctx context.Context, name, kind, pane string, args []string) (herdr.Pane, error) {
	h.startPane = pane
	if h.startErr != nil {
		return herdr.Pane{}, h.startErr
	}
	return h.fakeHerdr.AgentStart(ctx, name, kind, pane, args)
}

func TestWorkerStartUsesWorktreeWorkspace(t *testing.T) {
	x := newHarness(t)
	setupRun(x)
	x.h.addPane(coord, "codex", "idle", true)
	dir := t.TempDir()
	x.h.addPane("w2:p1", "", "unknown", false)
	x.h.panes["w2:p1"].WorkspaceID = "w2"
	h := &workerPaneHerdr{fakeHerdr: x.h, selected: *x.h.panes["w2:p1"]}
	x.e.h = h
	// A coordinator widget or stale caller context must not choose the target workspace.
	x.h.panes[coord].WorkspaceID = ""
	var w store.Worker
	x.mustOp(coord, "worker.start", WorkerStartArgs{Agent: "codex", Name: "you", Worktree: dir}, &w)
	if w.PaneID != "w2:p1" || w.Worktree != dir || h.worktree != dir || h.startPane != "w2:p1" || x.h.nextPane != 0 {
		t.Fatalf("worker=%+v worktree=%q start=%q tabs=%d", w, h.worktree, h.startPane, x.h.nextPane)
	}
}

func TestWorkerStartPreservesLegacyCwd(t *testing.T) {
	x := newHarness(t)
	setupRun(x)
	x.h.addPane(coord, "codex", "idle", true)
	dir := t.TempDir()
	h := &workerPaneHerdr{fakeHerdr: x.h}
	x.e.h = h
	var w store.Worker
	x.mustOp(coord, "worker.start", WorkerStartArgs{Agent: "codex", Name: "you", Cwd: dir}, &w)
	if h.worktree != "" || h.tabWorkspace != "w1" || h.tabCwd != dir || w.Worktree != dir {
		t.Fatalf("legacy cwd worker=%+v worktree=%q tabWorkspace=%q tabCwd=%q", w, h.worktree, h.tabWorkspace, h.tabCwd)
	}
}

func TestWorkerStartFailureOnlyClosesCreatedWorktreePane(t *testing.T) {
	for _, created := range []bool{false, true} {
		t.Run(map[bool]string{false: "reused", true: "created"}[created], func(t *testing.T) {
			x := newHarness(t)
			setupRun(x)
			x.h.addPane("w2:p1", "", "unknown", false)
			h := &workerPaneHerdr{fakeHerdr: x.h, selected: *x.h.panes["w2:p1"], created: created, startErr: errors.New("synthetic startup failure")}
			x.e.h = h
			_, err := x.op(coord, "worker.start", WorkerStartArgs{Agent: "codex", Name: "you", Worktree: t.TempDir()})
			if err == nil {
				t.Fatal("expected startup error")
			}
			if got := len(x.h.closed); (got == 1) != created {
				t.Fatalf("closed panes=%v created=%v", x.h.closed, created)
			}
			workers, err := x.st.ListWorkers("", false)
			if err != nil || len(workers) != 0 {
				t.Fatalf("registered failed start: %+v %v", workers, err)
			}
		})
	}
}

func TestWorkerStartSelectionFailureDoesNotStartAgent(t *testing.T) {
	x := newHarness(t)
	setupRun(x)
	h := &workerPaneHerdr{fakeHerdr: x.h, selectErr: errors.New("synthetic workspace failure")}
	x.e.h = h
	_, err := x.op(coord, "worker.start", WorkerStartArgs{Agent: "codex", Name: "you", Worktree: t.TempDir()})
	if err == nil || h.startPane != "" || x.h.nextPane != 0 || len(x.h.closed) != 0 {
		t.Fatalf("err=%v started=%q tabs=%d closed=%v", err, h.startPane, x.h.nextPane, x.h.closed)
	}
}

func TestWorkerStartCleansCreatedPaneWhenShellCheckFails(t *testing.T) {
	x := newHarness(t)
	setupRun(x)
	x.h.addPane("w2:p1", "", "unknown", false)
	h := &workerPaneHerdr{fakeHerdr: x.h, selected: *x.h.panes["w2:p1"], created: true, selectErr: errors.New("synthetic shell check failure")}
	x.e.h = h
	_, err := x.op(coord, "worker.start", WorkerStartArgs{Agent: "codex", Name: "you", Worktree: t.TempDir()})
	if err == nil || h.startPane != "" || len(x.h.closed) != 1 || x.h.closed[0] != "w2:p1" {
		t.Fatalf("err=%v start=%q closed=%v", err, h.startPane, x.h.closed)
	}
}

func TestWorkerStartRefusesNonShellExplicitPane(t *testing.T) {
	for _, tt := range []struct {
		name, workspace string
		pids            []int
		code            string
	}{
		{"missing workspace", "", nil, "unknown_pane"},
		{"widget", "w2", nil, "pane_busy"},
		{"foreground command", "w2", []int{2_000_000_000, 2_000_000_001}, "pane_busy"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			x := newHarness(t)
			setupRun(x)
			x.h.addPane("w2:p1", "", "unknown", false)
			x.h.panes["w2:p1"].WorkspaceID = tt.workspace
			x.h.pids["w2:p1"] = tt.pids
			_, err := x.op(coord, "worker.start", WorkerStartArgs{Agent: "codex", Name: "you", Pane: "w2:p1"})
			wantRefusal(t, err, tt.code)
			if len(x.h.closed) != 0 || x.h.named["w2:p1"] {
				t.Fatal("refused pane was changed")
			}
		})
	}
}

func TestWorkerStartRefusesConflictingWorktreeArguments(t *testing.T) {
	x := newHarness(t)
	setupRun(x)
	for _, args := range []WorkerStartArgs{
		{Agent: "codex", Worktree: t.TempDir(), Cwd: t.TempDir()},
		{Agent: "codex", Worktree: t.TempDir(), Pane: "w2:p1"},
	} {
		_, err := x.op(coord, "worker.start", args)
		wantRefusal(t, err, "bad_args")
	}
}

func TestWorkerStartRefusesCallerWithoutWorkspace(t *testing.T) {
	x := newHarness(t)
	setupRun(x)
	x.h.addPane(coord, "codex", "idle", true)
	x.h.panes[coord].WorkspaceID = ""
	_, err := x.op(coord, "worker.start", WorkerStartArgs{Agent: "codex", Name: "you"})
	wantRefusal(t, err, "unknown_pane")
	if x.h.nextPane != 0 {
		t.Fatal("created tab with no caller workspace")
	}
}

func TestWorkerStartRefusesMissingWorktreeDirectory(t *testing.T) {
	x := newHarness(t)
	setupRun(x)
	_, err := x.op(coord, "worker.start", WorkerStartArgs{Agent: "codex", Name: "you", Worktree: filepath.Join(t.TempDir(), "myapp")})
	wantRefusal(t, err, "bad_cwd")
	if x.h.nextPane != 0 {
		t.Fatal("created a pane for a missing checkout")
	}
}
