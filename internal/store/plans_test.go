package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

var (
	ownerActor = Actor{ID: "human", Kind: ActorHuman, Principal: "alice", Approval: true}
	orchActor  = Actor{ID: "w1:p1", Kind: ActorOrchestrator}
)

func seedPlan(t *testing.T, s *Store) string {
	t.Helper()
	seed := PlanSeed{Hash: "h1", Items: []PlanItem{
		{ID: "A", State: ItemPlanned, Issues: []string{"X-1"}, Listed: true},
		{ID: "B", State: ItemDispatched, Issues: []string{}, Listed: true},
		{ID: "C", State: ItemPlanned, Issues: []string{}, Listed: true},
	}}
	w, err := s.ImportPlan(seed, ImportOptions{Day: "2026-10-02", Project: "p", Ref: "abc"}, orchActor)
	if err != nil {
		t.Fatal(err)
	}
	if w.View.Plan.Status != PlanLive || w.View.Plan.Version != 1 || len(w.Events) != 1 {
		t.Fatalf("import = %+v", w)
	}
	return w.View.Plan.ID
}

func TestImportIsIdempotent(t *testing.T) {
	s := open(t)
	id := seedPlan(t, s)
	st := ItemSettled
	must[PlanWrite](t)(s.UpdateItem(id, "B", ItemPatch{State: &st}, "transition", "", 0, orchActor))
	// Same file again: nothing changes, live edits survive.
	w := must[PlanWrite](t)(s.ImportPlan(PlanSeed{Hash: "h1"}, ImportOptions{Day: "2026-10-02", Project: "p"}, orchActor))
	if len(w.Events) != 0 || len(w.View.Items) != 3 {
		t.Fatalf("re-import changed the plan: %+v", w)
	}
	if b, _ := w.View.Item("B"); b.State != ItemSettled {
		t.Fatalf("live edit lost: %+v", b)
	}
	// A different file is refused unless --replace.
	_, err := s.ImportPlan(PlanSeed{Hash: "h2"}, ImportOptions{Day: "2026-10-02", Project: "p"}, orchActor)
	refusal(t, err, "plan_exists")
	w = must[PlanWrite](t)(s.ImportPlan(PlanSeed{Hash: "h2", Items: []PlanItem{{ID: "Z", State: ItemPlanned, Listed: true}}}, ImportOptions{Day: "2026-10-02", Project: "p", Replace: true}, orchActor))
	if len(w.View.Items) != 1 || w.View.Items[0].ID != "Z" {
		t.Fatalf("replace = %+v", w.View.Items)
	}
	// A bad state in the file is refused before anything is written.
	_, err = s.ImportPlan(PlanSeed{Hash: "h3", Items: []PlanItem{{ID: "Q", State: "nope"}}}, ImportOptions{Day: "2026-10-03", Project: "p"}, orchActor)
	refusal(t, err, "bad_plan")
	_, err = s.FindPlan("p", "2026-10-03")
	refusal(t, err, "no_plan")
}

func TestVersionConflicts(t *testing.T) {
	s := open(t)
	id := seedPlan(t, s)
	a, _ := must[PlanView](t)(s.GetPlanView(id)).Item("A")
	if a.Version != 1 {
		t.Fatalf("version = %d", a.Version)
	}
	title := "first"
	w := must[PlanWrite](t)(s.UpdateItem(id, "A", ItemPatch{Title: &title}, "", "", 1, ownerActor))
	a2, _ := w.View.Item("A")
	if a2.Version != 2 || w.View.Plan.Version != 2 {
		t.Fatalf("after update item v%d plan v%d", a2.Version, w.View.Plan.Version)
	}
	// A stale writer (still at version 1) is refused and changes nothing.
	stale := "stale"
	_, err := s.UpdateItem(id, "A", ItemPatch{Title: &stale}, "", "", 1, ownerActor)
	refusal(t, err, "version_conflict")
	_, err = s.HoldItem(id, "A", "x", 1, ownerActor)
	refusal(t, err, "version_conflict")
	_, err = s.MoveItem(id, "A", 3, 1, ownerActor)
	refusal(t, err, "version_conflict")
	_, err = s.RemoveItem(id, "A", "", 1, ownerActor)
	refusal(t, err, "version_conflict")
	_, err = s.AddItem(id, PlanItem{ID: "D"}, 0, 1, ownerActor)
	refusal(t, err, "version_conflict")
	v := must[PlanView](t)(s.GetPlanView(id))
	if got, _ := v.Item("A"); got.Title != "first" || v.Plan.Version != 2 || len(v.Items) != 3 {
		t.Fatalf("stale write leaked: %+v", v)
	}
	// Current versions go through.
	must[PlanWrite](t)(s.AddItem(id, PlanItem{ID: "D"}, 1, 2, ownerActor))
	current, _ := must[PlanView](t)(s.GetPlanView(id)).Item("A")
	must[PlanWrite](t)(s.MoveItem(id, "A", 4, current.Version, ownerActor))
	v = must[PlanView](t)(s.GetPlanView(id))
	order := ""
	for _, it := range v.Items {
		order += it.ID
	}
	if order != "DBCA" {
		t.Fatalf("order = %s", order)
	}
}

func TestEventsRecordIdentity(t *testing.T) {
	s := open(t)
	id := seedPlan(t, s)
	st := ItemSettled
	must[PlanWrite](t)(s.UpdateItem(id, "B", ItemPatch{State: &st}, "transition", "done", 0, orchActor))
	must[PlanWrite](t)(s.HoldItem(id, "C", "waiting on design", 0, ownerActor))
	worker := Actor{ID: "w9:p1", Kind: ActorWorker}
	pr := "#12"
	must[PlanWrite](t)(s.UpdateItem(id, "A", ItemPatch{PR: &pr}, "", "", 0, worker))

	evs := must[[]PlanEvent](t)(s.PlanEvents(id, 0, 0))
	if len(evs) != 4 {
		t.Fatalf("events = %+v", evs)
	}
	want := []struct {
		op, actor, kind, principal string
		approval                   bool
	}{
		{"import", "w1:p1", ActorOrchestrator, "", false},
		{"transition", "w1:p1", ActorOrchestrator, "", false},
		{"item.hold", "human", ActorHuman, "alice", true},
		{"item.update", "w9:p1", ActorWorker, "", false},
	}
	for i, w := range want {
		e := evs[i]
		if e.Op != w.op || e.Actor != w.actor || e.ActorKind != w.kind || e.Principal != w.principal || e.Approval != w.approval || e.Seq <= 0 || e.PlanVersion != int64(i+1) {
			t.Fatalf("event %d = %+v, want %+v", i, e, w)
		}
	}
	var before, after PlanItem
	_ = json.Unmarshal(evs[2].Before, &before)
	_ = json.Unmarshal(evs[2].After, &after)
	if before.State != ItemPlanned || after.State != ItemHeld || after.HeldReason != "waiting on design" {
		t.Fatalf("hold before/after = %+v / %+v", before, after)
	}
	if tail := must[[]PlanEvent](t)(s.PlanEvents("", evs[2].Seq, 0)); len(tail) != 1 || tail[0].Op != "item.update" {
		t.Fatalf("since = %+v", tail)
	}
	// The log is append-only.
	if _, err := s.db.Exec(`UPDATE plan_events SET actor='x'`); err == nil {
		t.Fatal("plan_events accepted an update")
	}
	if _, err := s.db.Exec(`DELETE FROM plan_events`); err == nil {
		t.Fatal("plan_events accepted a delete")
	}
}

func TestHoldReleaseAndFinal(t *testing.T) {
	s := open(t)
	id := seedPlan(t, s)
	must[PlanWrite](t)(s.HoldItem(id, "B", "sim down", 0, ownerActor))
	st := ItemSettled
	_, err := s.UpdateItem(id, "B", ItemPatch{State: &st}, "transition", "", 0, orchActor)
	refusal(t, err, "item_held")
	held := ItemHeld
	_, err = s.UpdateItem(id, "A", ItemPatch{State: &held}, "transition", "", 0, orchActor)
	refusal(t, err, "bad_state")
	w := must[PlanWrite](t)(s.ReleaseItem(id, "B", "", 0, ownerActor))
	if b, _ := w.View.Item("B"); b.State != ItemDispatched || b.Held {
		t.Fatalf("release = %+v", b)
	}
	bad := "exploded"
	_, err = s.UpdateItem(id, "A", ItemPatch{State: &bad}, "transition", "", 0, orchActor)
	refusal(t, err, "bad_state")

	must[PlanWrite](t)(s.SetPlanStatus(id, PlanFinal, 0, ownerActor))
	_, err = s.HoldItem(id, "A", "late", 0, ownerActor)
	refusal(t, err, "plan_final")
}

func TestAddCannotBypassHold(t *testing.T) {
	s := open(t)
	id := seedPlan(t, s)
	_, err := s.AddItem(id, PlanItem{ID: "D", State: ItemHeld}, 0, 0, orchActor)
	refusal(t, err, "bad_state")
	v, events, err := s.GetPlanSnapshot(id)
	if err != nil || len(v.Items) != 3 || len(events) != 1 || v.Plan.Version != 1 {
		t.Fatalf("refused add changed the plan: %+v, %+v, %v", v, events, err)
	}
}

func TestOrderingChangesInvalidateDisplacedItemVersion(t *testing.T) {
	for _, op := range []string{"add", "move", "remove"} {
		t.Run(op, func(t *testing.T) {
			s := open(t)
			id := seedPlan(t, s)
			var w PlanWrite
			var err error
			wantPosition := 1
			switch op {
			case "add":
				w, err = s.AddItem(id, PlanItem{ID: "D"}, 1, 0, ownerActor)
				wantPosition = 3
			case "move":
				w, err = s.MoveItem(id, "A", 3, 1, ownerActor)
			case "remove":
				w, err = s.RemoveItem(id, "A", "", 1, ownerActor)
			}
			if err != nil {
				t.Fatal(err)
			}
			b, _ := w.View.Item("B")
			if b.Position != wantPosition || b.Version != 2 {
				t.Fatalf("displaced item must advance version: %+v", b)
			}
			if op == "move" {
				a, _ := w.View.Item("A")
				if a.Version != 2 {
					t.Fatalf("moved item advanced twice: %+v", a)
				}
			}
			if op == "add" {
				d, _ := w.View.Item("D")
				if d.Version != 1 {
					t.Fatalf("new item advanced before its first edit: %+v", d)
				}
			}
			_, err = s.MoveItem(id, "B", 3, 1, ownerActor)
			refusal(t, err, "version_conflict")
			_, err = s.MoveItem(id, "B", 3, 2, ownerActor)
			if err != nil {
				t.Fatalf("current item version refused: %v", err)
			}
		})
	}
}

func TestEventsRejectReplace(t *testing.T) {
	s := open(t)
	id := seedPlan(t, s)
	events := must[[]PlanEvent](t)(s.PlanEvents(id, 0, 0))
	e := events[0]
	_, err := s.db.Exec(`INSERT OR REPLACE INTO plan_events(seq,plan_id,ts,actor,actor_kind,principal,approval,op,item,note,before,after,plan_version)
		SELECT seq,plan_id,ts,'spoofed',actor_kind,principal,approval,op,item,note,before,after,plan_version FROM plan_events WHERE seq=?`, e.Seq)
	if err == nil {
		t.Fatal("append-only event overwritten with INSERT OR REPLACE")
	}
	after := must[[]PlanEvent](t)(s.PlanEvents(id, 0, 0))
	if len(after) != 1 || after[0].Actor != e.Actor || after[0].Seq != e.Seq {
		t.Fatalf("refused replacement changed the log: %+v", after)
	}
}

func TestConcurrentPlanSnapshots(t *testing.T) {
	s := open(t)
	id := seedPlan(t, s)
	writerErr := make(chan error, 1)
	go func() {
		for version := int64(2); version <= 257; version++ {
			title := strconv.FormatInt(version, 10)
			if _, err := s.UpdateItem(id, "A", ItemPatch{Title: &title}, "", "", 0, orchActor); err != nil {
				writerErr <- err
				return
			}
		}
		writerErr <- nil
	}()
	defer func() {
		if err := <-writerErr; err != nil {
			t.Error(err)
		}
	}()
	for range 256 {
		v := must[PlanView](t)(s.GetPlanView(id))
		a, _ := v.Item("A")
		if a.Title != "" && a.Title != strconv.FormatInt(v.Plan.Version, 10) {
			t.Fatalf("inconsistent view: plan v%d, item title=%s", v.Plan.Version, a.Title)
		}
		view, events, err := s.GetPlanSnapshot(id)
		if err != nil {
			t.Fatal(err)
		}
		a, _ = view.Item("A")
		if a.Title != "" && a.Title != strconv.FormatInt(view.Plan.Version, 10) {
			t.Fatalf("inconsistent export view: plan v%d, item title=%s", view.Plan.Version, a.Title)
		}
		if len(events) != int(view.Plan.Version) || events[len(events)-1].PlanVersion != view.Plan.Version {
			t.Fatalf("inconsistent export events: plan v%d, events=%+v", view.Plan.Version, events)
		}
	}
}

func TestConcurrentConditionalWriters(t *testing.T) {
	s := open(t)
	id := seedPlan(t, s)
	start := make(chan struct{})
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			title := strconv.Itoa(i)
			_, err := s.UpdateItem(id, "A", ItemPatch{Title: &title}, "", "", 1, orchActor)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		} else {
			refusal(t, err, "version_conflict")
		}
	}
	v, events, err := s.GetPlanSnapshot(id)
	if successes != 1 || err != nil || v.Plan.Version != 2 || len(events) != 2 {
		t.Fatalf("conditional writers: successes=%d plan=%+v events=%+v err=%v", successes, v.Plan, events, err)
	}
}

func TestBusyPlanWriteRollsBackAndRetries(t *testing.T) {
	s := open(t)
	id := seedPlan(t, s)
	var path string
	if err := s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	other := must[*sql.DB](t)(sql.Open("sqlite", "file:"+path))
	defer other.Close()
	other.SetMaxOpenConns(1)
	if _, err := other.Exec(`BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer other.Exec(`ROLLBACK`)
	if _, err := s.db.Exec(`PRAGMA busy_timeout=1`); err != nil {
		t.Fatal(err)
	}
	title := "blocked write"
	_, err := s.UpdateItem(id, "A", ItemPatch{Title: &title}, "", "", 1, orchActor)
	if err == nil || !strings.Contains(err.Error(), "SQLITE_BUSY") {
		t.Fatalf("want SQLITE_BUSY, got %v", err)
	}
	if _, err := other.Exec(`ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	v, events, err := s.GetPlanSnapshot(id)
	a, _ := v.Item("A")
	if err != nil || a.Title != "" || a.Version != 1 || v.Plan.Version != 1 || len(events) != 1 {
		t.Fatalf("busy write leaked changes: %+v events=%+v err=%v", v, events, err)
	}
	must[PlanWrite](t)(s.UpdateItem(id, "A", ItemPatch{Title: &title}, "", "", 1, orchActor))
}

func TestPlanMigrationPreservesExistingLiveDatabase(t *testing.T) {
	for _, version := range []int{2, 3} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "live.db")
			db := must[*sql.DB](t)(sql.Open("sqlite", "file:"+path))
			if _, err := db.Exec(`CREATE TABLE schema_version(v INTEGER NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < version; i++ {
				if _, err := db.Exec(migrations[i]); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO schema_version(v) VALUES(?)`, i+1); err != nil {
					t.Fatal(err)
				}
			}
			legacy := []string{
				`INSERT INTO runs(id,title,coordinator_pane_id,status,created_at,updated_at,idle_report_ms,idle_flag_ms) VALUES('r1','live run','w1:p1','running',1,2,123,456)`,
				`INSERT INTO tasks(id,run_id,title,status,created_at,updated_at) VALUES('t1','r1','live task','dispatched',1,2)`,
				`INSERT INTO dispatches(id,task_id,run_id,pane_id,status,started_at,idle_escalated,nudges) VALUES('d1','t1','r1','w2:p1','dispatched',1,1,2)`,
				`INSERT INTO messages(id,from_pane,to_pane,body,kind,created_at) VALUES('m1','w1:p1','w2:p1','keep me','note',1)`,
			}
			for _, q := range legacy {
				if _, err := db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if version == 3 {
				if _, err := db.Exec(`INSERT INTO plan_events(plan_id,ts,actor,actor_kind,op,plan_version) VALUES('p/2026-10-02',1,'original','human','import',1)`); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()
			for range 2 {
				s := must[*Store](t)(Open(path))
				r := must[Run](t)(s.GetRun("r1"))
				if r.Status != RunRunning || r.IdleReportMS != 123 || r.IdleFlagMS != 456 {
					t.Fatalf("run changed: %+v", r)
				}
				task := must[Task](t)(s.GetTask("t1"))
				if task.Status != TaskDispatched || task.Title != "live task" {
					t.Fatalf("task changed: %+v", task)
				}
				dispatch := must[Dispatch](t)(s.GetDispatch("d1"))
				if dispatch.Status != DispatchDispatched || !dispatch.IdleEscalated || dispatch.Nudges != 2 {
					t.Fatalf("dispatch changed: %+v", dispatch)
				}
				var body string
				if err := s.db.QueryRow(`SELECT body FROM messages WHERE id='m1'`).Scan(&body); err != nil || body != "keep me" {
					t.Fatalf("message changed: %q %v", body, err)
				}
				var count, latest int
				if err := s.db.QueryRow(`SELECT COUNT(*),MAX(v) FROM schema_version`).Scan(&count, &latest); err != nil || count != 4 || latest != 4 {
					t.Fatalf("migration versions: count=%d latest=%d err=%v", count, latest, err)
				}
				if version == 3 {
					events := must[[]PlanEvent](t)(s.PlanEvents("", 0, 0))
					if len(events) != 1 || events[0].Actor != "original" || events[0].Seq != 1 {
						t.Fatalf("existing event changed: %+v", events)
					}
					if _, err := s.db.Exec(`INSERT OR REPLACE INTO plan_events(seq,plan_id,ts,actor,actor_kind,op,plan_version) VALUES(1,'p/2026-10-02',2,'replacement','human','import',1)`); err == nil {
						t.Fatal("upgrade did not protect existing event from replacement")
					}
				}
				s.Close()
			}
		})
	}
}
