package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/config"
	"github.com/ellingtonsp/herdr-orch/internal/planmd"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

// ---------- event fan-out ----------

// planHub pushes plan events to `plan.subscribe` streams. A subscriber that falls more
// than its buffer behind is dropped; it reconnects with --since and loses nothing.
type planHub struct {
	mu   sync.Mutex
	subs map[chan store.PlanEvent]struct{}
}

func (h *planHub) subscribe() (chan store.PlanEvent, func()) {
	ch := make(chan store.PlanEvent, 256)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[chan store.PlanEvent]struct{}{}
	}
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

func (h *planHub) publish(evs []store.PlanEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		for _, e := range evs {
			select {
			case ch <- e:
			default:
				delete(h.subs, ch)
				close(ch)
			}
			if _, ok := h.subs[ch]; !ok {
				break
			}
		}
	}
}

// ---------- identity ----------

// PlanRef names a plan: project ("" = config default_project, else "default") and day
// ("" = the project's latest plan). Principal is the human the caller acts for.
type PlanRef struct {
	Project   string `json:"project,omitempty"`
	Day       string `json:"day,omitempty"`
	Principal string `json:"principal,omitempty"`
}

func (e *Engine) userConfig() config.Config {
	if e.cfg.UserConfig == nil {
		return config.Config{}
	}
	c, err := e.cfg.UserConfig()
	if err != nil {
		e.logf("config: %v", err)
		return config.Config{}
	}
	return c
}

// actor decides who a plan write is from. A terminal outside herdr (caller "human") acts
// for the configured owner unless it names another principal. Only the owner principal's
// writes are approvals; pane writes are the orchestrator's, or a worker's when the pane
// is a registered worker.
func (e *Engine) actor(caller, principal string) store.Actor {
	owner := e.userConfig().Owner.Principal
	if principal == "" && caller == "human" {
		principal = owner
	}
	a := store.Actor{ID: caller, Principal: principal}
	switch {
	case principal != "" || caller == "human":
		a.Kind = store.ActorHuman
		a.Approval = owner != "" && principal == owner
	case e.isWorker(caller):
		a.Kind = store.ActorWorker
	default:
		a.Kind = store.ActorOrchestrator
	}
	return a
}

func (e *Engine) isWorker(pane string) bool {
	if w, err := e.st.GetWorker(pane); err == nil && w.State == store.WorkerLive {
		return true
	}
	_, err := e.st.ActiveDispatchForPane(pane)
	return err == nil
}

func (e *Engine) planFor(r PlanRef) (store.Plan, error) {
	return e.st.FindPlan(e.project(r.Project), r.Day)
}

func (e *Engine) project(p string) string {
	if p != "" {
		return p
	}
	if d := e.userConfig().DefaultProject; d != "" {
		return d
	}
	return "default"
}

// afterPlanWriteLocked publishes the events and tells the coordinator about human edits.
func (e *Engine) afterPlanWriteLocked(w store.PlanWrite) {
	if len(w.Events) == 0 {
		return
	}
	e.hub.publish(w.Events)
	e.notify.broadcast()
	for _, ev := range w.Events {
		if ev.ActorKind != store.ActorHuman || ev.Op == "import" || ev.Op == "plan.status" {
			continue
		}
		who := ev.Principal
		if who == "" {
			who = ev.Actor
		}
		to := w.View.Plan.Coordinator
		if to == "" {
			to = e.latestCoordinator()
		}
		if to == "" || to == ev.Actor {
			continue
		}
		subject := fmt.Sprintf("plan replanned by %s: %s %s", who, ev.Op, ev.Item)
		if c := planmd.Change(ev); c != "" {
			subject += " (" + c + ")"
		}
		approval := "not an approval"
		if ev.Approval {
			approval = "counts as the owner's approval"
		}
		body := fmt.Sprintf("%s\nplan %s v%d, event %d; %s. `horch plan show --day %s` for the current plan.", subject, ev.PlanID, ev.PlanVersion, ev.Seq, approval, w.View.Plan.Day)
		m, err := e.st.InsertMessage(store.Message{From: store.Daemon, To: to, Kind: store.KindNote, Subject: firstLine(subject), Body: body})
		if err != nil {
			e.logf("plan notify: %v", err)
			continue
		}
		e.badgeMail(m.To)
	}
}

// latestCoordinator is the coordinator pane of the newest running run, if any.
func (e *Engine) latestCoordinator() string {
	rs, err := e.st.ListRuns()
	if err != nil {
		return ""
	}
	var best store.Run
	for _, r := range rs {
		if r.Status == store.RunRunning && isPane(r.CoordinatorPaneID) && r.CreatedAt >= best.CreatedAt {
			best = r
		}
	}
	return best.CoordinatorPaneID
}

// planWrite runs a store write under the engine lock and fans out its events.
func (e *Engine) planWrite(fn func() (store.PlanWrite, error)) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	w, err := fn()
	if err != nil {
		return nil, err
	}
	e.afterPlanWriteLocked(w)
	return w, nil
}

// ---------- ops ----------

type PlanImportArgs struct {
	PlanRef
	Markdown string `json:"markdown"`
	Ref      string `json:"ref,omitempty"` // published_ref: the commit or path the file came from
	Status   string `json:"status,omitempty"`
	Replace  bool   `json:"replace,omitempty"`
}

type PlanImportResult struct {
	store.PlanWrite
	Changed  bool     `json:"changed"`
	Warnings []string `json:"warnings"`
}

func (e *Engine) opPlanImport(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[PlanImportArgs](raw)
	if err != nil {
		return nil, err
	}
	if a.Day == "" {
		return nil, refusal("bad_args", "--day is required")
	}
	if _, err := time.Parse("2006-01-02", a.Day); err != nil {
		return nil, refusal("bad_args", "--day must be YYYY-MM-DD, got %q", a.Day)
	}
	seed, warn, err := planmd.Parse(a.Markdown)
	if err != nil {
		return nil, refusal("bad_plan", "%v", err)
	}
	act := e.actor(caller, a.Principal)
	coord := ""
	if act.Kind == store.ActorOrchestrator {
		coord = caller
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w, err := e.st.ImportPlan(seed, store.ImportOptions{Day: a.Day, Project: e.project(a.Project), Ref: a.Ref, Status: a.Status, Coordinator: coord, Replace: a.Replace}, act)
	if err != nil {
		return nil, err
	}
	e.afterPlanWriteLocked(w)
	if warn == nil {
		warn = []string{}
	}
	return PlanImportResult{PlanWrite: w, Changed: len(w.Events) > 0, Warnings: warn}, nil
}

func (e *Engine) opPlanShow(_ context.Context, _ string, raw json.RawMessage) (any, error) {
	a, err := decode[PlanRef](raw)
	if err != nil {
		return nil, err
	}
	p, err := e.planFor(a)
	if err != nil {
		return nil, err
	}
	return e.st.GetPlanView(p.ID)
}

// PlanItemArgs serves every `plan item` op and `plan transition`.
type PlanItemArgs struct {
	PlanRef
	Item          string          `json:"item"`
	IfVersion     int64           `json:"if_version,omitempty"`      // the item's version
	IfPlanVersion int64           `json:"if_plan_version,omitempty"` // the plan's version (add)
	Position      int             `json:"position,omitempty"`        // add, move (1-based)
	Reason        string          `json:"reason,omitempty"`          // hold
	Note          string          `json:"note,omitempty"`
	Patch         store.ItemPatch `json:"patch"`
}

func (e *Engine) planItemOp(op string) Handler {
	return func(_ context.Context, caller string, raw json.RawMessage) (any, error) {
		a, err := decode[PlanItemArgs](raw)
		if err != nil {
			return nil, err
		}
		if a.Item == "" {
			return nil, refusal("bad_args", "--item is required")
		}
		p, err := e.planFor(a.PlanRef)
		if err != nil {
			return nil, err
		}
		act := e.actor(caller, a.Principal)
		return e.planWrite(func() (store.PlanWrite, error) {
			// Checking the whole plan under the writer lock also fences a stale
			// reorder after another item moved or was added/removed.
			if op != "add" && a.IfPlanVersion != 0 {
				current, err := e.st.GetPlanView(p.ID)
				if err != nil {
					return store.PlanWrite{}, err
				}
				if current.Plan.Version != a.IfPlanVersion {
					return store.PlanWrite{}, refusal("version_conflict", "plan changed: reload and retry")
				}
			}
			switch op {
			case "add":
				it := store.PlanItem{ID: a.Item}
				a.Patch.Apply(&it)
				return e.st.AddItem(p.ID, it, a.Position, a.IfPlanVersion, act)
			case "update":
				return e.st.UpdateItem(p.ID, a.Item, a.Patch, "item.update", a.Note, a.IfVersion, act)
			case "transition":
				if a.Patch.State == nil {
					return store.PlanWrite{}, refusal("bad_args", "--state is required")
				}
				return e.st.UpdateItem(p.ID, a.Item, a.Patch, "transition", a.Note, a.IfVersion, act)
			case "move":
				if a.Position <= 0 {
					return store.PlanWrite{}, refusal("bad_args", "--to POSITION (1-based) is required")
				}
				return e.st.MoveItem(p.ID, a.Item, a.Position, a.IfVersion, act)
			case "hold":
				return e.st.HoldItem(p.ID, a.Item, a.Reason, a.IfVersion, act)
			case "release":
				return e.st.ReleaseItem(p.ID, a.Item, a.Note, a.IfVersion, act)
			case "remove":
				return e.st.RemoveItem(p.ID, a.Item, a.Note, a.IfVersion, act)
			}
			return store.PlanWrite{}, refusal("bad_args", "unknown item op %s", op)
		})
	}
}

type PlanStatusArgs struct {
	PlanRef
	Status        string `json:"status"`
	IfPlanVersion int64  `json:"if_plan_version,omitempty"`
}

func (e *Engine) opPlanStatus(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[PlanStatusArgs](raw)
	if err != nil {
		return nil, err
	}
	p, err := e.planFor(a.PlanRef)
	if err != nil {
		return nil, err
	}
	act := e.actor(caller, a.Principal)
	return e.planWrite(func() (store.PlanWrite, error) { return e.st.SetPlanStatus(p.ID, a.Status, a.IfPlanVersion, act) })
}

type PlanEventsArgs struct {
	PlanRef
	All       bool  `json:"all,omitempty"` // every plan, not just one
	Since     int64 `json:"since,omitempty"`
	Limit     int   `json:"limit,omitempty"`
	Wait      bool  `json:"wait,omitempty"` // long-poll until an event arrives
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
}

func (e *Engine) planEventsScope(a PlanEventsArgs) (string, error) {
	if a.All {
		return "", nil
	}
	p, err := e.planFor(a.PlanRef)
	return p.ID, err
}

func (e *Engine) opPlanEvents(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
	a, err := decode[PlanEventsArgs](raw)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(a.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	deadline := time.After(timeout)
	for {
		wake := e.notify.wait()
		scope, err := e.planEventsScope(a)
		if err != nil && !(a.Wait && isNoPlan(err)) {
			return nil, err
		}
		evs := []store.PlanEvent{}
		if err == nil {
			if evs, err = e.st.PlanEvents(scope, a.Since, a.Limit); err != nil {
				return nil, err
			}
		}
		if len(evs) > 0 || !a.Wait {
			return evs, nil
		}
		select {
		case <-wake:
		case <-deadline:
			return evs, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func isNoPlan(err error) bool {
	r, ok := AsRefusal(err)
	return ok && r.Code == "no_plan"
}

// streamPlanEvents serves `plan.subscribe`: the backlog after Since, then every new event
// as it is written, one response line each, until the client hangs up.
func (e *Engine) streamPlanEvents(ctx context.Context, _ string, raw json.RawMessage, send func(any) error) error {
	a, err := decode[PlanEventsArgs](raw)
	if err != nil {
		return err
	}
	// Subscribe before reading the backlog so nothing falls between the two.
	ch, cancel := e.hub.subscribe()
	defer cancel()
	scope := ""
	if !a.All {
		// Wait for the plan to exist (a subscriber may start before the import).
		for {
			wake := e.notify.wait()
			p, err := e.planFor(a.PlanRef)
			if err == nil {
				scope = p.ID
				break
			}
			if !isNoPlan(err) {
				return err
			}
			select {
			case <-wake:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	last := a.Since
	for {
		evs, err := e.st.PlanEvents(scope, last, 1000)
		if err != nil {
			return err
		}
		for _, ev := range evs {
			if err := send(ev); err != nil {
				return err
			}
			last = ev.Seq
		}
		if len(evs) < 1000 {
			break
		}
	}
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return refusal("lagging", "subscriber fell behind; reconnect with --since %d", last)
			}
			if ev.Seq <= last || (scope != "" && ev.PlanID != scope) {
				continue
			}
			if err := send(ev); err != nil {
				return err
			}
			last = ev.Seq
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type PlanExportArgs struct {
	PlanRef
	Format   string `json:"format,omitempty"` // md (default) or json
	Finalize bool   `json:"finalize,omitempty"`
}

type PlanExport struct {
	Plan     store.Plan        `json:"plan"`
	Markdown string            `json:"markdown,omitempty"`
	View     *store.PlanView   `json:"view,omitempty"`
	Events   []store.PlanEvent `json:"events"`
}

func (e *Engine) opPlanExport(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[PlanExportArgs](raw)
	if err != nil {
		return nil, err
	}
	if a.Format == "" {
		a.Format = "md"
	}
	if a.Format != "md" && a.Format != "json" {
		return nil, refusal("bad_args", "--format must be md or json")
	}
	// Keep finalization and its snapshot together with respect to daemon writes;
	// the store snapshot also protects against independent SQLite connections.
	e.mu.Lock()
	defer e.mu.Unlock()
	p, err := e.planFor(a.PlanRef)
	if err != nil {
		return nil, err
	}
	if a.Finalize && p.Status != store.PlanFinal {
		act := e.actor(caller, a.Principal)
		w, err := e.st.SetPlanStatus(p.ID, store.PlanFinal, 0, act)
		if err != nil {
			return nil, err
		}
		e.afterPlanWriteLocked(w)
	}
	v, evs, err := e.st.GetPlanSnapshot(p.ID)
	if err != nil {
		return nil, err
	}
	out := PlanExport{Plan: v.Plan, Events: evs}
	if a.Format == "md" {
		out.Markdown = planmd.Render(v, evs)
	} else {
		out.View = &v
	}
	return out, nil
}

func (e *Engine) opPlanConfig(context.Context, string, json.RawMessage) (any, error) {
	c := e.userConfig()
	missing := c.Missing()
	if missing == nil {
		missing = []string{}
	}
	return map[string]any{"config": c, "missing": missing}, nil
}
