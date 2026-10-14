package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// These are the released upstream additions, kept independently of migrations so
// changing migration order cannot turn upgrade coverage into a self-fulfilling fixture.
const releasedHolds = `ALTER TABLE dispatches ADD COLUMN hold_until INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dispatches ADD COLUMN hold_event TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatches ADD COLUMN hold_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatches ADD COLUMN held_by TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatches ADD COLUMN monitor_after INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dispatches ADD COLUMN usage_cap_notice TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatches ADD COLUMN usage_cap_until INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dispatches ADD COLUMN usage_cap_unknown INTEGER NOT NULL DEFAULT 0;`
const releasedLifecycle = `ALTER TABLE workers ADD COLUMN agent_args TEXT;
ALTER TABLE workers ADD COLUMN lifecycle TEXT NOT NULL DEFAULT '';
ALTER TABLE workers ADD COLUMN hand_off_to TEXT;
ALTER TABLE workers ADD COLUMN hand_off_at INTEGER;
ALTER TABLE workers ADD COLUMN hand_off_worktree TEXT;`

const releasedMetadataReports = `ALTER TABLE workers ADD COLUMN account TEXT;
ALTER TABLE workers ADD COLUMN profile TEXT;
ALTER TABLE workers ADD COLUMN model TEXT;
ALTER TABLE dispatches ADD COLUMN account TEXT;
ALTER TABLE dispatches ADD COLUMN profile TEXT;
ALTER TABLE dispatches ADD COLUMN model TEXT;
CREATE TABLE dispatch_reports(dispatch_id TEXT NOT NULL REFERENCES dispatches(id),message_id TEXT PRIMARY KEY REFERENCES messages(id),late INTEGER NOT NULL DEFAULT 0);
INSERT INTO dispatch_reports(dispatch_id,message_id,late) SELECT id,done_message_id,0 FROM dispatches WHERE done_message_id IN (SELECT id FROM messages);
CREATE INDEX dispatch_reports_dispatch ON dispatch_reports(dispatch_id);`
const releasedDelivery = `ALTER TABLE messages ADD COLUMN delivery_status TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN delivery_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN delivered_at INTEGER;
ALTER TABLE messages ADD COLUMN picked_up_at INTEGER;
ALTER TABLE messages ADD COLUMN pickup_via TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN delivery_error TEXT NOT NULL DEFAULT '';`

func TestUpgradeReleasedMainPreservesHoldsAndLifecycle(t *testing.T) {
	for _, version := range []int{5, 6, 7, 8} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "orch.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TABLE schema_version(v INTEGER NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			for i, m := range append(append([]string{}, migrations[:4]...), releasedHolds, releasedLifecycle, releasedMetadataReports, releasedDelivery) {
				if i >= version {
					break
				}
				if _, err := db.Exec(m); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO schema_version(v) VALUES(?)`, i+1); err != nil {
					t.Fatal(err)
				}
			}
			fixtures := []string{
				`INSERT INTO runs(id,title,status,created_at,updated_at) VALUES('r1','existing run','running',1,2)`,
				`INSERT INTO tasks(id,run_id,title,status,attempts,created_at,updated_at) VALUES('t1','r1','existing task','dispatched',1,1,2)`,
				`INSERT INTO workers(pane_id,run_id,name,state,started_at) VALUES('w1:worker','r1','existing worker','live',1)`,
				`INSERT INTO dispatches(id,task_id,run_id,pane_id,status,started_at,hold_event,hold_reason,held_by,usage_cap_notice,usage_cap_unknown) VALUES('d1','t1','r1','w1:worker','dispatched',1,'ci-green','review pending','w1:coord','cap warning',1)`,
			}
			if version >= 6 {
				fixtures = append(fixtures, `UPDATE workers SET agent_args='["--model","example-model"]',lifecycle='restart_failed',hand_off_to='w1:recipient',hand_off_at=42,hand_off_worktree='/tmp/example' WHERE pane_id='w1:worker'`)
			}
			fixtures = append(fixtures,
				`INSERT INTO messages(id,run_id,from_pane,to_pane,body,kind,created_at) VALUES('m1','r1','w1:worker','horch','retained completion report','done',11)`,
				`UPDATE dispatches SET done_message_id='m1' WHERE id='d1'`,
				`INSERT INTO messages(id,run_id,from_pane,to_pane,body,kind,created_at) VALUES('m2','r1','w1:coord','w1:worker','retained coordinator message','note',12)`,
			)
			if version >= 7 {
				fixtures = append(fixtures,
					`UPDATE workers SET account='worker-account',profile='worker-profile',model='worker-model' WHERE pane_id='w1:worker'`,
					`UPDATE dispatches SET account='dispatch-account',profile='dispatch-profile',model='dispatch-model' WHERE id='d1'`,
					`INSERT INTO dispatch_reports(dispatch_id,message_id,late) VALUES('d1','m1',0)`,
				)
			}
			if version >= 8 {
				fixtures = append(fixtures, `UPDATE messages SET delivery_status='picked_up',delivery_attempts=2,delivered_at=15,picked_up_at=17,pickup_via='event',delivery_error='' WHERE id='m2'`)
			}
			for _, q := range fixtures {
				if _, err := db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			s := must[*Store](t)(Open(path))
			defer s.Close()
			r := must[Run](t)(s.GetRun("r1"))
			if r.Title != "existing run" || r.MaxLoad != 0 || r.MinFreeMem != 0 {
				t.Fatal(r)
			}
			d := must[Dispatch](t)(s.GetDispatch("d1"))
			if d.Status != DispatchDispatched || d.HoldEvent != "ci-green" || d.HoldReason != "review pending" || d.UsageCapNotice != "cap warning" || !d.UsageCapUnknown {
				t.Fatal(d)
			}
			w := must[Worker](t)(s.GetWorker("w1:worker"))
			if version >= 6 && (!w.LaunchKnown || len(w.AgentArgs) != 2 || w.Lifecycle != "restart_failed" || w.HandOffTo != "w1:recipient" || w.HandOffAt != 42 || w.HandOffWorktree != "/tmp/example") {
				t.Fatalf("lifecycle data lost: %+v", w)
			}
			if version >= 7 {
				if w.Account != "worker-account" || w.Profile != "worker-profile" || w.Model != "worker-model" || d.Account != "dispatch-account" || d.Profile != "dispatch-profile" || d.Model != "dispatch-model" {
					t.Fatalf("metadata changed: worker=%+v dispatch=%+v", w, d)
				}
			} else if w.Account != "" || w.Profile != "" || w.Model != "" || d.Account != "" || d.Profile != "" || d.Model != "" {
				t.Fatal("upgrade invented historical metadata")
			}
			reports := must[[]Message](t)(s.DispatchReports("d1"))
			if len(reports) != 1 || reports[0].ID != "m1" || reports[0].Body != "retained completion report" {
				t.Fatalf("report history changed: %+v", reports)
			}
			m := must[Message](t)(s.GetMessage("m2"))
			if m.Body != "retained coordinator message" {
				t.Fatalf("mail content changed: %+v", m)
			}
			if version >= 8 {
				if m.DeliveryStatus != "picked_up" || m.DeliveryAttempts != 2 || m.DeliveredAt != 15 || m.PickedUpAt != 17 || m.PickupVia != "event" || m.DeliveryError != "" {
					t.Fatalf("delivery receipt changed: %+v", m)
				}
			} else if m.DeliveryStatus != "" || m.DeliveryAttempts != 0 || m.DeliveredAt != 0 || m.PickedUpAt != 0 || m.PickupVia != "" || m.DeliveryError != "" {
				t.Fatalf("upgrade invented delivery evidence: %+v", m)
			}
			var latest int
			if err := s.db.QueryRow(`SELECT MAX(v) FROM schema_version`).Scan(&latest); err != nil || latest != 9 {
				t.Fatalf("upgrade version=%d %v", latest, err)
			}
		})
	}
}
