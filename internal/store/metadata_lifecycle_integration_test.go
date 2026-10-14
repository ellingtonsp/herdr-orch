package store

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestMetadataAndLaunchConfigurationSurviveRestartReplacementAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synthetic.db")
	s := must[*Store](t)(Open(path))
	r := must[Run](t)(s.CreateRun("Synthetic run", "w1:coordinator"))
	w := must[Worker](t)(s.RegisterWorker(Worker{
		PaneID: "w1:worker", RunID: r.ID, Name: "synthetic-worker", Agent: "codex",
		Account: "account-a", Profile: "profile-a", Model: "model-a", LaunchKnown: true,
		AgentArgs: []string{"--profile", "profile-a", "--model", "model-a"},
	}))
	task := must[Task](t)(s.CreateTask(r.ID, "Synthetic task", "", nil))
	old := must[Dispatch](t)(s.BeginDispatch(task.ID, w.PaneID, w.Agent, "synthetic-hash"))
	if err := s.BeginWorkerLifecycle(w.PaneID, "restart"); err != nil {
		t.Fatal(err)
	}
	next := must[Dispatch](t)(s.ReplaceWorkerDispatch(old.ID))
	if next.Account != old.Account || next.Profile != old.Profile || next.Model != old.Model || next.Model == "" {
		t.Fatalf("replacement lost launch labels: old=%+v next=%+v", old, next)
	}
	_, err := s.RegisterWorker(Worker{PaneID: w.PaneID, Model: "model-b", LaunchKnown: true})
	refusal(t, err, "worker_lifecycle_busy")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = must[*Store](t)(Open(path))
	t.Cleanup(func() { s.Close() })
	if err := s.RecoverWorkerLifecycles(); err != nil {
		t.Fatal(err)
	}
	got := must[Worker](t)(s.GetWorker(w.PaneID))
	if got.Lifecycle != "restart_failed" || !got.LaunchKnown || got.Account != w.Account || got.Profile != w.Profile || got.Model != w.Model || !reflect.DeepEqual(got.AgentArgs, w.AgentArgs) {
		t.Fatalf("recovery lost launch configuration: %+v", got)
	}
	if task := must[Task](t)(s.GetTask(task.ID)); task.Status != TaskDispatched || task.Attempts != 1 {
		t.Fatalf("replacement/recovery changed task ownership: %+v", task)
	}
	if got := must[Dispatch](t)(s.GetDispatch(next.ID)); got != next {
		t.Fatalf("replacement metadata changed on reopen: %+v", got)
	}
}

func TestHandOffRetainsDistinctSourceAndRecipientLaunchMetadata(t *testing.T) {
	s := open(t)
	source := must[Worker](t)(s.RegisterWorker(Worker{PaneID: "w1:helper", Account: "account-a", Profile: "profile-a", Model: "model-a", LaunchKnown: true, AgentArgs: []string{"--model", "model-a"}}))
	target := must[Worker](t)(s.RegisterWorker(Worker{PaneID: "w1:owner", Account: "account-b", Profile: "profile-b", Model: "model-b", LaunchKnown: true, AgentArgs: []string{"--model", "model-b"}}))
	if err := s.BeginWorkerLifecycle(source.PaneID, "releasing"); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginWorkerLifecycle(target.PaneID, "receiving_hand_off"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordWorkerHandOff(source.PaneID, target.PaneID, "synthetic-worktree"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWorkerState(source.PaneID, WorkerReleased, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWorkerLifecycle(target.PaneID, ""); err != nil {
		t.Fatal(err)
	}
	for _, before := range []Worker{source, target} {
		after := must[Worker](t)(s.GetWorker(before.PaneID))
		if after.Account != before.Account || after.Profile != before.Profile || after.Model != before.Model || !after.LaunchKnown || !reflect.DeepEqual(after.AgentArgs, before.AgentArgs) {
			t.Fatalf("handoff changed worker identity: before=%+v after=%+v", before, after)
		}
	}
}
