package daemon

import (
	"context"
	"testing"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

func TestDispatchHoldAndInfraFailureRetainLeases(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:worker", "w1:other")
	var task store.Task
	x.mustOp(coord, "task.create", TaskCreateArgs{Title: "task"}, &task)
	var d store.Dispatch
	x.mustOp(coord, "dispatch", DispatchArgs{Task: task.ID, To: "w1:worker", Leases: []string{"sim:device-1"}}, &d)
	before, err := x.st.ListLeases()
	if err != nil || len(before) != 1 {
		t.Fatalf("leases %+v %v", before, err)
	}
	x.mustOp(coord, "dispatch.hold", DispatchHoldArgs{Task: task.ID, Until: "event:review", Reason: "review pending"}, nil)
	x.mustOp(coord, "dispatch.resume", DispatchHoldArgs{Task: task.ID}, nil)
	var settlement store.Settlement
	x.mustOp(coord, "dispatch.fail", DispatchActArgs{Task: task.ID, Reason: "account unavailable", Infra: true}, &settlement)
	if settlement.Task.Attempts != 0 || settlement.Task.Status != store.TaskReady {
		t.Fatal(settlement)
	}
	after, err := x.st.ListLeases()
	if err != nil || len(after) != 1 || after[0].Worker != before[0].Worker || after[0].ExpiresAt != before[0].ExpiresAt {
		t.Fatalf("infra failure freed or renewed leases: %+v %v", after, err)
	}
	var report store.CompletionReport
	x.mustOp("w1:worker", "send", SendArgs{Kind: store.KindDone, Task: task.ID, Body: "late test evidence"}, &report)
	if !report.Late {
		t.Fatal("expected a late report for fenced dispatch")
	}
	late, err := x.st.ListLeases()
	if err != nil || len(late) != 1 || late[0] != after[0] || x.task(task.ID).Attempts != 0 {
		t.Fatalf("late completion changed retained responsibility: %+v %v", late, err)
	}
	_, err = x.op(coord, "dispatch", DispatchArgs{Task: task.ID, To: "w1:other", Leases: []string{"sim:device-1"}})
	wantRefusal(t, err, "lease_held")
	if x.h.promptCount("w1:other") != 0 || x.task(task.ID).Attempts != 0 {
		t.Fatal("retry bypassed retained lease")
	}
}

func TestLifecycleHeldWorkerRefusesLeaseAndDispatchBeforeAdmission(t *testing.T) {
	x := newHarness(t)
	run := setupRun(x, "w1:worker")
	var task store.Task
	x.mustOp(coord, "task.create", TaskCreateArgs{Title: "task"}, &task)
	max := 1.0
	x.mustOp(coord, "run.update", RunUpdateArgs{Run: run.ID, MaxLoad: &max}, nil)
	x.e.cfg.SystemMetrics = func(context.Context) (HostMetrics, error) {
		t.Fatal("held worker reached host collection")
		return HostMetrics{}, nil
	}
	if err := x.st.BeginWorkerLifecycle("w1:worker", "restart"); err != nil {
		t.Fatal(err)
	}
	_, err := x.op(coord, "lease.acquire", LeaseArgs{Worker: "w1:worker", Resource: "port:7171"})
	wantRefusal(t, err, "worker_lifecycle_busy")
	_, err = x.op(coord, "dispatch", DispatchArgs{Task: task.ID, To: "w1:worker", Leases: []string{"db:test"}})
	wantRefusal(t, err, "worker_lifecycle_busy")
	ls, err := x.st.ListLeases()
	if err != nil || len(ls) != 0 || x.task(task.ID).Attempts != 0 {
		t.Fatalf("held refusal changed state: %+v %v", ls, err)
	}
}
