package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDispatchHoldSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "holds.db")
	s := must[*Store](t)(Open(path))
	now := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	r := must[Run](t)(s.CreateRun("acme", "w1:coord"))
	task := must[Task](t)(s.CreateTask(r.ID, "ACME-12", "", nil))
	d := must[Dispatch](t)(s.BeginDispatch(task.ID, "w1:worker", "claude", ""))
	must[Dispatch](t)(s.HoldDispatch(d.ID, now.Add(time.Hour).UnixMilli(), "", "review pending", "w1:coord"))
	if err := s.SetUsageCapNotice(d.ID, "usage cap"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = must[*Store](t)(Open(path))
	defer s.Close()
	s.Now = func() time.Time { return now }
	d = must[Dispatch](t)(s.GetDispatch(d.ID))
	if !d.Held(now.UnixMilli()) || d.HoldReason != "review pending" || d.UsageCapNotice != "usage cap" {
		t.Fatalf("lost persisted state: %+v", d)
	}
	d = must[Dispatch](t)(s.ResumeDispatch(d.ID))
	if d.Held(now.UnixMilli()) || d.MonitorAfter != now.UnixMilli() {
		t.Fatalf("resume: %+v", d)
	}
	if got := must[Task](t)(s.GetTask(task.ID)); got.Attempts != 1 || got.Status != TaskDispatched {
		t.Fatalf("hold changed task: %+v", got)
	}
	must[Settlement](t)(s.Settle(d.ID, true, "done", ""))
	_, err := s.HoldDispatch(d.ID, now.Add(time.Hour).UnixMilli(), "", "wait", "human")
	refusal(t, err, "no_active_dispatch")
}

func TestUsageCapAndMessageAreAtomicAndDeduplicated(t *testing.T) {
	s := open(t)
	r := must[Run](t)(s.CreateRun("acme", "w1:coord"))
	task := must[Task](t)(s.CreateTask(r.ID, "ACME-12", "", nil))
	d := must[Dispatch](t)(s.BeginDispatch(task.ID, "w1:worker", "claude", ""))
	m := Message{RunID: r.ID, To: "w1:coord", From: Daemon, Kind: KindEscalation, Subject: "usage cap until unknown"}
	// A failing message write must roll back both signature and hold.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_cap_message BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'test failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordUsageCap(d.ID, "usage cap", 0, true, m); err == nil {
		t.Fatal("expected injected failure")
	}
	got := must[Dispatch](t)(s.GetDispatch(d.ID))
	if got.UsageCapNotice != "" || got.Held(s.now()) {
		t.Fatal("partial cap transaction persisted")
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_cap_message`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		must[Dispatch](t)(s.RecordUsageCap(d.ID, "usage cap", 0, true, m))
	}
	got = must[Dispatch](t)(s.GetDispatch(d.ID))
	messages := must[[]Message](t)(s.Unread([]string{"w1:coord"}, false))
	if got.HoldEvent != "usage-reset" || len(messages) != 1 {
		t.Fatalf("cap not exactly once: %+v %v", got, messages)
	}
}
