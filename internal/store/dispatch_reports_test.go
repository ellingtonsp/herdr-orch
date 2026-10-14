package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCompletionReportPersistsHistoryWithoutChangingSettlement(t *testing.T) {
	p := filepath.Join(t.TempDir(), "orch.db")
	s := must[*Store](t)(Open(p))
	s.Now = func() time.Time { return time.Unix(100, 0) }
	r := must[Run](t)(s.CreateRun("Synthetic run", "w1:p1"))
	task := must[Task](t)(s.CreateTask(r.ID, "Synthetic task", "", nil))
	d := must[Dispatch](t)(s.BeginDispatch(task.ID, "w1:p2", "codex", ""))
	active := must[CompletionReport](t)(s.RecordDispatchReport(d.ID, Message{From: d.PaneID, Body: "initial report", Outcome: "succeeded"}))
	if active.Late || active.Message.Kind != KindDone || active.Dispatch.DoneMessageID != active.Message.ID {
		t.Fatalf("active report: %+v", active)
	}
	_, err := s.RecordDispatchReport(d.ID, Message{From: d.PaneID, Body: "duplicate"})
	refusal(t, err, "already_done")
	closed := must[Settlement](t)(s.Settle(d.ID, true, "completed", "original result"))
	for _, body := range []string{"late report one", "late report two"} {
		late := must[CompletionReport](t)(s.RecordDispatchReport(d.ID, Message{From: d.PaneID, Body: body, Outcome: "failed"}))
		if !late.Late || late.Message.Kind != KindNote || late.Message.Body != body || late.Dispatch != closed.Dispatch {
			t.Fatalf("late report: %+v", late)
		}
	}
	if task := must[Task](t)(s.GetTask(task.ID)); !reflect.DeepEqual(task, closed.Task) {
		t.Fatalf("late report mutated task: %+v", task)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = must[*Store](t)(Open(p))
	t.Cleanup(func() { s.Close() })
	reports := must[[]Message](t)(s.DispatchReports(d.ID))
	if len(reports) != 3 || reports[0].Body != "initial report" || reports[1].Body != "late report one" || reports[2].Body != "late report two" {
		t.Fatalf("persisted reports: %+v", reports)
	}
}

func TestCompletionReportRejectsWrongOrSupersededAssigneeAtomically(t *testing.T) {
	s := open(t)
	s.Now = func() time.Time { return time.Unix(100, 0) }
	r := must[Run](t)(s.CreateRun("Synthetic run", "w1:p1"))
	task := must[Task](t)(s.CreateTask(r.ID, "Synthetic task", "", nil))
	d := must[Dispatch](t)(s.BeginDispatch(task.ID, "w1:p2", "codex", ""))
	_, err := s.RecordDispatchReport(d.ID, Message{From: "human", Body: "claim"})
	refusal(t, err, "not_assignee")
	for i := 0; i < 10; i++ {
		must[Settlement](t)(s.Fence(d.ID, "superseded"))
		d = must[Dispatch](t)(s.BeginDispatch(task.ID, "w1:p2", "codex", ""))
	}
	ds := must[[]Dispatch](t)(s.DispatchesForTask(task.ID))
	if ds[len(ds)-1].ID != "d11" {
		t.Fatalf("latest dispatch sorted lexically: %+v", ds)
	}
	_, err = s.RecordDispatchReport(ds[0].ID, Message{From: "w1:p2", Body: "superseded report"})
	refusal(t, err, "not_latest_dispatch")
	if messages := must[[]Message](t)(s.RecentMessages(r.ID, 20)); len(messages) != 0 {
		t.Fatalf("refusal left orphan messages: %+v", messages)
	}
	// Force an association write failure: the message insert must roll back too.
	if _, err := s.db.Exec(`CREATE TRIGGER reject_report BEFORE INSERT ON dispatch_reports BEGIN SELECT RAISE(ABORT, 'synthetic write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordDispatchReport(d.ID, Message{From: d.PaneID, Body: "report"}); err == nil {
		t.Fatal("expected association failure")
	}
	if messages := must[[]Message](t)(s.RecentMessages(r.ID, 20)); len(messages) != 0 {
		t.Fatalf("association failure left orphan messages: %+v", messages)
	}
	if got := must[Dispatch](t)(s.GetDispatch(d.ID)); got.DoneMessageID != "" {
		t.Fatalf("association failure attached report: %+v", got)
	}
}

func TestWorkerMetadataUpgradeAndDispatchSnapshot(t *testing.T) {
	p := filepath.Join(t.TempDir(), "orch.db")
	db := must[*sql.DB](t)(sql.Open("sqlite", p))
	for _, migration := range migrations[:4] {
		if _, err := db.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{
		`CREATE TABLE schema_version(v INTEGER NOT NULL)`,
		`INSERT INTO schema_version(v) VALUES(4)`,
		`INSERT INTO workers(pane_id,state,started_at) VALUES('w1:p2','live',100)`,
		`INSERT INTO runs(id,title,status,created_at,updated_at) VALUES('r0','Synthetic previous run','completed',1,2)`,
		`INSERT INTO tasks(id,run_id,title,status,created_at,updated_at) VALUES('t0','r0','Synthetic previous task','completed',1,2)`,
		`INSERT INTO messages(id,run_id,from_pane,to_pane,body,kind,created_at) VALUES('m0','r0','w1:p2','horch','legacy report','done',100)`,
		`INSERT INTO dispatches(id,task_id,run_id,pane_id,status,started_at,done_message_id) VALUES('d0','t0','r0','w1:p2','completed',100,'m0')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s := must[*Store](t)(Open(p))
	t.Cleanup(func() { s.Close() })
	old := must[Worker](t)(s.GetWorker("w1:p2"))
	if old.Account != "" || old.Profile != "" || old.Model != "" || old.StartedAt != 100 {
		t.Fatalf("upgraded worker: %+v", old)
	}
	if reports := must[[]Message](t)(s.DispatchReports("d0")); len(reports) != 1 || reports[0].Body != "legacy report" {
		t.Fatalf("legacy report association was lost: %+v", reports)
	}
	r := must[Run](t)(s.CreateRun("Synthetic run", "w1:p1"))
	w := must[Worker](t)(s.RegisterWorker(Worker{PaneID: old.PaneID, RunID: r.ID, Agent: "codex", Account: "account-a", Profile: "profile-a", Model: "model-a"}))
	task := must[Task](t)(s.CreateTask(r.ID, "Synthetic task", "", nil))
	d := must[Dispatch](t)(s.BeginDispatch(task.ID, w.PaneID, w.Agent, ""))
	if d.Account != w.Account || d.Profile != w.Profile || d.Model != w.Model {
		t.Fatalf("dispatch metadata: %+v", d)
	}
	must[Worker](t)(s.RegisterWorker(Worker{PaneID: w.PaneID, RunID: r.ID, Account: "account-b", Profile: "profile-b", Model: "model-b"}))
	if got := must[Dispatch](t)(s.GetDispatch(d.ID)); got != d {
		t.Fatalf("revival altered historical metadata: %+v", got)
	}
	reset := must[Worker](t)(s.RegisterWorker(Worker{PaneID: w.PaneID, RunID: r.ID}))
	if reset.Account != "" || reset.Profile != "" || reset.Model != "" {
		t.Fatalf("revival retained stale metadata: %+v", reset)
	}
}
