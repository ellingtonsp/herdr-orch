package planmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

// testdata/example.plan.md is a synthetic day plan in the published format. It covers the
// shapes real plans use: items missing from the What table, "—" for no issue or model, odd
// held-row keys ("#205", "A / B", "A, B, misc"), "(parent …)" in What issues, and free-text
// sections (Summary, How, Steers) that must survive export byte for byte.
func fixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "example.plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "orch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var owner = store.Actor{ID: "human", Kind: store.ActorHuman, Principal: "owner", Approval: true}
var orch = store.Actor{ID: "w1:p1", Kind: store.ActorOrchestrator}

func TestParseExamplePlan(t *testing.T) {
	seed, warn, err := Parse(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(warn) > 0 {
		t.Fatalf("warnings: %v", warn)
	}
	byID := map[string]store.PlanItem{}
	var ids []string
	for _, it := range seed.Items {
		byID[it.ID] = it
		ids = append(ids, it.ID)
	}
	want := "R1 R2 U2 C1 B1 I1 I1r W1 #205 ACME-44 ACME-51 / 52 ACME-12, 17, misc epics"
	if got := strings.Join(ids, " "); got != want {
		t.Fatalf("items:\n got %s\nwant %s", got, want)
	}
	r1 := byID["R1"]
	if r1.State != "merged" || r1.PR != "#210" || r1.DispatchRef != "horch:r1/t1/d1@w2:p1" || r1.Issues[0] != "ACME-41" || !r1.Listed {
		t.Fatalf("R1 = %+v", r1)
	}
	if r1.Title != "review #210 (login fix)" || r1.What.Where != "local slot" || !strings.HasPrefix(r1.Why, "login sessions expire") {
		t.Fatalf("R1 what/why = %+v", r1)
	}
	if b1 := byID["B1"]; b1.State != "dispatched" || b1.What.Issues != "ACME-50 (parent ACME-49)" {
		t.Fatalf("B1 = %+v", b1)
	}
	if c1 := byID["C1"]; len(c1.Issues) != 0 || c1.Model != "—" {
		t.Fatalf("C1 = %+v", c1)
	}
	h := byID["#205"]
	if !h.Held || h.Listed || h.State != store.ItemHeld || h.Title != "ACME-30 legacy importer" || !strings.HasPrefix(h.HeldReason, "Superseded by ACME-49") {
		t.Fatalf("#205 = %+v", h)
	}
	if len(seed.Decisions) != 3 || seed.Decisions[2].Title != "#212" {
		t.Fatalf("decisions = %+v", seed.Decisions)
	}
}

func TestExportReproducesPublishedFile(t *testing.T) {
	src := fixture(t)
	s := openStore(t)
	seed, _, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.ImportPlan(seed, store.ImportOptions{Day: "2026-03-14", Project: "acme", Ref: "1a2b3c4"}, orch)
	if err != nil {
		t.Fatal(err)
	}
	if got := Render(w.View, nil); got != src {
		t.Fatalf("export differs from the published file:\n%s", diff(src, got))
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	s := openStore(t)
	seed, _, _ := Parse(fixture(t))
	w, err := s.ImportPlan(seed, store.ImportOptions{Day: "2026-03-14", Project: "acme"}, orch)
	if err != nil {
		t.Fatal(err)
	}
	id := w.View.Plan.ID
	// A day's worth of edits: transitions, a hold, a move, an add, a remove, a release.
	steps := []func() (store.PlanWrite, error){
		func() (store.PlanWrite, error) {
			st, pr := "settled", "#214"
			return s.UpdateItem(id, "B1", store.ItemPatch{State: &st, PR: &pr}, "transition", "PR up", 0, orch)
		},
		func() (store.PlanWrite, error) { return s.HoldItem(id, "I1", "sim | farm down", 0, owner) },
		func() (store.PlanWrite, error) { return s.MoveItem(id, "W1", 1, 0, owner) },
		func() (store.PlanWrite, error) {
			return s.AddItem(id, store.PlanItem{ID: "B2", Issues: []string{"ACME-51"}, Kind: "build", Lane: "local", Title: "slice b", Why: "next slice"}, 3, 0, owner)
		},
		func() (store.PlanWrite, error) { return s.RemoveItem(id, "U2", "folded into R2", 0, owner) },
		func() (store.PlanWrite, error) { return s.ReleaseItem(id, "#205", "", 0, owner) },
	}
	for i, f := range steps {
		if _, err := f(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	before, _ := s.GetPlanView(id)
	events, _ := s.PlanEvents(id, 0, 0)
	md := Render(before, events)
	if !strings.Contains(md, "## Event log") || !strings.Contains(md, "owner (human)") {
		t.Fatalf("export lacks the event log:\n%s", md)
	}

	seed2, warn, err := Parse(md)
	if err != nil || len(warn) > 0 {
		t.Fatalf("re-parse: %v %v", err, warn)
	}
	s2 := openStore(t)
	w2, err := s2.ImportPlan(seed2, store.ImportOptions{Day: "2026-03-14", Project: "acme"}, orch)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := comparable(before), comparable(w2.View); a != b {
		t.Fatalf("round trip changed the plan:\n%s", diff(a, b))
	}
	if again := Render(w2.View, events); again != md {
		t.Fatalf("second export differs:\n%s", diff(md, again))
	}
}

func TestExportPreservesCompleteItemsAndReleaseState(t *testing.T) {
	s := openStore(t)
	seed := store.PlanSeed{Hash: "roundtrip", Items: []store.PlanItem{
		{ID: "A", State: store.ItemDispatched, Listed: true, Model: "codex", What: store.ItemWhat{Where: "desk", Output: "line one\nline two | pipe"}},
		{ID: "H", State: store.ItemHeld, Held: true, Listed: false, Title: "held", Why: "reason\ncontinued", Kind: "review", PR: "#2"},
	}}
	w, err := s.ImportPlan(seed, store.ImportOptions{Day: "2026-10-02", Project: "p"}, orch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.HoldItem(w.View.Plan.ID, "A", "pause", 0, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveItem(w.View.Plan.ID, "H", 1, 0, owner); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetPlanView(w.View.Plan.ID)
	es, _ := s.PlanEvents(w.View.Plan.ID, 0, 0)
	parsed, warnings, err := Parse(Render(v, es))
	if err != nil || len(warnings) > 0 {
		t.Fatalf("parse: %v %v", err, warnings)
	}
	s2 := openStore(t)
	w2, err := s2.ImportPlan(parsed, store.ImportOptions{Day: "2026-10-02", Project: "p"}, orch)
	if err != nil {
		t.Fatal(err)
	}
	for i, before := range v.Items {
		after := w2.View.Items[i]
		before.Version, before.CreatedAt, before.UpdatedAt = 0, 0, 0
		after.Version, after.CreatedAt, after.UpdatedAt = 0, 0, 0
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("item lost data: %+v / %+v", before, after)
		}
	}
	released, err := s2.ReleaseItem(w2.View.Plan.ID, "A", "", 0, owner)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := released.View.Item("A")
	if a.State != store.ItemDispatched {
		t.Fatalf("release returned to %s", a.State)
	}
}

// comparable renders the plan content that must survive a round trip (not versions/times).
func comparable(v store.PlanView) string {
	var b strings.Builder
	for _, it := range v.Items {
		it.Version, it.CreatedAt, it.UpdatedAt, it.HeldFrom = 0, 0, 0, ""
		b.WriteString(strings.Join([]string{it.ID, strings.Join(it.Issues, ","), it.Kind, it.Lane, it.Model, it.State, it.PR, it.DispatchRef, it.Title, it.What.Issues, it.What.Where, it.What.Model, it.What.Output, it.Why, it.HeldReason}, "|"))
		b.WriteString(map[bool]string{true: " held", false: ""}[it.Held] + map[bool]string{true: " listed", false: ""}[it.Listed] + "\n")
	}
	for _, d := range v.Decisions {
		b.WriteString(d.Title + " — " + d.Body + "\n")
	}
	return b.String()
}

func diff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < max(len(al), len(bl)); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return "line " + itoa(i+1) + ":\n- " + x + "\n+ " + y
		}
	}
	return "(equal)"
}

func itoa(i int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + fmtInt(i)) }

func fmtInt(i int) string {
	if i == 0 {
		return "0"
	}
	var d []byte
	for ; i > 0; i /= 10 {
		d = append([]byte{byte('0' + i%10)}, d...)
	}
	return string(d)
}
