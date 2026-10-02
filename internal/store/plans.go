package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Day-plan store: the live layer between the published <date>.plan.md and the EOD export.
// Every write runs in one transaction that checks versions, bumps the plan version and
// appends one plan_events row recording who made the change.

// Plan statuses.
const (
	PlanDraft = "draft"
	PlanLive  = "live"
	PlanFinal = "final"
)

// Item states.
const (
	ItemPlanned    = "planned"
	ItemDispatched = "dispatched"
	ItemSettled    = "settled"
	ItemBounced    = "bounced"
	ItemRatified   = "ratified"
	ItemMerged     = "merged"
	ItemHeld       = "held"
	ItemReplanned  = "replanned"
	ItemDropped    = "dropped"
)

var ItemStates = []string{ItemPlanned, ItemDispatched, ItemSettled, ItemBounced, ItemRatified, ItemMerged, ItemHeld, ItemReplanned, ItemDropped}

// Actor kinds.
const (
	ActorHuman        = "human"
	ActorOrchestrator = "orchestrator"
	ActorWorker       = "worker"
)

// Actor is who made a plan write. Approval is true only for the configured owner
// principal; the daemon decides it, never the caller.
type Actor struct {
	ID        string `json:"actor"`
	Kind      string `json:"actor_kind"`
	Principal string `json:"principal,omitempty"`
	Approval  bool   `json:"approval"`
}

// PlanSection is one "## " section of the published file. Kind "text" keeps its body
// verbatim; the structured kinds (what, why, decisions, held, items) are rendered from
// the store on export.
type PlanSection struct {
	Heading string `json:"heading"`
	Kind    string `json:"kind"`
	Body    string `json:"body,omitempty"`
}

type Plan struct {
	ID           string        `json:"id"`
	Day          string        `json:"day"`
	Project      string        `json:"project"`
	PublishedRef string        `json:"published_ref,omitempty"`
	SourceHash   string        `json:"source_hash,omitempty"`
	Status       string        `json:"status"`
	Coordinator  string        `json:"coordinator,omitempty"`
	Sections     []PlanSection `json:"sections"`
	Version      int64         `json:"version"`
	CreatedAt    int64         `json:"created_at"`
	UpdatedAt    int64         `json:"updated_at"`
}

// ItemWhat is the item's row in the plan's What table.
type ItemWhat struct {
	Issues string `json:"issues,omitempty"`
	Where  string `json:"where,omitempty"`
	Model  string `json:"model,omitempty"`
	Output string `json:"output,omitempty"`
}

type PlanItem struct {
	ID          string   `json:"id"`
	Position    int      `json:"position"`
	Issues      []string `json:"issues"`
	Kind        string   `json:"kind"`
	Lane        string   `json:"lane"`
	Model       string   `json:"model"`
	State       string   `json:"state"`
	PR          string   `json:"pr"`
	DispatchRef string   `json:"dispatch_ref"`
	Title       string   `json:"title"`
	What        ItemWhat `json:"what"`
	Why         string   `json:"why"`
	Held        bool     `json:"held"`
	HeldReason  string   `json:"held_reason,omitempty"`
	HeldFrom    string   `json:"held_from,omitempty"`
	// Listed items appear in the Items table; unlisted ones only in "Held on purpose".
	Listed    bool  `json:"listed"`
	Version   int64 `json:"version"`
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

type PlanDecision struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type PlanEvent struct {
	Seq         int64           `json:"seq"`
	PlanID      string          `json:"plan_id"`
	TS          int64           `json:"ts"`
	Actor       string          `json:"actor"`
	ActorKind   string          `json:"actor_kind"`
	Principal   string          `json:"principal,omitempty"`
	Approval    bool            `json:"approval"`
	Op          string          `json:"op"`
	Item        string          `json:"item,omitempty"`
	Note        string          `json:"note,omitempty"`
	Before      json.RawMessage `json:"before,omitempty"`
	After       json.RawMessage `json:"after,omitempty"`
	PlanVersion int64           `json:"plan_version"`
}

// PlanView is a plan with its items (in position order) and decisions.
type PlanView struct {
	Plan      Plan           `json:"plan"`
	Items     []PlanItem     `json:"items"`
	Decisions []PlanDecision `json:"decisions"`
}

func (v PlanView) Item(id string) (PlanItem, bool) {
	for _, it := range v.Items {
		if it.ID == id {
			return it, true
		}
	}
	return PlanItem{}, false
}

// PlanSeed is a parsed published plan, ready to import.
type PlanSeed struct {
	Sections  []PlanSection
	Items     []PlanItem
	Decisions []PlanDecision
	Hash      string
}

func PlanID(project, day string) string { return project + "/" + day }

// ---------- reads ----------

const planCols = `id,day,project,published_ref,source_hash,status,coordinator,sections,version,created_at,updated_at`

func scanPlan(r interface{ Scan(...any) error }) (Plan, error) {
	var p Plan
	var secs string
	err := r.Scan(&p.ID, &p.Day, &p.Project, &p.PublishedRef, &p.SourceHash, &p.Status, &p.Coordinator, &secs, &p.Version, &p.CreatedAt, &p.UpdatedAt)
	_ = json.Unmarshal([]byte(secs), &p.Sections)
	if p.Sections == nil {
		p.Sections = []PlanSection{}
	}
	return p, err
}

const itemCols = `id,position,issues,kind,lane,model,state,pr,dispatch_ref,title,what,why,held,held_reason,held_from,listed,version,created_at,updated_at`

func scanItem(r interface{ Scan(...any) error }) (PlanItem, error) {
	var it PlanItem
	var issues, what string
	err := r.Scan(&it.ID, &it.Position, &issues, &it.Kind, &it.Lane, &it.Model, &it.State, &it.PR, &it.DispatchRef, &it.Title, &what, &it.Why, &it.Held, &it.HeldReason, &it.HeldFrom, &it.Listed, &it.Version, &it.CreatedAt, &it.UpdatedAt)
	it.Issues = parseList(issues)
	_ = json.Unmarshal([]byte(what), &it.What)
	return it, err
}

func getPlan(q querier, id string) (Plan, error) {
	p, err := scanPlan(q.QueryRow(`SELECT `+planCols+` FROM plans WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return p, refuse("no_plan", "no plan %s (import one with `horch plan import`)", id)
	}
	return p, err
}

// FindPlan resolves a plan by project and day; day "" is the project's latest plan.
func (s *Store) FindPlan(project, day string) (Plan, error) {
	if day != "" {
		return getPlan(s.db, PlanID(project, day))
	}
	p, err := scanPlan(s.db.QueryRow(`SELECT `+planCols+` FROM plans WHERE project=? ORDER BY day DESC LIMIT 1`, project))
	if err == sql.ErrNoRows {
		return p, refuse("no_plan", "no plan for project %s (import one with `horch plan import`)", project)
	}
	return p, err
}

func (s *Store) GetPlanView(id string) (PlanView, error) { return planView(s.db, id) }

func planView(q querier, id string) (PlanView, error) {
	var v PlanView
	var err error
	if v.Plan, err = getPlan(q, id); err != nil {
		return v, err
	}
	if v.Items, err = listItems(q, id); err != nil {
		return v, err
	}
	rows, err := q.Query(`SELECT title,body FROM plan_decisions WHERE plan_id=? ORDER BY position`, id)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	v.Decisions = []PlanDecision{}
	for rows.Next() {
		var d PlanDecision
		if err := rows.Scan(&d.Title, &d.Body); err != nil {
			return v, err
		}
		v.Decisions = append(v.Decisions, d)
	}
	return v, rows.Err()
}

func listItems(q querier, plan string) ([]PlanItem, error) {
	rows, err := q.Query(`SELECT `+itemCols+` FROM plan_items WHERE plan_id=? ORDER BY position`, plan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlanItem{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func getItem(q querier, plan, id string) (PlanItem, error) {
	it, err := scanItem(q.QueryRow(`SELECT `+itemCols+` FROM plan_items WHERE plan_id=? AND id=?`, plan, id))
	if err == sql.ErrNoRows {
		return it, refuse("unknown_item", "plan %s has no item %q", plan, id)
	}
	return it, err
}

// PlanEvents returns events with seq > since, oldest first; plan "" means every plan.
func (s *Store) PlanEvents(plan string, since int64, limit int) ([]PlanEvent, error) {
	if limit <= 0 {
		limit = 1000
	}
	q := `SELECT seq,plan_id,ts,actor,actor_kind,principal,approval,op,item,note,COALESCE(before,''),COALESCE(after,''),plan_version FROM plan_events WHERE seq>?`
	args := []any{since}
	if plan != "" {
		q += ` AND plan_id=?`
		args = append(args, plan)
	}
	rows, err := s.db.Query(q+` ORDER BY seq LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlanEvent{}
	for rows.Next() {
		var e PlanEvent
		var before, after string
		if err := rows.Scan(&e.Seq, &e.PlanID, &e.TS, &e.Actor, &e.ActorKind, &e.Principal, &e.Approval, &e.Op, &e.Item, &e.Note, &before, &after, &e.PlanVersion); err != nil {
			return nil, err
		}
		if before != "" {
			e.Before = json.RawMessage(before)
		}
		if after != "" {
			e.After = json.RawMessage(after)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- writes ----------

// PlanWrite is the result of a plan write: the plan after it and the event it appended
// (none when the write was a no-op, e.g. re-importing the same file).
type PlanWrite struct {
	View   PlanView    `json:"view"`
	Events []PlanEvent `json:"events"`
}

type planTx struct {
	tx     *sql.Tx
	s      *Store
	plan   Plan
	actor  Actor
	events []PlanEvent
}

// writePlan runs fn against a live (or draft) plan after checking the plan version.
func (s *Store) writePlan(id string, ifPlanVersion int64, a Actor, fn func(*planTx) error) (PlanWrite, error) {
	var out PlanWrite
	err := s.tx(func(tx *sql.Tx) error {
		var err error
		out, err = s.writePlanIn(tx, id, ifPlanVersion, a, fn)
		return err
	})
	return out, err
}

func (s *Store) writePlanIn(tx *sql.Tx, id string, ifPlanVersion int64, a Actor, fn func(*planTx) error) (PlanWrite, error) {
	var out PlanWrite
	err := func() error {
		p, err := getPlan(tx, id)
		if err != nil {
			return err
		}
		if p.Status == PlanFinal {
			return refuse("plan_final", "plan %s is final; it can no longer change", id)
		}
		if ifPlanVersion != 0 && p.Version != ifPlanVersion {
			return refuse("version_conflict", "plan %s is at version %d, not %d: reload and retry", id, p.Version, ifPlanVersion)
		}
		pt := &planTx{tx: tx, s: s, plan: p, actor: a}
		if err := fn(pt); err != nil {
			return err
		}
		if out.View, err = planView(tx, id); err != nil {
			return err
		}
		out.Events = pt.events
		return nil
	}()
	return out, err
}

// event bumps the plan version and appends one event.
func (pt *planTx) event(op, item, note string, before, after any) error {
	now := pt.s.now()
	pt.plan.Version++
	if _, err := pt.tx.Exec(`UPDATE plans SET version=?, updated_at=? WHERE id=?`, pt.plan.Version, now, pt.plan.ID); err != nil {
		return err
	}
	e := PlanEvent{PlanID: pt.plan.ID, TS: now, Actor: pt.actor.ID, ActorKind: pt.actor.Kind, Principal: pt.actor.Principal, Approval: pt.actor.Approval, Op: op, Item: item, Note: note, PlanVersion: pt.plan.Version}
	if before != nil {
		e.Before, _ = json.Marshal(before)
	}
	if after != nil {
		e.After, _ = json.Marshal(after)
	}
	res, err := pt.tx.Exec(`INSERT INTO plan_events(plan_id,ts,actor,actor_kind,principal,approval,op,item,note,before,after,plan_version) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.PlanID, e.TS, e.Actor, e.ActorKind, e.Principal, e.Approval, e.Op, e.Item, e.Note, nullRaw(e.Before), nullRaw(e.After), e.PlanVersion)
	if err != nil {
		return err
	}
	e.Seq, _ = res.LastInsertId()
	pt.events = append(pt.events, e)
	return nil
}

func nullRaw(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func insertItem(q querier, plan string, it PlanItem, now int64) error {
	what, _ := json.Marshal(it.What)
	_, err := q.Exec(`INSERT INTO plan_items(plan_id,id,position,issues,kind,lane,model,state,pr,dispatch_ref,title,what,why,held,held_reason,held_from,listed,version,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?)`,
		plan, it.ID, it.Position, jsonList(it.Issues), it.Kind, it.Lane, it.Model, it.State, it.PR, it.DispatchRef, it.Title, string(what), it.Why, it.Held, it.HeldReason, it.HeldFrom, it.Listed, now, now)
	return err
}

// saveItem writes it back with version+1.
func (pt *planTx) saveItem(it *PlanItem) error {
	it.Version++
	it.UpdatedAt = pt.s.now()
	what, _ := json.Marshal(it.What)
	_, err := pt.tx.Exec(`UPDATE plan_items SET position=?,issues=?,kind=?,lane=?,model=?,state=?,pr=?,dispatch_ref=?,title=?,what=?,why=?,held=?,held_reason=?,held_from=?,listed=?,version=?,updated_at=? WHERE plan_id=? AND id=?`,
		it.Position, jsonList(it.Issues), it.Kind, it.Lane, it.Model, it.State, it.PR, it.DispatchRef, it.Title, string(what), it.Why, it.Held, it.HeldReason, it.HeldFrom, it.Listed, it.Version, it.UpdatedAt, pt.plan.ID, it.ID)
	return err
}

// item loads an item and checks its version.
func (pt *planTx) item(id string, ifVersion int64) (PlanItem, error) {
	it, err := getItem(pt.tx, pt.plan.ID, id)
	if err != nil {
		return it, err
	}
	if ifVersion != 0 && it.Version != ifVersion {
		return it, refuse("version_conflict", "item %s is at version %d, not %d: reload and retry", id, it.Version, ifVersion)
	}
	return it, nil
}

// renumber stores positions 1..n in the given order.
func (pt *planTx) renumber(ids []string) error {
	for i, id := range ids {
		if _, err := pt.tx.Exec(`UPDATE plan_items SET position=? WHERE plan_id=? AND id=?`, i+1, pt.plan.ID, id); err != nil {
			return err
		}
	}
	return nil
}

func (pt *planTx) order() ([]string, error) {
	items, err := listItems(pt.tx, pt.plan.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids, nil
}

func validState(s string) error {
	if !slices.Contains(ItemStates, s) {
		return refuse("bad_state", "unknown item state %q (want one of %s)", s, strings.Join(ItemStates, ", "))
	}
	return nil
}

// ImportOptions control ImportPlan.
type ImportOptions struct {
	Day, Project, Ref, Status, Coordinator string
	// Replace reseeds a plan already imported from a different file, discarding live edits.
	Replace bool
}

// ImportPlan seeds the plan for day from a published file. Re-importing the same file is
// a no-op (live edits survive); a different file is refused unless Replace.
func (s *Store) ImportPlan(seed PlanSeed, o ImportOptions, a Actor) (PlanWrite, error) {
	if o.Day == "" || o.Project == "" {
		return PlanWrite{}, refuse("bad_args", "import needs a day and a project")
	}
	if o.Status == "" {
		o.Status = PlanLive
	}
	if o.Status != PlanDraft && o.Status != PlanLive {
		return PlanWrite{}, refuse("bad_value", "import status must be draft or live")
	}
	seen := map[string]bool{}
	for _, it := range seed.Items {
		if it.ID == "" || seen[it.ID] {
			return PlanWrite{}, refuse("bad_plan", "plan file has an empty or duplicate item id %q", it.ID)
		}
		seen[it.ID] = true
		if err := validState(it.State); err != nil {
			return PlanWrite{}, refuse("bad_plan", "item %s: %s", it.ID, err.(*Refusal).Message)
		}
	}
	id := PlanID(o.Project, o.Day)
	existing, err := getPlan(s.db, id)
	exists := err == nil
	if err != nil && !isRefusal(err, "no_plan") {
		return PlanWrite{}, err
	}
	if exists {
		if existing.SourceHash == seed.Hash {
			v, err := s.GetPlanView(id)
			return PlanWrite{View: v, Events: []PlanEvent{}}, err
		}
		if !o.Replace {
			return PlanWrite{}, refuse("plan_exists", "plan %s was imported from a different file (ref %s); pass --replace to reseed it (live edits since then are discarded, the event log is kept)", id, or(existing.PublishedRef, "?"))
		}
		if existing.Status == PlanFinal {
			return PlanWrite{}, refuse("plan_final", "plan %s is final; it can no longer change", id)
		}
	}
	now := s.now()
	secs, _ := json.Marshal(seed.Sections)
	var out PlanWrite
	err = s.tx(func(tx *sql.Tx) error {
		if !exists {
			if _, err := tx.Exec(`INSERT INTO plans(id,day,project,status,version,created_at,updated_at) VALUES(?,?,?,?,0,?,?)`, id, o.Day, o.Project, o.Status, now, now); err != nil {
				return err
			}
		}
		var err error
		out, err = s.writePlanIn(tx, id, 0, a, func(pt *planTx) error {
			before := map[string]any{"items": 0}
			if exists {
				before = map[string]any{"ref": existing.PublishedRef, "hash": existing.SourceHash}
			}
			for _, q := range []string{`DELETE FROM plan_items WHERE plan_id=?`, `DELETE FROM plan_decisions WHERE plan_id=?`} {
				if _, err := pt.tx.Exec(q, id); err != nil {
					return err
				}
			}
			coord := o.Coordinator
			if coord == "" {
				coord = existing.Coordinator
			}
			if _, err := pt.tx.Exec(`UPDATE plans SET published_ref=?, source_hash=?, status=?, coordinator=?, sections=? WHERE id=?`, o.Ref, seed.Hash, o.Status, coord, string(secs), id); err != nil {
				return err
			}
			for i, it := range seed.Items {
				it.Position = i + 1
				if err := insertItem(pt.tx, id, it, now); err != nil {
					return err
				}
			}
			for i, d := range seed.Decisions {
				if _, err := pt.tx.Exec(`INSERT INTO plan_decisions(plan_id,position,title,body) VALUES(?,?,?,?)`, id, i+1, d.Title, d.Body); err != nil {
					return err
				}
			}
			return pt.event("import", "", o.Ref, before, map[string]any{"ref": o.Ref, "hash": seed.Hash, "items": len(seed.Items), "decisions": len(seed.Decisions), "status": o.Status})
		})
		return err
	})
	return out, err
}

func isRefusal(err error, code string) bool {
	r, ok := err.(*Refusal)
	return ok && r.Code == code
}

func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// AddItem inserts it at position (1-based; 0 or past the end appends).
func (s *Store) AddItem(plan string, it PlanItem, position int, ifPlanVersion int64, a Actor) (PlanWrite, error) {
	if it.ID == "" {
		return PlanWrite{}, refuse("bad_args", "item needs an --id")
	}
	if it.State == "" {
		it.State = ItemPlanned
	}
	if err := validState(it.State); err != nil {
		return PlanWrite{}, err
	}
	it.Listed = true
	return s.writePlan(plan, ifPlanVersion, a, func(pt *planTx) error {
		if _, err := getItem(pt.tx, plan, it.ID); err == nil {
			return refuse("item_exists", "plan %s already has item %s", plan, it.ID)
		}
		ids, err := pt.order()
		if err != nil {
			return err
		}
		if position <= 0 || position > len(ids)+1 {
			position = len(ids) + 1
		}
		it.Position = position
		if err := insertItem(pt.tx, plan, it, pt.s.now()); err != nil {
			return err
		}
		ids = slices.Insert(ids, position-1, it.ID)
		if err := pt.renumber(ids); err != nil {
			return err
		}
		after, _ := getItem(pt.tx, plan, it.ID)
		return pt.event("item.add", it.ID, "", nil, after)
	})
}

// ItemPatch changes the fields that are set.
type ItemPatch struct {
	Issues      *[]string `json:"issues,omitempty"`
	Kind        *string   `json:"kind,omitempty"`
	Lane        *string   `json:"lane,omitempty"`
	Model       *string   `json:"model,omitempty"`
	State       *string   `json:"state,omitempty"`
	PR          *string   `json:"pr,omitempty"`
	DispatchRef *string   `json:"dispatch_ref,omitempty"`
	Title       *string   `json:"title,omitempty"`
	Where       *string   `json:"where,omitempty"`
	Output      *string   `json:"output,omitempty"`
	Why         *string   `json:"why,omitempty"`
}

// Apply sets the patched fields on it.
func (p ItemPatch) Apply(it *PlanItem) {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	if p.Issues != nil {
		it.Issues = *p.Issues
	}
	set(&it.Kind, p.Kind)
	set(&it.Lane, p.Lane)
	set(&it.Model, p.Model)
	set(&it.State, p.State)
	set(&it.PR, p.PR)
	set(&it.DispatchRef, p.DispatchRef)
	set(&it.Title, p.Title)
	set(&it.What.Where, p.Where)
	set(&it.What.Output, p.Output)
	set(&it.Why, p.Why)
}

// UpdateItem applies patch to an item (op names the event: item.update or transition).
func (s *Store) UpdateItem(plan, id string, patch ItemPatch, op, note string, ifVersion int64, a Actor) (PlanWrite, error) {
	if patch.State != nil {
		if err := validState(*patch.State); err != nil {
			return PlanWrite{}, err
		}
	}
	if op == "" {
		op = "item.update"
	}
	return s.writePlan(plan, 0, a, func(pt *planTx) error {
		before, err := pt.item(id, ifVersion)
		if err != nil {
			return err
		}
		after := before
		patch.Apply(&after)
		if patch.State != nil && *patch.State != before.State {
			switch {
			case *patch.State == ItemHeld:
				return refuse("bad_state", "use `horch plan item hold` to hold an item")
			case before.Held && *patch.State != ItemDropped && *patch.State != ItemReplanned:
				return refuse("item_held", "item %s is held (%s); release it first", id, or(before.HeldReason, "no reason"))
			}
		}
		if err := pt.saveItem(&after); err != nil {
			return err
		}
		return pt.event(op, id, note, before, after)
	})
}

// MoveItem moves an item to position (1-based; clamped).
func (s *Store) MoveItem(plan, id string, position int, ifVersion int64, a Actor) (PlanWrite, error) {
	return s.writePlan(plan, 0, a, func(pt *planTx) error {
		before, err := pt.item(id, ifVersion)
		if err != nil {
			return err
		}
		ids, err := pt.order()
		if err != nil {
			return err
		}
		ids = slices.DeleteFunc(ids, func(x string) bool { return x == id })
		position = max(1, min(position, len(ids)+1))
		ids = slices.Insert(ids, position-1, id)
		if err := pt.renumber(ids); err != nil {
			return err
		}
		after := before
		after.Position = position
		if err := pt.saveItem(&after); err != nil {
			return err
		}
		return pt.event("item.move", id, fmt.Sprintf("%d → %d", before.Position, position), before, after)
	})
}

// HoldItem parks an item: state held, remembering the state to release back to.
func (s *Store) HoldItem(plan, id, reason string, ifVersion int64, a Actor) (PlanWrite, error) {
	return s.writePlan(plan, 0, a, func(pt *planTx) error {
		before, err := pt.item(id, ifVersion)
		if err != nil {
			return err
		}
		if before.Held {
			return refuse("already_held", "item %s is already held", id)
		}
		after := before
		after.Held, after.HeldReason, after.HeldFrom, after.State = true, reason, before.State, ItemHeld
		if err := pt.saveItem(&after); err != nil {
			return err
		}
		return pt.event("item.hold", id, reason, before, after)
	})
}

// ReleaseItem un-holds an item, back to the state it was held from (planned if none).
func (s *Store) ReleaseItem(plan, id, note string, ifVersion int64, a Actor) (PlanWrite, error) {
	return s.writePlan(plan, 0, a, func(pt *planTx) error {
		before, err := pt.item(id, ifVersion)
		if err != nil {
			return err
		}
		if !before.Held {
			return refuse("not_held", "item %s is not held", id)
		}
		after := before
		after.Held, after.HeldReason, after.HeldFrom, after.Listed = false, "", "", true
		if after.State == ItemHeld {
			after.State = or(before.HeldFrom, ItemPlanned)
			if after.State == ItemHeld {
				after.State = ItemPlanned
			}
		}
		if err := pt.saveItem(&after); err != nil {
			return err
		}
		return pt.event("item.release", id, note, before, after)
	})
}

// RemoveItem deletes an item from the plan; the event keeps its last state.
func (s *Store) RemoveItem(plan, id, note string, ifVersion int64, a Actor) (PlanWrite, error) {
	return s.writePlan(plan, 0, a, func(pt *planTx) error {
		before, err := pt.item(id, ifVersion)
		if err != nil {
			return err
		}
		if _, err := pt.tx.Exec(`DELETE FROM plan_items WHERE plan_id=? AND id=?`, plan, id); err != nil {
			return err
		}
		ids, err := pt.order()
		if err != nil {
			return err
		}
		if err := pt.renumber(ids); err != nil {
			return err
		}
		return pt.event("item.remove", id, note, before, nil)
	})
}

// SetPlanStatus changes the plan status (final freezes it).
func (s *Store) SetPlanStatus(plan, status string, ifPlanVersion int64, a Actor) (PlanWrite, error) {
	if status != PlanDraft && status != PlanLive && status != PlanFinal {
		return PlanWrite{}, refuse("bad_value", "plan status must be draft, live or final")
	}
	return s.writePlan(plan, ifPlanVersion, a, func(pt *planTx) error {
		before := pt.plan.Status
		if before == status {
			return nil
		}
		if _, err := pt.tx.Exec(`UPDATE plans SET status=? WHERE id=?`, status, plan); err != nil {
			return err
		}
		return pt.event("plan.status", "", "", map[string]string{"status": before}, map[string]string{"status": status})
	})
}

// SetPlanCoordinator records which pane hears about human edits.
func (s *Store) SetPlanCoordinator(plan, pane string) error {
	_, err := s.db.Exec(`UPDATE plans SET coordinator=? WHERE id=?`, pane, plan)
	return err
}
