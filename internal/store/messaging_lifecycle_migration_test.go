package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMessagingUpgradePreservesReleasedLifecycleSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orch.db")
	db := must[*sql.DB](t)(sql.Open("sqlite", path))
	for _, migration := range migrations[:6] {
		if _, err := db.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`CREATE TABLE schema_version(v INTEGER NOT NULL)`,
		`INSERT INTO schema_version(v) VALUES(6)`,
		`INSERT INTO runs(id,title,status,created_at,updated_at) VALUES('r1','acme','running',1,1)`,
		`INSERT INTO tasks(id,run_id,title,status,created_at,updated_at) VALUES('t1','r1','acme task','dispatched',1,1)`,
		`INSERT INTO workers(pane_id,run_id,agent,state,started_at,agent_args,lifecycle,hand_off_to,hand_off_at,hand_off_worktree) VALUES('w1:p2','r1','codex','live',1,'["--model","model-a"]','restart_failed','w1:p3',2,'/tmp/acme')`,
		`INSERT INTO dispatches(id,task_id,run_id,pane_id,status,started_at,hold_event,hold_reason,held_by,usage_cap_notice,usage_cap_unknown) VALUES('d1','t1','r1','w1:p2','dispatched',1,'resume-event','intentional wait','w1:p1','session limit reached',1)`,
		`INSERT INTO messages(id,from_pane,to_pane,body,kind,created_at) VALUES('m1','w1:p1','w1:p2','retained mail','note',1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s := must[*Store](t)(Open(path))
	defer s.Close()
	w := must[Worker](t)(s.GetWorker("w1:p2"))
	if !w.LaunchKnown || !reflect.DeepEqual(w.AgentArgs, []string{"--model", "model-a"}) || w.Lifecycle != "restart_failed" || w.HandOffTo != "w1:p3" || w.HandOffAt != 2 {
		t.Fatalf("worker lifecycle lost: %+v", w)
	}
	d := must[Dispatch](t)(s.GetDispatch("d1"))
	if d.HoldEvent != "resume-event" || d.HoldReason != "intentional wait" || d.HeldBy != "w1:p1" || !d.UsageCapUnknown || d.UsageCapNotice != "session limit reached" {
		t.Fatalf("dispatch lifecycle lost: %+v", d)
	}
	m := must[Message](t)(s.GetMessage("m1"))
	if m.Body != "retained mail" || m.DeliveryStatus != "" {
		t.Fatalf("mail changed: %+v", m)
	}
	if w.Account != "" || w.Profile != "" || w.Model != "" || d.Account != "" {
		t.Fatal("migration invented historical identity")
	}
}
