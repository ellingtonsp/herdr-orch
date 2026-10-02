package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/herdr"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

// Handler serves one op.
type Handler func(ctx context.Context, caller string, args json.RawMessage) (any, error)

// Ops returns the op table served on orch.sock.
func (e *Engine) Ops() map[string]Handler {
	return map[string]Handler{
		"run.create":      e.opRunCreate,
		"run.use":         e.opRunUse,
		"run.current":     e.opRunCurrent,
		"run.list":        e.opRunList,
		"run.show":        e.opRunShow,
		"run.update":      e.opRunUpdate,
		"send":            e.opSend,
		"check":           e.opCheck,
		"ask":             e.opAsk,
		"reply":           e.opReply,
		"inbox":           e.opInbox,
		"task.create":     e.opTaskCreate,
		"task.list":       e.opTaskList,
		"task.show":       e.opTaskShow,
		"task.update":     e.opTaskUpdate,
		"dispatch":        e.opDispatch,
		"dispatch.show":   e.opDispatchShow,
		"dispatch.next":   e.opDispatchNext,
		"dispatch.nudge":  e.opDispatchNudge,
		"dispatch.fail":   e.opDispatchFail,
		"gate.create":     e.opGateCreate,
		"gate.resolve":    e.opGateResolve,
		"gate.list":       e.opGateList,
		"worker.start":    e.opWorkerStart,
		"worker.show":     e.opWorkerShow,
		"worker.read":     e.opWorkerRead,
		"worker.list":     e.opWorkerList,
		"worker.stop":     e.opWorkerStop,
		"worker.release":  e.opWorkerRelease,
		"worker.abandon":  e.opWorkerAbandon,
		"worker.retain":   e.opWorkerRetain,
		"worker.register": e.opWorkerRegister,
		"schedule.add":    e.opScheduleAdd,
		"schedule.list":   e.opScheduleList,
		"schedule.rm":     e.opScheduleRm,
		"schedule.run":    e.opScheduleRun,
		"schedule.enable": e.opScheduleEnable,
		"board":           e.opBoard,

		"plan.import":       e.opPlanImport,
		"plan.show":         e.opPlanShow,
		"plan.item.add":     e.planItemOp("add"),
		"plan.item.update":  e.planItemOp("update"),
		"plan.item.move":    e.planItemOp("move"),
		"plan.item.hold":    e.planItemOp("hold"),
		"plan.item.release": e.planItemOp("release"),
		"plan.item.remove":  e.planItemOp("remove"),
		"plan.transition":   e.planItemOp("transition"),
		"plan.status":       e.opPlanStatus,
		"plan.events":       e.opPlanEvents,
		"plan.export":       e.opPlanExport,
		"plan.config":       e.opPlanConfig,
	}
}

// StreamHandler serves a streaming op: it calls send once per result line until it
// returns (or the client hangs up).
type StreamHandler func(ctx context.Context, caller string, args json.RawMessage, send func(any) error) error

// Streams returns the streaming ops served on orch.sock.
func (e *Engine) Streams() map[string]StreamHandler {
	return map[string]StreamHandler{
		"plan.subscribe": e.streamPlanEvents,
	}
}

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, refusal("bad_args", "%v", err)
	}
	return v, nil
}

// runFor resolves an explicit run id or the caller's current run.
func (e *Engine) runFor(caller, run string) (store.Run, error) {
	if run != "" {
		return e.st.GetRun(run)
	}
	return e.st.CurrentRun(caller)
}

// ---------- runs ----------

type RunCreateArgs struct {
	Title        string `json:"title"`
	Coordinator  string `json:"coordinator,omitempty"`
	AutoDispatch bool   `json:"auto_dispatch,omitempty"`
	MaxAttempts  int    `json:"max_attempts,omitempty"`
}

func (e *Engine) opRunCreate(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[RunCreateArgs](raw)
	if err != nil {
		return nil, err
	}
	if a.Title == "" {
		return nil, refusal("bad_args", "--title is required")
	}
	coord := a.Coordinator
	if coord == "" && isPane(caller) {
		coord = caller
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r, err := e.st.CreateRun(a.Title, coord)
	if err != nil {
		return nil, err
	}
	u := store.RunUpdate{}
	if a.AutoDispatch {
		u.AutoDispatch = &a.AutoDispatch
	}
	if a.MaxAttempts > 0 {
		u.MaxAttempts = &a.MaxAttempts
	}
	if u.AutoDispatch != nil || u.MaxAttempts != nil {
		r, err = e.st.UpdateRun(r.ID, u)
	}
	e.notify.broadcast()
	return r, err
}

type RunRef struct {
	Run string `json:"run,omitempty"`
}

func (e *Engine) opRunUse(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[RunRef](raw)
	if err != nil {
		return nil, err
	}
	if a.Run == "" {
		return nil, refusal("bad_args", "run id required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.st.UseRun(caller, a.Run); err != nil {
		return nil, err
	}
	return e.st.GetRun(a.Run)
}

func (e *Engine) opRunCurrent(_ context.Context, caller string, _ json.RawMessage) (any, error) {
	return e.st.CurrentRun(caller)
}

func (e *Engine) opRunList(context.Context, string, json.RawMessage) (any, error) {
	return e.st.ListRuns()
}

type RunShow struct {
	Run        store.Run        `json:"run"`
	Tasks      []store.Task     `json:"tasks"`
	Gates      []store.Gate     `json:"gates"`
	Workers    []store.Worker   `json:"workers"`
	Dispatches []store.Dispatch `json:"active_dispatches"`
}

func (e *Engine) opRunShow(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[RunRef](raw)
	if err != nil {
		return nil, err
	}
	r, err := e.runFor(caller, a.Run)
	if err != nil {
		return nil, err
	}
	return e.runShow(r)
}

func (e *Engine) runShow(r store.Run) (RunShow, error) {
	out := RunShow{Run: r}
	var err error
	if out.Tasks, err = e.st.ListTasks(r.ID); err != nil {
		return out, err
	}
	if out.Gates, err = e.st.ListGates(r.ID, false); err != nil {
		return out, err
	}
	if out.Workers, err = e.st.ListWorkers(r.ID, false); err != nil {
		return out, err
	}
	ds, err := e.st.ActiveDispatches()
	if err != nil {
		return out, err
	}
	out.Dispatches = []store.Dispatch{}
	for _, d := range ds {
		if d.RunID == r.ID {
			out.Dispatches = append(out.Dispatches, d)
		}
	}
	return out, nil
}

type RunUpdateArgs struct {
	Run          string  `json:"run,omitempty"`
	Status       *string `json:"status,omitempty"`
	Coordinator  *string `json:"coordinator,omitempty"`
	AutoDispatch *bool   `json:"auto_dispatch,omitempty"`
	MaxAttempts  *int    `json:"max_attempts,omitempty"`
	Title        *string `json:"title,omitempty"`
	IdleReportMS *int64  `json:"idle_report_ms,omitempty"`
	IdleFlagMS   *int64  `json:"idle_flag_ms,omitempty"`
}

func (e *Engine) opRunUpdate(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[RunUpdateArgs](raw)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r, err := e.runFor(caller, a.Run)
	if err != nil {
		return nil, err
	}
	if a.Coordinator != nil && *a.Coordinator == "me" {
		*a.Coordinator = caller
	}
	r, err = e.st.UpdateRun(r.ID, store.RunUpdate{Status: a.Status, Coordinator: a.Coordinator, AutoDispatch: a.AutoDispatch, MaxAttempts: a.MaxAttempts, Title: a.Title, IdleReportMS: a.IdleReportMS, IdleFlagMS: a.IdleFlagMS})
	if err != nil {
		return nil, err
	}
	if a.AutoDispatch != nil && *a.AutoDispatch {
		e.autoDispatchLocked(r.ID)
	}
	e.notify.broadcast()
	return r, nil
}

// ---------- messages ----------

type SendArgs struct {
	Run     string `json:"run,omitempty"`
	To      string `json:"to"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Task    string `json:"task,omitempty"`
	Failed  bool   `json:"failed,omitempty"`
}

// resolveRecipients maps --to to pane ids: coordinator | run | human | horch | <pane> |
// <worker name> | <herdr agent name>.
func (e *Engine) resolveRecipients(ctx context.Context, caller string, run store.Run, to string) ([]string, error) {
	switch to {
	case "":
		return nil, refusal("bad_args", "--to is required")
	case "coordinator":
		if run.CoordinatorPaneID == "" {
			return []string{"human"}, nil
		}
		return []string{run.CoordinatorPaneID}, nil
	case "run":
		members, err := e.st.RunMembers(run.ID)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, m := range members {
			if m != caller {
				out = append(out, m)
			}
		}
		if len(out) == 0 {
			return nil, refusal("no_recipient", "run %s has no other members", run.ID)
		}
		return out, nil
	case "human", store.Daemon:
		return []string{to}, nil
	}
	if isPane(to) {
		return []string{to}, nil
	}
	if w, err := e.st.FindWorker(to); err == nil {
		return []string{w.PaneID}, nil
	}
	if a, err := e.h.AgentGet(ctx, to); err == nil && a.PaneID != "" {
		return []string{a.PaneID}, nil
	}
	return nil, refusal("no_recipient", "cannot resolve recipient %q (use coordinator, run, human, a pane id, or a worker/agent name)", to)
}

func (e *Engine) opSend(ctx context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[SendArgs](raw)
	if err != nil {
		return nil, err
	}
	if a.Kind == "" {
		a.Kind = store.KindNote
	}
	if a.Kind == store.KindDone {
		return e.done(caller, a)
	}
	if a.Kind == store.KindReply {
		return nil, refusal("bad_kind", "use `horch reply --id <question> --body ...` to reply")
	}
	run, err := e.runFor(caller, a.Run)
	if err != nil {
		return nil, err
	}
	to, err := e.resolveRecipients(ctx, caller, run, a.To)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []store.Message
	for _, r := range to {
		m, err := e.st.InsertMessage(store.Message{RunID: run.ID, From: caller, To: r, Subject: a.Subject, Body: a.Body, Kind: a.Kind, TaskID: a.Task})
		if err != nil {
			return nil, err
		}
		out = append(out, m)
		e.badgeMail(r)
		if a.Kind == store.KindEscalation {
			e.async(func() {
				ctx, cancel := bg()
				defer cancel()
				_ = e.h.Notify(ctx, "horch escalation from "+caller, firstLine(a.Subject+" "+a.Body), true)
			})
		}
	}
	e.notify.broadcast()
	return out, nil
}

// done records a worker's completion report against its active dispatch. The message goes
// to the daemon; the coordinator hears about it once the dispatch settles.
func (e *Engine) done(caller string, a SendArgs) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var d store.Dispatch
	var err error
	if a.Task != "" {
		ds, err := e.st.DispatchesForTask(a.Task)
		if err != nil {
			return nil, err
		}
		found := false
		for _, x := range ds {
			if x.Active() {
				d, found = x, true
			}
		}
		if !found {
			return nil, refusal("no_active_dispatch", "task %s has no active dispatch", a.Task)
		}
		if isPane(caller) && d.PaneID != caller {
			return nil, refusal("not_assignee", "task %s is dispatched to %s, not %s", a.Task, d.PaneID, caller)
		}
	} else {
		d, err = e.st.ActiveDispatchForPane(caller)
		if err != nil {
			return nil, refusal("no_active_dispatch", "pane %s has no active dispatch; pass --task", caller)
		}
	}
	if d.DoneMessageID != "" {
		return nil, refusal("already_done", "dispatch %s already has a done report (%s)", d.ID, d.DoneMessageID)
	}
	outcome := "succeeded"
	if a.Failed {
		outcome = "failed"
	}
	m, err := e.st.InsertMessage(store.Message{RunID: d.RunID, From: caller, To: store.Daemon, Subject: a.Subject, Body: a.Body, Kind: store.KindDone, TaskID: d.TaskID, Outcome: outcome})
	if err != nil {
		return nil, err
	}
	d, err = e.st.RecordDone(d.ID, m.ID, outcome)
	if err != nil {
		return nil, err
	}
	e.evaluateLocked(d)
	e.notify.broadcast()
	d, _ = e.st.GetDispatch(d.ID)
	return map[string]any{"message": m, "dispatch": d}, nil
}

type CheckArgs struct {
	Wait      bool  `json:"wait,omitempty"`
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
	Peek      bool  `json:"peek,omitempty"`
}

func (e *Engine) opCheck(ctx context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[CheckArgs](raw)
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
		e.mu.Lock()
		msgs, err := e.st.Unread([]string{caller}, !a.Peek)
		if err == nil && len(msgs) > 0 && !a.Peek {
			e.badgeMail(caller)
		}
		e.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if len(msgs) > 0 || !a.Wait {
			return msgs, nil
		}
		select {
		case <-wake:
		case <-deadline:
			return []store.Message{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

type AskArgs struct {
	Run       string `json:"run,omitempty"`
	To        string `json:"to,omitempty"`
	Question  string `json:"question"`
	Task      string `json:"task,omitempty"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
}

func (e *Engine) opAsk(ctx context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[AskArgs](raw)
	if err != nil {
		return nil, err
	}
	if a.Question == "" {
		return nil, refusal("bad_args", "--question is required")
	}
	if a.To == "" {
		a.To = "coordinator"
	}
	run, err := e.runFor(caller, a.Run)
	if err != nil {
		return nil, err
	}
	to, err := e.resolveRecipients(ctx, caller, run, a.To)
	if err != nil {
		return nil, err
	}
	if len(to) != 1 {
		return nil, refusal("bad_args", "ask needs a single recipient")
	}
	if a.Task == "" {
		if d, err := e.st.ActiveDispatchForPane(caller); err == nil {
			a.Task = d.TaskID
		}
	}
	e.mu.Lock()
	q, err := e.st.InsertMessage(store.Message{RunID: run.ID, From: caller, To: to[0], Kind: store.KindQuestion, Subject: "question", Body: a.Question, TaskID: a.Task})
	if err == nil {
		e.badgeMail(to[0])
		if to[0] == "human" {
			e.async(func() {
				ctx, cancel := bg()
				defer cancel()
				_ = e.h.Notify(ctx, "horch question from "+caller, firstLine(a.Question), true)
			})
		}
	}
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	e.notify.broadcast()
	return e.awaitReply(ctx, q, time.Duration(a.TimeoutMS)*time.Millisecond)
}

func (e *Engine) awaitReply(ctx context.Context, q store.Message, timeout time.Duration) (any, error) {
	if timeout <= 0 {
		// An ask must outlive several coordinator ticks and any authorization that has to
		// reach a human.
		timeout = 2 * time.Hour
	}
	deadline := time.After(timeout)
	for {
		wake := e.notify.wait()
		e.mu.Lock()
		r, err := e.st.TakeReply(q.ID)
		e.mu.Unlock()
		if err == nil {
			return map[string]any{"question": q, "reply": r}, nil
		}
		if err != store.ErrNotFound {
			return nil, err
		}
		select {
		case <-wake:
		case <-deadline:
			return nil, refusal("timeout", "no reply to %s within %s; the reply will still show up in `horch check`", q.ID, timeout)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

type ReplyArgs struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

func (e *Engine) opReply(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[ReplyArgs](raw)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	q, err := e.st.GetMessage(a.ID)
	if err != nil {
		return nil, err
	}
	if q.Kind != store.KindQuestion {
		return nil, refusal("not_a_question", "message %s is a %s, not a question", a.ID, q.Kind)
	}
	m, err := e.st.InsertMessage(store.Message{RunID: q.RunID, From: caller, To: q.From, Kind: store.KindReply, Subject: "re: question", Body: a.Body, ReplyTo: q.ID, TaskID: q.TaskID})
	if err != nil {
		return nil, err
	}
	e.badgeMail(q.From)
	e.notify.broadcast()
	return m, nil
}

type InboxArgs struct {
	Limit int    `json:"limit,omitempty"`
	All   bool   `json:"all,omitempty"`
	Run   string `json:"run,omitempty"`
}

func (e *Engine) opInbox(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[InboxArgs](raw)
	if err != nil {
		return nil, err
	}
	if a.All {
		return e.st.RecentMessages(a.Run, a.Limit)
	}
	return e.st.Inbox(caller, a.Limit)
}

// ---------- tasks ----------

type TaskCreateArgs struct {
	Run   string   `json:"run,omitempty"`
	Title string   `json:"title"`
	Spec  string   `json:"spec,omitempty"`
	Deps  []string `json:"deps,omitempty"`
}

func (e *Engine) opTaskCreate(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[TaskCreateArgs](raw)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	run, err := e.runFor(caller, a.Run)
	if err != nil {
		return nil, err
	}
	t, err := e.st.CreateTask(run.ID, a.Title, a.Spec, a.Deps)
	if err != nil {
		return nil, err
	}
	if t.Status == store.TaskReady {
		e.autoDispatchLocked(run.ID)
		t, _ = e.st.GetTask(t.ID)
	}
	e.notify.broadcast()
	return t, nil
}

func (e *Engine) opTaskList(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[RunRef](raw)
	if err != nil {
		return nil, err
	}
	run, err := e.runFor(caller, a.Run)
	if err != nil {
		return nil, err
	}
	return e.st.ListTasks(run.ID)
}

type TaskRef struct {
	ID string `json:"id"`
}

func (e *Engine) opTaskShow(_ context.Context, _ string, raw json.RawMessage) (any, error) {
	a, err := decode[TaskRef](raw)
	if err != nil {
		return nil, err
	}
	t, err := e.st.GetTask(a.ID)
	if err != nil {
		return nil, err
	}
	ds, err := e.st.DispatchesForTask(a.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"task": t, "dispatches": ds}, nil
}

type TaskUpdateArgs struct {
	ID            string    `json:"id"`
	Title         *string   `json:"title,omitempty"`
	Spec          *string   `json:"spec,omitempty"`
	Status        *string   `json:"status,omitempty"`
	Result        *string   `json:"result,omitempty"`
	Deps          *[]string `json:"deps,omitempty"`
	ResetAttempts bool      `json:"reset_attempts,omitempty"`
}

func (e *Engine) opTaskUpdate(_ context.Context, _ string, raw json.RawMessage) (any, error) {
	a, err := decode[TaskUpdateArgs](raw)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ready, err := e.st.UpdateTask(a.ID, store.TaskUpdate{Title: a.Title, Spec: a.Spec, Status: a.Status, Result: a.Result, Deps: a.Deps, ResetAttempts: a.ResetAttempts})
	if err != nil {
		return nil, err
	}
	if len(ready) > 0 {
		e.autoDispatchLocked(t.RunID)
		t, _ = e.st.GetTask(t.ID)
	}
	e.notify.broadcast()
	return t, nil
}

// ---------- dispatch ----------

type DispatchArgs struct {
	Run   string `json:"run,omitempty"`
	Task  string `json:"task,omitempty"`
	To    string `json:"to"`
	Title string `json:"title,omitempty"`
	Spec  string `json:"spec,omitempty"`
	// For --to new:<agent>.
	Cwd  string `json:"cwd,omitempty"`
	Name string `json:"name,omitempty"`
}

func (e *Engine) opDispatch(ctx context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[DispatchArgs](raw)
	if err != nil {
		return nil, err
	}
	if a.To == "" {
		return nil, refusal("bad_args", "--to is required")
	}
	run, err := e.runFor(caller, a.Run)
	if err != nil {
		return nil, err
	}
	if a.Task == "" {
		if a.Spec == "" {
			return nil, refusal("bad_args", "--task or --spec is required")
		}
		title := a.Title
		if title == "" {
			title = firstLine(a.Spec)
			if len(title) > 60 {
				title = title[:60]
			}
		}
		e.mu.Lock()
		t, err := e.st.CreateTask(run.ID, title, a.Spec, nil)
		e.mu.Unlock()
		if err != nil {
			return nil, err
		}
		a.Task = t.ID
	}
	t, err := e.st.GetTask(a.Task)
	if err != nil {
		return nil, err
	}
	if t.Status != store.TaskReady {
		return nil, refusal("task_not_ready", "task %s is %s, not ready", t.ID, t.Status)
	}
	var pane string
	if kind, ok := strings.CutPrefix(a.To, "new:"); ok {
		w, err := e.startWorker(ctx, caller, WorkerStartArgs{Run: t.RunID, Agent: kind, Cwd: a.Cwd, Name: a.Name})
		if err != nil {
			return nil, err
		}
		pane = w.PaneID
	} else {
		to, err := e.resolveRecipients(ctx, caller, run, a.To)
		if err != nil {
			return nil, err
		}
		if len(to) != 1 || !isPane(to[0]) {
			return nil, refusal("bad_args", "dispatch needs a single worker pane")
		}
		pane = to[0]
	}
	return e.dispatchTo(ctx, t.ID, pane)
}

// dispatchTo claims the task, submits the prompt and marks the dispatch sent. A failed
// submission fences the dispatch (the attempt is not counted) and refuses.
func (e *Engine) dispatchTo(ctx context.Context, taskID, pane string) (store.Dispatch, error) {
	info, err := e.h.PaneGet(ctx, pane)
	if err != nil {
		return store.Dispatch{}, refusal("unknown_pane", "pane %s: %v", pane, err)
	}
	switch info.AgentStatus {
	case "working":
		return store.Dispatch{}, refusal("worker_busy", "pane %s is working; wait for it to go idle", pane)
	case "blocked":
		return store.Dispatch{}, refusal("agent_blocked", "pane %s is blocked at a prompt; resolve that first", pane)
	}
	agent := ""
	if info.Agent != nil {
		agent = *info.Agent
	}
	e.mu.Lock()
	t, err := e.st.GetTask(taskID)
	if err != nil {
		e.mu.Unlock()
		return store.Dispatch{}, err
	}
	prompt := buildPrompt(t, "", e.horchCmd(pane))
	sum := sha256.Sum256([]byte(prompt))
	d, err := e.st.BeginDispatch(taskID, pane, agent, hex.EncodeToString(sum[:8]))
	if err == nil {
		e.status[pane] = info.AgentStatus
		_ = e.st.SetFlagged(pane, 0)
		e.recomputeWatch()
	}
	e.mu.Unlock()
	if err != nil {
		return store.Dispatch{}, err
	}
	prompt = buildPrompt(t, d.ID, e.horchCmd(pane))
	via, sendErr := e.submit(ctx, pane, prompt)

	e.mu.Lock()
	defer e.mu.Unlock()
	if sendErr != nil {
		st, ferr := e.st.Fence(d.ID, "prompt not delivered: "+sendErr.Error())
		if ferr == nil {
			e.recomputeWatch()
			e.notify.broadcast()
			_ = st
		}
		code := herdr.ErrCode(sendErr)
		if code == "" {
			code = "send_failed"
		}
		return store.Dispatch{}, refusal(code, "could not deliver task %s to %s: %v", taskID, pane, sendErr)
	}
	if err := e.st.MarkSent(d.ID, "sent via "+via); err != nil {
		return store.Dispatch{}, err
	}
	e.badgeTask(pane, taskID)
	e.notify.broadcast()
	d, err = e.st.GetDispatch(d.ID)
	if err == nil {
		e.evaluateLocked(d)
	}
	return d, err
}

// submit delivers a prompt with agent.prompt. An agent herdr started but has not marked
// interactive yet is retried for a while; only panes herdr does not manage as named agents
// (e.g. report-agent integrations) fall back to typed text + Enter.
func (e *Engine) submit(ctx context.Context, pane, prompt string) (string, error) {
	err := e.h.AgentPrompt(ctx, pane, prompt)
	if err == nil {
		return "agent.prompt", nil
	}
	notNamed := herdr.ErrCode(err) == "agent_not_ready" && strings.Contains(err.Error(), "not an active named agent") ||
		herdr.ErrCode(err) == "agent_not_found"
	if !notNamed {
		return "", err
	}
	if a, gerr := e.h.AgentGet(ctx, pane); gerr == nil && a.Name != nil && *a.Name != "" {
		for i := 0; i < 20; i++ {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(e.promptRetry):
			}
			if err = e.h.AgentPrompt(ctx, pane, prompt); err == nil {
				return fmt.Sprintf("agent.prompt (after %d retries)", i+1), nil
			}
		}
		return "", err
	}
	flat := strings.Join(strings.Fields(strings.ReplaceAll(prompt, "\n", " ")), " ")
	if err2 := e.h.SendText(ctx, pane, flat); err2 != nil {
		return "", err2
	}
	return "pane.send_text (" + err.Error() + ")", nil
}

// horchCmd is the horch invocation a worker in pane should run. It pins the binary, the
// herdr session and the caller identity: agents such as codex run shell commands from
// their own long-lived process, without the pane's PATH or HERDR_* environment.
func (e *Engine) horchCmd(pane string) string {
	bin := "horch"
	if e.cfg.HorchBin != "" {
		bin = e.cfg.HorchBin
	}
	if e.cfg.HerdrSock == "" {
		return "HORCH_AS=" + pane + " " + bin
	}
	return "HERDR_SOCKET_PATH=" + e.cfg.HerdrSock + " HORCH_AS=" + pane + " " + bin
}

// buildPrompt wraps the spec with the reporting instructions.
func buildPrompt(t store.Task, dispatchID, horch string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[horch dispatch %s · run %s · task %s: %s]\n\n", dispatchID, t.RunID, t.ID, t.Title)
	if t.Spec != "" {
		b.WriteString(t.Spec)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "---\nWhen you have finished, report back by running exactly one of:\n")
	fmt.Fprintf(&b, "  %s done --task %s --body \"<summary of what you did; branch/commit/PR if any>\"\n", horch, t.ID)
	fmt.Fprintf(&b, "  %s done --task %s --failed --body \"<why it failed>\"\n", horch, t.ID)
	fmt.Fprintf(&b, "If you need a decision first, run `%s ask --question \"...\"` (it blocks until answered).\n", horch)
	fmt.Fprintf(&b, "Do not end your turn without running `%s done`.", horch)
	return b.String()
}

type DispatchShowArgs struct {
	Task string `json:"task,omitempty"`
	ID   string `json:"id,omitempty"`
}

type DispatchView struct {
	store.Dispatch
	LiveStatus string `json:"live_status,omitempty"`
	Mismatch   string `json:"mismatch,omitempty"`
}

func (e *Engine) opDispatchShow(_ context.Context, _ string, raw json.RawMessage) (any, error) {
	a, err := decode[DispatchShowArgs](raw)
	if err != nil {
		return nil, err
	}
	var ds []store.Dispatch
	if a.ID != "" {
		d, err := e.st.GetDispatch(a.ID)
		if err != nil {
			return nil, err
		}
		ds = []store.Dispatch{d}
	} else {
		if ds, err = e.st.DispatchesForTask(a.Task); err != nil {
			return nil, err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []DispatchView{}
	for _, d := range ds {
		v := DispatchView{Dispatch: d}
		if d.Active() {
			v.LiveStatus = e.status[d.PaneID]
			switch {
			case d.DoneMessageID != "" && v.LiveStatus == "working":
				v.Mismatch = "worker reported done but herdr still shows it working"
			case d.DoneMessageID == "" && d.SawWorking && (v.LiveStatus == "idle" || v.LiveStatus == "done"):
				v.Mismatch = "herdr shows the worker idle but it has not reported done"
			}
		}
		out = append(out, v)
	}
	return out, nil
}

func (e *Engine) opDispatchNext(ctx context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[RunRef](raw)
	if err != nil {
		return nil, err
	}
	run, err := e.runFor(caller, a.Run)
	if err != nil {
		return nil, err
	}
	return e.dispatchNext(ctx, run.ID)
}

// idleWorkersLocked lists live, unretained workers in run with no active dispatch whose
// agent is ready.
func (e *Engine) idleWorkersLocked(run string) []store.Worker {
	ws, err := e.st.ListWorkers(run, true)
	if err != nil {
		return nil
	}
	var out []store.Worker
	for _, w := range ws {
		if w.State != store.WorkerLive || w.Retained {
			// Retained workers (e.g. a PR owner waiting on its merge) are reserved.
			continue
		}
		if _, err := e.st.ActiveDispatchForPane(w.PaneID); err == nil {
			continue
		}
		if s := e.status[w.PaneID]; s == "idle" || s == "done" {
			out = append(out, w)
		}
	}
	return out
}

// dispatchNext pairs ready tasks with idle workers in run, oldest task first.
func (e *Engine) dispatchNext(ctx context.Context, run string) ([]store.Dispatch, error) {
	e.mu.Lock()
	tasks, err := e.st.ReadyTasks(run)
	workers := e.idleWorkersLocked(run)
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := []store.Dispatch{}
	for i := 0; i < len(tasks) && i < len(workers); i++ {
		d, err := e.dispatchTo(ctx, tasks[i].ID, workers[i].PaneID)
		if err != nil {
			e.logf("dispatch next %s → %s: %v", tasks[i].ID, workers[i].PaneID, err)
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// autoDispatchLocked schedules dispatchNext for runs with auto_dispatch on. It runs
// asynchronously because dispatching calls herdr and takes the engine lock itself.
func (e *Engine) autoDispatchLocked(run string) {
	r, err := e.st.GetRun(run)
	if err != nil || !r.AutoDispatch || r.Status != store.RunRunning {
		return
	}
	e.async(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = e.dispatchNext(ctx, run)
	})
}

type DispatchActArgs struct {
	Task   string `json:"task"`
	Reason string `json:"reason,omitempty"`
	Text   string `json:"text,omitempty"`
	Force  bool   `json:"force,omitempty"`
}

func (e *Engine) activeDispatchForTask(task string) (store.Dispatch, error) {
	if task == "" {
		return store.Dispatch{}, refusal("bad_args", "--task is required")
	}
	ds, err := e.st.DispatchesForTask(task)
	if err != nil {
		return store.Dispatch{}, err
	}
	for i := len(ds) - 1; i >= 0; i-- {
		if ds[i].Active() {
			return ds[i], nil
		}
	}
	return store.Dispatch{}, refusal("no_active_dispatch", "task %s has no active dispatch", task)
}

// opDispatchNudge reminds an idle worker to report. It refuses while the worker is
// working or blocked (unless --force), so it never interrupts a turn in progress.
func (e *Engine) opDispatchNudge(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
	a, err := decode[DispatchActArgs](raw)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	d, err := e.activeDispatchForTask(a.Task)
	status := e.status[d.PaneID]
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !a.Force && (status == "working" || status == "blocked") {
		return nil, refusal("worker_busy", "pane %s is %s; a nudge would interrupt it (use --force to send anyway)", d.PaneID, status)
	}
	t, err := e.st.GetTask(d.TaskID)
	if err != nil {
		return nil, err
	}
	cmd := e.horchCmd(d.PaneID)
	text := a.Text
	if text == "" {
		text = fmt.Sprintf("[horch nudge · task %s · dispatch %s] You went idle without reporting on %q. "+
			"If it is finished, run: %s done --task %s --body \"<summary>\". "+
			"If it failed, run: %s done --task %s --failed --body \"<why>\". "+
			"If you are still on it (waiting on CI, a loop, a background job), reply in one line saying what you are waiting on and carry on.",
			t.ID, d.ID, t.Title, cmd, t.ID, cmd, t.ID)
	}
	via, err := e.submit(ctx, d.PaneID, text)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	n, err := e.st.AddNudge(d.ID)
	if err != nil {
		return nil, err
	}
	d, _ = e.st.GetDispatch(d.ID)
	return map[string]any{"dispatch": d, "nudges": n, "via": via}, nil
}

// opDispatchFail is the coordinator declaring a dispatch failed (a positive decision, so
// the attempt counts toward the circuit breaker).
func (e *Engine) opDispatchFail(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[DispatchActArgs](raw)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Reason) == "" {
		return nil, refusal("bad_args", "--reason is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.activeDispatchForTask(a.Task)
	if err != nil {
		return nil, err
	}
	st, err := e.st.Settle(d.ID, false, "failed: "+a.Reason+" (by "+caller+")", "")
	if err != nil {
		return nil, err
	}
	e.logf("dispatch %s failed by %s: %s", d.ID, caller, a.Reason)
	e.afterSettleLocked(st)
	return st, nil
}

// ---------- gates ----------

type GateCreateArgs struct {
	Run       string   `json:"run,omitempty"`
	Task      string   `json:"task,omitempty"`
	Question  string   `json:"question"`
	Options   []string `json:"options,omitempty"`
	TimeoutMS int64    `json:"timeout_ms,omitempty"`
}

func (e *Engine) opGateCreate(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[GateCreateArgs](raw)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	runID := ""
	if a.Task == "" {
		run, err := e.runFor(caller, a.Run)
		if err != nil {
			return nil, err
		}
		runID = run.ID
	}
	var timeoutAt int64
	if a.TimeoutMS > 0 {
		timeoutAt = e.st.Now().Add(time.Duration(a.TimeoutMS) * time.Millisecond).UnixMilli()
	}
	g, err := e.st.CreateGate(runID, a.Task, a.Question, a.Options, timeoutAt)
	if err != nil {
		return nil, err
	}
	opts := ""
	if len(g.Options) > 0 {
		opts = " [" + strings.Join(g.Options, "/") + "]"
	}
	e.async(func() {
		ctx, cancel := bg()
		defer cancel()
		_ = e.h.Notify(ctx, "horch: decision needed ("+g.ID+")", g.Question+opts, true)
	})
	e.notify.broadcast()
	return g, nil
}

type GateResolveArgs struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
}

func (e *Engine) opGateResolve(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[GateResolveArgs](raw)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	g, ready, err := e.st.ResolveGate(a.ID, a.Decision, caller)
	if err != nil {
		return nil, err
	}
	body := fmt.Sprintf("Gate %s (%q) resolved by %s: %s.", g.ID, g.Question, caller, g.Decision)
	if g.TaskID != "" {
		t, _ := e.st.GetTask(g.TaskID)
		body += fmt.Sprintf(" Task %s is now %s.", t.ID, t.Status)
	}
	e.messageCoordinatorLocked(g.RunID, store.KindNote, "gate "+g.ID+" resolved: "+g.Decision, body, g.TaskID, "")
	if len(ready) > 0 {
		e.autoDispatchLocked(g.RunID)
	}
	e.notify.broadcast()
	return g, nil
}

type GateListArgs struct {
	Run string `json:"run,omitempty"`
	All bool   `json:"all,omitempty"`
}

func (e *Engine) opGateList(_ context.Context, _ string, raw json.RawMessage) (any, error) {
	a, err := decode[GateListArgs](raw)
	if err != nil {
		return nil, err
	}
	return e.st.ListGates(a.Run, !a.All)
}

// ---------- board ----------

type Board struct {
	Runs     []store.Run       `json:"runs"`
	Selected *RunShow          `json:"selected,omitempty"`
	Pending  []store.Gate      `json:"pending_gates"`
	Messages []store.Message   `json:"messages"`
	Status   map[string]string `json:"status"`
}

func (e *Engine) opBoard(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[RunRef](raw)
	if err != nil {
		return nil, err
	}
	b := Board{}
	if b.Runs, err = e.st.ListRuns(); err != nil {
		return nil, err
	}
	var run store.Run
	if a.Run != "" {
		run, err = e.st.GetRun(a.Run)
	} else {
		run, err = e.st.CurrentRun(caller)
	}
	if err == nil {
		rs, err := e.runShow(run)
		if err != nil {
			return nil, err
		}
		b.Selected = &rs
		b.Messages, _ = e.st.RecentMessages(run.ID, 20)
	} else {
		b.Messages, _ = e.st.RecentMessages("", 20)
	}
	b.Pending, _ = e.st.ListGates("", true)
	e.mu.Lock()
	b.Status = map[string]string{}
	for k, v := range e.status {
		b.Status[k] = v
	}
	e.mu.Unlock()
	return b, nil
}
