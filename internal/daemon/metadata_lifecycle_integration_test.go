package daemon

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

func TestWorkerRestartPreservesExplicitMetadataAndNormalizedLaunchArgs(t *testing.T) {
	x := newHarness(t)
	setupRun(x)
	x.h.addPane(coord, "codex", "idle", true)
	var w store.Worker
	x.mustOp(coord, "worker.start", WorkerStartArgs{
		Agent: "codex", Name: "synthetic-worker", Account: "account-a", Profile: "profile-a", Model: "model-a",
		Args: []string{"--reasoning", "high"},
	}, &w)
	wantArgs := []string{"--reasoning", "high", "--model", "model-a"}
	persisted, err := x.st.GetWorker(w.PaneID)
	if err != nil || !persisted.LaunchKnown || !reflect.DeepEqual(persisted.AgentArgs, wantArgs) {
		t.Fatalf("normalized launch configuration not stored: %+v %v", persisted, err)
	}
	var task store.Task
	x.mustOp(coord, "task.create", TaskCreateArgs{Title: "Synthetic task"}, &task)
	x.mustOp(coord, "dispatch", DispatchArgs{Task: task.ID, To: w.PaneID}, nil)
	f := &restartFake{fakeHerdr: x.h}
	x.e.h = f
	x.mustOp(coord, "worker.restart", WorkerRestartArgs{Worker: w.PaneID}, nil)
	got, err := x.st.GetWorker(w.PaneID)
	if err != nil || !reflect.DeepEqual(got.AgentArgs, wantArgs) || !reflect.DeepEqual(f.args, wantArgs) || got.Account != w.Account || got.Profile != w.Profile || got.Model != w.Model {
		t.Fatalf("restart lost launch identity: %+v args=%v err=%v", got, f.args, err)
	}
	x.mustOp(w.PaneID, "send", SendArgs{Kind: store.KindDone, Task: task.ID, Body: "restarted summary"}, nil)
	ms := x.inbox(coord)
	if len(ms) != 1 || ms[0].Kind != store.KindDone {
		t.Fatalf("restart completion: %+v", ms)
	}
	for _, token := range []string{`account="account-a"`, `profile="profile-a"`, `requested model="model-a"`} {
		if !strings.Contains(ms[0].Body, token) {
			t.Fatalf("restart completion lost %s: %s", token, ms[0].Body)
		}
	}
}

func TestWorkerReleaseHandOffPreservesBothWorkersLaunchIdentities(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:helper", "w1:owner")
	dir := gitWorktree(t)
	before := []store.Worker{}
	for i, pane := range []string{"w1:helper", "w1:owner"} {
		x.h.panes[pane].Cwd = &dir
		w, err := x.st.GetWorker(pane)
		if err != nil {
			t.Fatal(err)
		}
		w.Worktree, w.LaunchKnown = dir, true
		label := []string{"a", "b"}[i]
		w.Account, w.Profile, w.Model = "account-"+label, "profile-"+label, "model-"+label
		w.AgentArgs = []string{"--model", w.Model}
		w, err = x.st.RegisterWorker(w)
		if err != nil {
			t.Fatal(err)
		}
		before = append(before, w)
	}
	_, err := x.e.opWorkerRelease(context.Background(), coord, mustJSON(WorkerRef{Worker: before[0].PaneID, HandOffTo: before[1].PaneID}))
	if err != nil {
		t.Fatal(err)
	}
	for i, original := range before {
		got, err := x.st.GetWorker(original.PaneID)
		if err != nil || got.Account != original.Account || got.Profile != original.Profile || got.Model != original.Model || !reflect.DeepEqual(got.AgentArgs, original.AgentArgs) {
			t.Fatalf("handoff lost launch metadata: original=%+v got=%+v err=%v", original, got, err)
		}
		if i == 0 && (got.HandOffTo != before[1].PaneID || got.State != store.WorkerReleased) {
			t.Fatalf("handoff/release lost: %+v", got)
		}
		if i == 1 && (got.State != store.WorkerLive || got.Lifecycle != "") {
			t.Fatalf("recipient lifecycle not restored: %+v", got)
		}
	}
}
