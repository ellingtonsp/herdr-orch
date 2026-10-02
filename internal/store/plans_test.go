package store

import (
	"encoding/json"
	"testing"
)

var (
	ownerActor = Actor{ID: "human", Kind: ActorHuman, Principal: "stephen", Approval: true}
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
	must[PlanWrite](t)(s.MoveItem(id, "A", 4, 2, ownerActor))
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
		{"item.hold", "human", ActorHuman, "stephen", true},
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
