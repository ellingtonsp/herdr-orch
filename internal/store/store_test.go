package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "orch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func refusal(t *testing.T, err error, code string) {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) || r.Code != code {
		t.Fatalf("want refusal %s, got %v", code, err)
	}
}

func status(t *testing.T, s *Store, id string) string {
	t.Helper()
	return must[Task](t)(s.GetTask(id)).Status
}

func TestMigrateIsIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "orch.db")
	s := must[*Store](t)(Open(p))
	s.Close()
	s = must[*Store](t)(Open(p))
	s.Close()
}

func TestRunsAndCurrentRun(t *testing.T) {
	s := open(t)
	_, err := s.CurrentRun("w1:p1")
	refusal(t, err, "no_run")
	r1 := must[Run](t)(s.CreateRun("one", "w1:p1"))
	r2 := must[Run](t)(s.CreateRun("two", ""))
	if r1.ID != "r1" || r2.ID != "r2" || r1.Status != RunRunning {
		t.Fatalf("ids/status: %+v %+v", r1, r2)
	}
	if got := must[Run](t)(s.CurrentRun("w1:p1")); got.ID != "r1" {
		t.Fatalf("bound pane should use r1, got %s", got.ID)
	}
	if got := must[Run](t)(s.CurrentRun("w9:p9")); got.ID != "r2" {
		t.Fatalf("unbound pane should use newest running run, got %s", got.ID)
	}
	if err := s.UseRun("w9:p9", "r1"); err != nil {
		t.Fatal(err)
	}
	if got := must[Run](t)(s.CurrentRun("w9:p9")); got.ID != "r1" {
		t.Fatalf("after use: %s", got.ID)
	}
}

func TestMessagesUnreadAndReplies(t *testing.T) {
	s := open(t)
	q := must[Message](t)(s.InsertMessage(Message{From: "w1:p2", To: "w1:p1", Kind: KindQuestion, Body: "which db?"}))
	must[Message](t)(s.InsertMessage(Message{From: "w1:p3", To: "w1:p1", Kind: KindNote, Body: "hi"}))
	_, err := s.InsertMessage(Message{From: "a", To: "b", Kind: "bogus"})
	refusal(t, err, "bad_kind")

	if n := must[int](t)(s.UnreadCount("w1:p1")); n != 2 {
		t.Fatalf("unread=%d", n)
	}
	got := must[[]Message](t)(s.Unread([]string{"w1:p1"}, true))
	if len(got) != 2 || got[0].ID != q.ID {
		t.Fatalf("unread order: %+v", got)
	}
	if n := must[int](t)(s.UnreadCount("w1:p1")); n != 0 {
		t.Fatalf("unread after mark=%d", n)
	}
	if _, err := s.TakeReply(q.ID); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	must[Message](t)(s.InsertMessage(Message{From: "w1:p1", To: "w1:p2", Kind: KindReply, ReplyTo: q.ID, Body: "sqlite"}))
	r := must[Message](t)(s.TakeReply(q.ID))
	if r.Body != "sqlite" || r.ReadAt == 0 {
		t.Fatalf("reply: %+v", r)
	}
	if n := must[int](t)(s.UnreadCount("w1:p2")); n != 0 {
		t.Fatalf("taken reply should be read, unread=%d", n)
	}
}

func TestDependencyReadiness(t *testing.T) {
	s := open(t)
	r := must[Run](t)(s.CreateRun("dag", "c"))
	a := must[Task](t)(s.CreateTask(r.ID, "A", "", nil))
	b := must[Task](t)(s.CreateTask(r.ID, "B", "", []string{a.ID}))
	c := must[Task](t)(s.CreateTask(r.ID, "C", "", []string{a.ID, b.ID, a.ID}))
	if a.Status != TaskReady || b.Status != TaskPending || c.Status != TaskPending {
		t.Fatalf("initial: %s %s %s", a.Status, b.Status, c.Status)
	}
	if len(c.Deps) != 2 {
		t.Fatalf("deps not deduped: %v", c.Deps)
	}
	_, err := s.CreateTask(r.ID, "X", "", []string{"t99"})
	refusal(t, err, "unknown_task")

	d := must[Dispatch](t)(s.BeginDispatch(a.ID, "w", "pi", "h"))
	if status(t, s, a.ID) != TaskDispatched {
		t.Fatal("A should be dispatched")
	}
	st := must[Settlement](t)(s.Settle(d.ID, true, "ok", "A done"))
	if st.Task.Status != TaskCompleted || st.Task.Result != "A done" || st.Dispatch.Status != DispatchCompleted {
		t.Fatalf("settle: %+v", st)
	}
	if len(st.NowReady) != 1 || st.NowReady[0] != b.ID {
		t.Fatalf("now ready: %v", st.NowReady)
	}
	if status(t, s, c.ID) != TaskPending {
		t.Fatal("C still needs B")
	}
}

func TestDepCycleRefused(t *testing.T) {
	s := open(t)
	r := must[Run](t)(s.CreateRun("cyc", ""))
	a := must[Task](t)(s.CreateTask(r.ID, "A", "", nil))
	b := must[Task](t)(s.CreateTask(r.ID, "B", "", []string{a.ID}))
	_, _, err := s.UpdateTask(a.ID, TaskUpdate{Deps: &[]string{b.ID}})
	refusal(t, err, "dep_cycle")
	_, _, err = s.UpdateTask(a.ID, TaskUpdate{Deps: &[]string{a.ID}})
	refusal(t, err, "dep_cycle")
}

func TestGateBlocksTask(t *testing.T) {
	s := open(t)
	r := must[Run](t)(s.CreateRun("gate", ""))
	a := must[Task](t)(s.CreateTask(r.ID, "A", "", nil))
	g := must[Gate](t)(s.CreateGate("", a.ID, "ship it?", []string{"yes", "no"}, 0))
	if g.RunID != r.ID || status(t, s, a.ID) != TaskBlocked {
		t.Fatalf("gate should block A: %s", status(t, s, a.ID))
	}
	_, err := s.BeginDispatch(a.ID, "w", "", "")
	refusal(t, err, "task_not_ready")
	_, _, err = s.ResolveGate(g.ID, "maybe", "human")
	refusal(t, err, "bad_decision")
	g2, ready, err := s.ResolveGate(g.ID, "yes", "human")
	if err != nil {
		t.Fatal(err)
	}
	if g2.Status != GateResolved || g2.Decision != "yes" || len(ready) != 1 {
		t.Fatalf("resolve: %+v ready=%v", g2, ready)
	}
	_, _, err = s.ResolveGate(g.ID, "no", "human")
	refusal(t, err, "gate_resolved")

	// A gate cannot be put in front of a dispatched task.
	must[Dispatch](t)(s.BeginDispatch(a.ID, "w", "", ""))
	_, err = s.CreateGate("", a.ID, "late?", nil, 0)
	refusal(t, err, "task_not_gateable")
}

func TestGateTimeoutKeepsTaskBlocked(t *testing.T) {
	s := open(t)
	now := time.Unix(1000, 0)
	s.Now = func() time.Time { return now }
	r := must[Run](t)(s.CreateRun("gate", ""))
	a := must[Task](t)(s.CreateTask(r.ID, "A", "", nil))
	g := must[Gate](t)(s.CreateGate("", a.ID, "?", nil, now.Add(time.Minute).UnixMilli()))
	if exp := must[[]Gate](t)(s.ExpireGates()); len(exp) != 0 {
		t.Fatal("expired early")
	}
	now = now.Add(2 * time.Minute)
	exp := must[[]Gate](t)(s.ExpireGates())
	if len(exp) != 1 || exp[0].ID != g.ID || exp[0].Status != GateTimeout {
		t.Fatalf("expire: %+v", exp)
	}
	if status(t, s, a.ID) != TaskBlocked {
		t.Fatal("timed-out gate must keep its task blocked")
	}
	_, ready, err := s.ResolveGate(g.ID, "go", "human")
	if err != nil || len(ready) != 1 {
		t.Fatalf("resolve after timeout: %v %v", ready, err)
	}
}

func TestCircuitBreaker(t *testing.T) {
	s := open(t)
	r := must[Run](t)(s.CreateRun("cb", ""))
	max := 2
	must[Run](t)(s.UpdateRun(r.ID, RunUpdate{MaxAttempts: &max}))
	a := must[Task](t)(s.CreateTask(r.ID, "A", "", nil))

	d1 := must[Dispatch](t)(s.BeginDispatch(a.ID, "w1", "", ""))
	st := must[Settlement](t)(s.Settle(d1.ID, false, "failed: silent", ""))
	if st.Dispatch.Status != DispatchFailed || st.Task.Status != TaskReady || st.Task.Attempts != 1 {
		t.Fatalf("first failure: %+v", st)
	}
	d2 := must[Dispatch](t)(s.BeginDispatch(a.ID, "w2", "", ""))
	st = must[Settlement](t)(s.Settle(d2.ID, false, "failed: pane_exited", ""))
	if st.Dispatch.Status != DispatchCircuitBroken || st.Task.Status != TaskFailed {
		t.Fatalf("second failure should break the circuit: %+v", st)
	}
	_, _, err := s.UpdateTask(a.ID, TaskUpdate{Status: ptr(TaskReady)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.BeginDispatch(a.ID, "w3", "", "")
	refusal(t, err, "circuit_broken")
	must[Task](t)(func() (Task, error) {
		t, _, err := s.UpdateTask(a.ID, TaskUpdate{ResetAttempts: true})
		return t, err
	}())
	must[Dispatch](t)(s.BeginDispatch(a.ID, "w3", "", ""))
}

func TestDispatchGuards(t *testing.T) {
	s := open(t)
	r := must[Run](t)(s.CreateRun("g", ""))
	a := must[Task](t)(s.CreateTask(r.ID, "A", "", nil))
	b := must[Task](t)(s.CreateTask(r.ID, "B", "", nil))
	d := must[Dispatch](t)(s.BeginDispatch(a.ID, "w1", "", ""))
	_, err := s.BeginDispatch(a.ID, "w2", "", "")
	refusal(t, err, "task_not_ready")
	_, err = s.BeginDispatch(b.ID, "w1", "", "")
	refusal(t, err, "pane_busy")
	if got := must[Dispatch](t)(s.ActiveDispatchForPane("w1")); got.ID != d.ID {
		t.Fatal("active dispatch lookup")
	}
	_, _, err = s.UpdateTask(a.ID, TaskUpdate{Status: ptr(TaskCompleted)})
	refusal(t, err, "task_dispatched")
	must[Settlement](t)(s.Settle(d.ID, true, "", ""))
	_, err = s.Settle(d.ID, true, "", "")
	refusal(t, err, "dispatch_settled")
}

func TestFenceDoesNotCountAttempt(t *testing.T) {
	s := open(t)
	r := must[Run](t)(s.CreateRun("f", ""))
	a := must[Task](t)(s.CreateTask(r.ID, "A", "", nil))
	d := must[Dispatch](t)(s.BeginDispatch(a.ID, "w1", "", ""))
	st := must[Settlement](t)(s.Fence(d.ID, "worker stopped"))
	if st.Task.Status != TaskReady || st.Task.Attempts != 0 || st.Dispatch.Status != DispatchFailed {
		t.Fatalf("fence: %+v", st)
	}
	if _, err := s.ActiveDispatchForPane("w1"); err != ErrNotFound {
		t.Fatal("fenced dispatch must not stay active")
	}
}

func TestObserveStatus(t *testing.T) {
	s := open(t)
	r := must[Run](t)(s.CreateRun("o", ""))
	a := must[Task](t)(s.CreateTask(r.ID, "A", "", nil))
	d := must[Dispatch](t)(s.BeginDispatch(a.ID, "w1", "", ""))
	d = must[Dispatch](t)(s.ObserveStatus(d.ID, "idle"))
	if d.SawWorking || d.IdleSince == 0 {
		t.Fatalf("idle: %+v", d)
	}
	d = must[Dispatch](t)(s.ObserveStatus(d.ID, "working"))
	if !d.SawWorking || d.IdleSince != 0 {
		t.Fatalf("working: %+v", d)
	}
}

func TestWorkersAndMembers(t *testing.T) {
	s := open(t)
	r := must[Run](t)(s.CreateRun("w", "c1"))
	must[Worker](t)(s.RegisterWorker(Worker{PaneID: "w1:p2", RunID: r.ID, Name: "alpha", Agent: "pi"}))
	must[Worker](t)(s.RegisterWorker(Worker{PaneID: "w1:p3", RunID: r.ID, Name: "beta", Agent: "codex"}))
	if w := must[Worker](t)(s.FindWorker("alpha")); w.PaneID != "w1:p2" {
		t.Fatal("find by name")
	}
	if err := s.SetWorkerState("w1:p3", WorkerReleased, "done", "/tmp/a"); err != nil {
		t.Fatal(err)
	}
	m := must[[]string](t)(s.RunMembers(r.ID))
	if len(m) != 2 || m[0] != "c1" || m[1] != "w1:p2" {
		t.Fatalf("members: %v", m)
	}
	live := must[[]Worker](t)(s.ListWorkers(r.ID, true))
	if len(live) != 1 {
		t.Fatalf("live: %+v", live)
	}
}

func ptr[T any](v T) *T { return &v }
