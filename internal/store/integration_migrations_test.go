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

func TestUpgradeReleasedMainPreservesHoldsAndLifecycle(t *testing.T) {
	for _, version := range []int{5, 6} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "orch.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TABLE schema_version(v INTEGER NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			for i, m := range append(append([]string{}, migrations[:4]...), releasedHolds, releasedLifecycle) {
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
			if version == 6 {
				fixtures = append(fixtures, `UPDATE workers SET agent_args='["--model","example-model"]',lifecycle='restart_failed',hand_off_to='w1:recipient',hand_off_at=42,hand_off_worktree='/tmp/example' WHERE pane_id='w1:worker'`)
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
			if version == 6 && (!w.LaunchKnown || len(w.AgentArgs) != 2 || w.Lifecycle != "restart_failed" || w.HandOffTo != "w1:recipient" || w.HandOffAt != 42 || w.HandOffWorktree != "/tmp/example") {
				t.Fatalf("lifecycle data lost: %+v", w)
			}
			var latest int
			if err := s.db.QueryRow(`SELECT MAX(v) FROM schema_version`).Scan(&latest); err != nil || latest != 7 {
				t.Fatalf("upgrade version=%d %v", latest, err)
			}
		})
	}
}
