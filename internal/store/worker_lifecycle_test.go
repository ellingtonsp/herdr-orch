package store

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestWorkerLifecyclePersistedLaunchAndCrashRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acme.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	workers := []Worker{{PaneID: "w1:acme-restart", Name: "acme", Agent: "codex", LaunchKnown: true, AgentArgs: []string{"--model", "acme-model"}}, {PaneID: "w1:acme-release"}, {PaneID: "w1:acme-recipient"}, {PaneID: "w1:acme-adopted"}}
	for _, w := range workers {
		if _, err := s.RegisterWorker(w); err != nil {
			t.Fatal(err)
		}
	}
	for pane, phase := range map[string]string{"w1:acme-restart": "restart", "w1:acme-release": "releasing", "w1:acme-recipient": "receiving_hand_off"} {
		if err := s.BeginWorkerLifecycle(pane, phase); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecoverWorkerLifecycles(); err != nil {
		t.Fatal(err)
	}
	w, err := s.GetWorker("w1:acme-restart")
	if err != nil {
		t.Fatal(err)
	}
	if w.Lifecycle != "restart_failed" || !w.LaunchKnown || !reflect.DeepEqual(w.AgentArgs, workers[0].AgentArgs) {
		t.Fatalf("lost durable launch/hold: %+v", w)
	}
	if err := s.BeginWorkerLifecycle(w.PaneID, "restart"); err != nil {
		t.Fatal(err)
	}
	w, err = s.GetWorker("w1:acme-release")
	if err != nil {
		t.Fatal(err)
	}
	if w.Lifecycle != "release_interrupted" {
		t.Fatalf("interrupted release not held: %+v", w)
	}
	if err := s.BeginWorkerLifecycle(w.PaneID, "releasing"); err != nil {
		t.Fatal(err)
	}
	if s.WorkerLifecycleHeld("w1:acme-recipient") {
		t.Fatal("recipient orphaned")
	}
	w, err = s.GetWorker("w1:acme-adopted")
	if err != nil || w.LaunchKnown {
		t.Fatalf("adopted launch invented: %+v %v", w, err)
	}
}

func TestWorkerLifecycleReplaceDispatchNeverExposesReadyTask(t *testing.T) {
	s := open(t)
	r := must[Run](t)(s.CreateRun("acme run", "w1:acme-coordinator"))
	w := must[Worker](t)(s.RegisterWorker(Worker{PaneID: "w1:acme-worker", RunID: r.ID, LaunchKnown: true}))
	task := must[Task](t)(s.CreateTask(r.ID, "acme task", "acme spec", nil))
	old := must[Dispatch](t)(s.BeginDispatch(task.ID, w.PaneID, "codex", "acme-hash"))
	if err := s.BeginWorkerLifecycle(w.PaneID, "restart"); err != nil {
		t.Fatal(err)
	}
	next := must[Dispatch](t)(s.ReplaceWorkerDispatch(old.ID))
	got := must[Task](t)(s.GetTask(task.ID))
	if got.Status != TaskDispatched || got.Attempts != 1 || next.TaskID != task.ID || next.ID == old.ID {
		t.Fatalf("task claim changed: %+v %+v", got, next)
	}
	ready := must[[]Task](t)(s.ReadyTasks(r.ID))
	if len(ready) != 0 {
		t.Fatal("restart exposed ready task")
	}
	old = must[Dispatch](t)(s.GetDispatch(old.ID))
	if old.Active() {
		t.Fatal("old dispatch active")
	}
	refusal(t, s.BeginWorkerLifecycle(w.PaneID, "releasing"), "worker_lifecycle_busy")
	_, err := s.RegisterWorker(w)
	refusal(t, err, "worker_lifecycle_busy")
}
