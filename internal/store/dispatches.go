package store

import (
	"database/sql"
	"time"
)

type Dispatch struct {
	UsageCapUntil       int64  `json:"usage_cap_until,omitempty"`
	UsageCapUnknown     bool   `json:"usage_cap_unknown,omitempty"`
	HoldUntil           int64  `json:"hold_until,omitempty"`
	HoldEvent           string `json:"hold_event,omitempty"`
	HoldReason          string `json:"hold_reason,omitempty"`
	HeldBy              string `json:"held_by,omitempty"`
	MonitorAfter        int64  `json:"monitor_after,omitempty"`
	UsageCapNotice      string `json:"usage_cap_notice,omitempty"`
	ID                  string `json:"id"`
	TaskID              string `json:"task_id"`
	RunID               string `json:"run_id"`
	PaneID              string `json:"pane_id"`
	Agent               string `json:"agent,omitempty"`
	Account             string `json:"account,omitempty"`
	Profile             string `json:"profile,omitempty"`
	Model               string `json:"model,omitempty"`
	PromptHash          string `json:"prompt_hash,omitempty"`
	Status              string `json:"status"`
	SawWorking          bool   `json:"saw_working"`
	DoneMessageID       string `json:"done_message_id,omitempty"`
	DoneOutcome         string `json:"done_outcome,omitempty"`
	IdleSince           int64  `json:"idle_since,omitempty"`
	UnobservedEscalated bool   `json:"unobserved_escalated,omitempty"`
	IdleEscalated       bool   `json:"idle_escalated,omitempty"`
	Nudges              int    `json:"nudges,omitempty"`
	StartedAt           int64  `json:"started_at"`
	SettledAt           int64  `json:"settled_at,omitempty"`
	Outcome             string `json:"outcome,omitempty"`
}

// Active reports whether the dispatch has not settled.
func (d Dispatch) Active() bool {
	return d.Status == DispatchPending || d.Status == DispatchDispatched
}

const dispCols = `id,task_id,run_id,pane_id,COALESCE(agent,''),COALESCE(prompt_hash,''),status,saw_working,COALESCE(done_message_id,''),COALESCE(done_outcome,''),COALESCE(idle_since,0),unobserved_escalated,idle_escalated,nudges,started_at,COALESCE(settled_at,0),COALESCE(outcome,''),hold_until,hold_event,hold_reason,held_by,monitor_after,usage_cap_notice,usage_cap_until,usage_cap_unknown,COALESCE(account,''),COALESCE(profile,''),COALESCE(model,'')`

func scanDisp(r interface{ Scan(...any) error }) (Dispatch, error) {
	var d Dispatch
	err := r.Scan(&d.ID, &d.TaskID, &d.RunID, &d.PaneID, &d.Agent, &d.PromptHash, &d.Status, &d.SawWorking, &d.DoneMessageID, &d.DoneOutcome, &d.IdleSince, &d.UnobservedEscalated, &d.IdleEscalated, &d.Nudges, &d.StartedAt, &d.SettledAt, &d.Outcome, &d.HoldUntil, &d.HoldEvent, &d.HoldReason, &d.HeldBy, &d.MonitorAfter, &d.UsageCapNotice, &d.UsageCapUntil, &d.UsageCapUnknown, &d.Account, &d.Profile, &d.Model)
	return d, err
}

func (s *Store) queryDisps(q string, args ...any) ([]Dispatch, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Dispatch{}
	for rows.Next() {
		d, err := scanDisp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) GetDispatch(id string) (Dispatch, error) {
	d, err := scanDisp(s.db.QueryRow(`SELECT `+dispCols+` FROM dispatches WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return d, refuse("unknown_dispatch", "no dispatch %q", id)
	}
	return d, err
}

// DispatchesForTask lists every dispatch of a task, oldest first.
func (s *Store) DispatchesForTask(task string) ([]Dispatch, error) {
	return s.queryDisps(`SELECT `+dispCols+` FROM dispatches WHERE task_id=? ORDER BY started_at, CAST(SUBSTR(id,2) AS INTEGER)`, task)
}

// ActiveDispatches lists unsettled dispatches.
func (s *Store) ActiveDispatches() ([]Dispatch, error) {
	return s.queryDisps(`SELECT `+dispCols+` FROM dispatches WHERE status IN (?,?) ORDER BY started_at`, DispatchPending, DispatchDispatched)
}

// ActiveDispatchForPane returns the pane's unsettled dispatch, or ErrNotFound.
func (s *Store) ActiveDispatchForPane(pane string) (Dispatch, error) {
	d, err := scanDisp(s.db.QueryRow(`SELECT `+dispCols+` FROM dispatches WHERE pane_id=? AND status IN (?,?) LIMIT 1`, pane, DispatchPending, DispatchDispatched))
	if err == sql.ErrNoRows {
		return d, ErrNotFound
	}
	return d, err
}

// BeginDispatch claims task for pane: task ready → dispatched, attempts+1, new dispatch in
// `pending`. Refuses when the task is not ready, already has an active dispatch, the pane
// is busy with another dispatch, or the circuit breaker is open.
func (s *Store) BeginDispatch(taskID, pane, agent, promptHash string) (Dispatch, error) {
	return s.BeginDispatchWithLeases(taskID, pane, agent, promptHash, nil, DefaultLeaseTTL)
}

func (s *Store) BeginDispatchWithLeases(taskID, pane, agent, promptHash string, resources []string, ttl time.Duration) (Dispatch, error) {
	normalized := []string{}
	for _, resource := range resources {
		r, err := NormalizeResource(resource)
		if err != nil {
			return Dispatch{}, err
		}
		normalized = append(normalized, r)
	}
	now := s.now()
	expires, err := leaseExpiry(now, ttl)
	if err != nil {
		return Dispatch{}, err
	}
	var id string
	err = s.tx(func(tx *sql.Tx) error {
		if len(normalized) > 0 {
			if err := expireLeases(tx, now); err != nil {
				return err
			}
			if err := leaseWorker(tx, pane); err != nil {
				return err
			}
		}
		var phase string
		err := tx.QueryRow(`SELECT lifecycle FROM workers WHERE pane_id=?`, pane).Scan(&phase)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if phase != "" {
			return refuse("worker_lifecycle_busy", "worker %s is held for a lifecycle operation", pane)
		}
		t, err := getTask(tx, taskID)
		if err != nil {
			return err
		}
		r, err := scanRun(tx.QueryRow(`SELECT `+runCols+` FROM runs WHERE id=?`, t.RunID))
		if err != nil {
			return err
		}
		if t.Status != TaskReady {
			return refuse("task_not_ready", "task %s is %s, not ready", taskID, t.Status)
		}
		if t.Attempts >= r.MaxAttempts {
			return refuse("circuit_broken", "task %s already failed %d/%d attempts; `horch task update --id %s --reset-attempts` to retry", taskID, t.Attempts, r.MaxAttempts, taskID)
		}
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM dispatches WHERE pane_id=? AND status IN (?,?)`, pane, DispatchPending, DispatchDispatched).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return refuse("pane_busy", "pane %s already has an active dispatch", pane)
		}
		id, err = nextID(tx, "d")
		if err != nil {
			return err
		}
		now := s.now()
		if _, err := tx.Exec(`INSERT INTO dispatches(id,task_id,run_id,pane_id,agent,prompt_hash,status,started_at,account,profile,model)
			VALUES(?,?,?,?,?,?,?,?,(SELECT account FROM workers WHERE pane_id=?),(SELECT profile FROM workers WHERE pane_id=?),(SELECT model FROM workers WHERE pane_id=?))`,
			id, taskID, t.RunID, pane, nullStr(agent), nullStr(promptHash), DispatchPending, now, pane, pane, pane); err != nil {
			return err
		}
		for _, resource := range normalized {
			if err := acquireLease(tx, resource, pane, s.leaseScope, id, now, expires, false); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`UPDATE tasks SET status=?, assignee_pane_id=?, attempts=attempts+1, updated_at=? WHERE id=?`, TaskDispatched, pane, now, taskID)
		if err != nil {
			return err
		}
		return bindPane(tx, pane, t.RunID, now)
	})
	if err != nil {
		return Dispatch{}, err
	}
	return s.GetDispatch(id)
}

// MarkSent moves a pending dispatch to dispatched once the prompt was submitted.
func (s *Store) MarkSent(id, note string) error {
	_, err := s.db.Exec(`UPDATE dispatches SET status=?, outcome=? WHERE id=? AND status=?`, DispatchDispatched, nullStr(note), id, DispatchPending)
	return err
}

// ObserveStatus records an agent status for an active dispatch: working/blocked sets
// saw_working and starts a new idle episode (idle_since and idle_escalated cleared);
// idle/done sets idle_since (if unset).
func (s *Store) ObserveStatus(id, status string) (Dispatch, error) {
	now := s.now()
	var err error
	switch status {
	case "working", "blocked":
		_, err = s.db.Exec(`UPDATE dispatches SET saw_working=1, idle_since=NULL, idle_escalated=0 WHERE id=?`, id)
	case "idle", "done":
		_, err = s.db.Exec(`UPDATE dispatches SET idle_since=COALESCE(idle_since, ?) WHERE id=?`, now, id)
	}
	if err != nil {
		return Dispatch{}, err
	}
	return s.GetDispatch(id)
}

// RecordDone attaches the worker's done message to its dispatch.
func (s *Store) RecordDone(id, messageID, outcome string) (Dispatch, error) {
	if _, err := s.db.Exec(`UPDATE dispatches SET done_message_id=?, done_outcome=? WHERE id=?`, messageID, outcome, id); err != nil {
		return Dispatch{}, err
	}
	return s.GetDispatch(id)
}

// CompletionReport records a worker report and the dispatch it belongs to. Late
// reports are notes and never change the closed dispatch or its task.
type CompletionReport struct {
	Message  Message  `json:"message"`
	Dispatch Dispatch `json:"dispatch"`
	Late     bool     `json:"late"`
}

// RecordDispatchReport persists the message and association atomically, enforcing
// the latest dispatch's assignee even for callers that are not pane-shaped IDs.
func (s *Store) RecordDispatchReport(id string, m Message) (CompletionReport, error) {
	var report CompletionReport
	err := s.tx(func(tx *sql.Tx) error {
		d, err := scanDisp(tx.QueryRow(`SELECT `+dispCols+` FROM dispatches WHERE id=?`, id))
		if err == sql.ErrNoRows {
			return refuse("unknown_dispatch", "no dispatch %q", id)
		} else if err != nil {
			return err
		}
		var latest string
		if err := tx.QueryRow(`SELECT id FROM dispatches WHERE task_id=? ORDER BY started_at DESC, CAST(SUBSTR(id,2) AS INTEGER) DESC LIMIT 1`, d.TaskID).Scan(&latest); err != nil {
			return err
		}
		if latest != id {
			return refuse("not_latest_dispatch", "dispatch %s is no longer the latest dispatch of task %s", id, d.TaskID)
		}
		if m.From != d.PaneID {
			return refuse("not_assignee", "task %s is dispatched to %s, not %s", d.TaskID, d.PaneID, m.From)
		}
		report.Late = !d.Active()
		if !report.Late && d.DoneMessageID != "" {
			return refuse("already_done", "dispatch %s already has a done report (%s)", d.ID, d.DoneMessageID)
		}
		m.RunID, m.TaskID, m.To, m.Kind = d.RunID, d.TaskID, Daemon, KindDone
		if report.Late {
			m.Kind = KindNote
		}
		m.ID, err = insertMessage(tx, m, s.now())
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO dispatch_reports(dispatch_id,message_id,late) VALUES(?,?,?)`, id, m.ID, report.Late); err != nil {
			return err
		}
		if !report.Late {
			if _, err := tx.Exec(`UPDATE dispatches SET done_message_id=?, done_outcome=? WHERE id=?`, m.ID, m.Outcome, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return report, err
	}
	report.Message, err = s.GetMessage(m.ID)
	if err != nil {
		return report, err
	}
	report.Dispatch, err = s.GetDispatch(id)
	return report, err
}

// DispatchReports returns every persisted worker report, including late notes.
func (s *Store) DispatchReports(id string) ([]Message, error) {
	return s.queryMsgs(`SELECT `+msgCols+` FROM messages WHERE id IN (SELECT message_id FROM dispatch_reports WHERE dispatch_id=?) ORDER BY created_at, CAST(SUBSTR(id,2) AS INTEGER)`, id)
}

func (s *Store) MarkIdleEscalated(id string) error {
	_, err := s.db.Exec(`UPDATE dispatches SET idle_escalated=1 WHERE id=?`, id)
	return err
}

// AddNudge counts one coordinator reminder sent to the worker and returns the new total.
func (s *Store) AddNudge(id string) (int, error) {
	if _, err := s.db.Exec(`UPDATE dispatches SET nudges=nudges+1 WHERE id=?`, id); err != nil {
		return 0, err
	}
	var n int
	err := s.db.QueryRow(`SELECT nudges FROM dispatches WHERE id=?`, id).Scan(&n)
	return n, err
}

func (s *Store) MarkUnobservedEscalated(id string) error {
	_, err := s.db.Exec(`UPDATE dispatches SET unobserved_escalated=1 WHERE id=?`, id)
	return err
}

// Settlement is the result of settling a dispatch.
type Settlement struct {
	Dispatch Dispatch `json:"dispatch"`
	Task     Task     `json:"task"`
	NowReady []string `json:"now_ready,omitempty"`
}

// Settle closes an active dispatch. success → dispatch and task completed (result stored),
// dependents re-evaluated. failure → dispatch failed and task back to ready, or, once the
// task has used max_attempts, dispatch circuit_broken and task failed.
func (s *Store) Settle(id string, success bool, outcome, result string) (Settlement, error) {
	var st Settlement
	err := s.tx(func(tx *sql.Tx) error {
		d, err := scanDisp(tx.QueryRow(`SELECT `+dispCols+` FROM dispatches WHERE id=?`, id))
		if err == sql.ErrNoRows {
			return refuse("unknown_dispatch", "no dispatch %q", id)
		} else if err != nil {
			return err
		}
		if !d.Active() {
			return refuse("dispatch_settled", "dispatch %s already %s", id, d.Status)
		}
		t, err := getTask(tx, d.TaskID)
		if err != nil {
			return err
		}
		r, err := scanRun(tx.QueryRow(`SELECT `+runCols+` FROM runs WHERE id=?`, t.RunID))
		if err != nil {
			return err
		}
		now := s.now()
		dstatus, tstatus := DispatchCompleted, TaskCompleted
		if !success {
			dstatus, tstatus = DispatchFailed, TaskReady
			if t.Attempts >= r.MaxAttempts {
				dstatus, tstatus = DispatchCircuitBroken, TaskFailed
			}
		}
		if _, err := tx.Exec(`UPDATE dispatches SET status=?, settled_at=?, outcome=? WHERE id=?`, dstatus, now, nullStr(outcome), id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE tasks SET status=?, result=COALESCE(?,result), updated_at=? WHERE id=?`, tstatus, nullStr(result), now, d.TaskID); err != nil {
			return err
		}
		if tstatus == TaskReady {
			// Back to ready only if deps/gates still allow it.
			if _, err := tx.Exec(`UPDATE tasks SET status=? WHERE id=?`, TaskPending, d.TaskID); err != nil {
				return err
			}
		}
		st.NowReady, err = refreshReadiness(tx, t.RunID, now)
		return err
	})
	if err != nil {
		return st, err
	}
	st.Dispatch, err = s.GetDispatch(id)
	if err != nil {
		return st, err
	}
	st.Task, err = s.GetTask(st.Dispatch.TaskID)
	return st, err
}

// Fence settles an active dispatch as failed without counting the attempt (the worker was
// stopped or abandoned, not at fault). The task returns to ready.
func (s *Store) Fence(id, reason string) (Settlement, error) {
	return s.fence(id, reason, false)
}

// FenceUnsent rolls back only resources first claimed by a prompt that was not delivered.
// Worker stop/release fences must retain resources until process cleanup is verified.
func (s *Store) FenceUnsent(id, reason string) (Settlement, error) {
	return s.fence(id, reason, true)
}
func (s *Store) fence(id, reason string, releaseUnsent bool) (Settlement, error) {
	var st Settlement
	err := s.tx(func(tx *sql.Tx) error {
		d, err := scanDisp(tx.QueryRow(`SELECT `+dispCols+` FROM dispatches WHERE id=?`, id))
		if err != nil {
			return err
		}
		if !d.Active() {
			return refuse("dispatch_settled", "dispatch %s already %s", id, d.Status)
		}
		now := s.now()
		if _, err := tx.Exec(`UPDATE dispatches SET status=?, settled_at=?, outcome=? WHERE id=?`, DispatchFailed, now, "fenced: "+reason, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE tasks SET status=?, attempts=MAX(attempts-1,0), updated_at=? WHERE id=?`, TaskPending, now, d.TaskID); err != nil {
			return err
		}
		t, err := getTask(tx, d.TaskID)
		if err != nil {
			return err
		}
		if releaseUnsent {
			if d.Status != DispatchPending {
				return refuse("dispatch_sent", "dispatch %s was already submitted; leases must be retained", id)
			}
			if _, err := tx.Exec(`DELETE FROM lease_registry.leases WHERE dispatch_id=? AND scope=?`, id, s.leaseScope); err != nil {
				return err
			}
		}
		st.NowReady, err = refreshReadiness(tx, t.RunID, now)
		return err
	})
	if err != nil {
		return st, err
	}
	st.Dispatch, _ = s.GetDispatch(id)
	st.Task, err = s.GetTask(st.Dispatch.TaskID)
	return st, err
}

// LastSettledForPane returns when the pane's most recent dispatch settled (0 if never).
func (s *Store) LastSettledForPane(pane string) (int64, error) {
	var t int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(settled_at),0) FROM dispatches WHERE pane_id=?`, pane).Scan(&t)
	return t, err
}

// Held reports an unexpired timed hold or an explicitly resumed event hold.
func (d Dispatch) Held(now int64) bool {
	return d.HoldEvent != "" || d.HoldUntil > now
}

// HoldDispatch preserves the active claim while suppressing supervision reminders.
func (s *Store) HoldDispatch(id string, until int64, event, reason, actor string) (Dispatch, error) {
	if (until <= s.now() && event == "") || (until != 0 && event != "") || reason == "" {
		return Dispatch{}, refuse("bad_args", "hold requires a future deadline or event, and a reason")
	}
	res, err := s.db.Exec(`UPDATE dispatches SET hold_until=?,hold_event=?,hold_reason=?,held_by=? WHERE id=? AND status IN (?,?)`, until, event, reason, actor, id, DispatchPending, DispatchDispatched)
	if err != nil {
		return Dispatch{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Dispatch{}, refuse("no_active_dispatch", "dispatch %s is not active", id)
	}
	return s.GetDispatch(id)
}

// ResumeDispatch starts a fresh supervision grace period, without changing attempt history.
func (s *Store) ResumeDispatch(id string) (Dispatch, error) {
	now := s.now()
	res, err := s.db.Exec(`UPDATE dispatches SET hold_until=0,hold_event='',hold_reason='',held_by='',usage_cap_until=0,usage_cap_unknown=0,idle_since=?,idle_escalated=0,unobserved_escalated=0,monitor_after=? WHERE id=? AND status IN (?,?)`, now, now, id, DispatchPending, DispatchDispatched)
	if err != nil {
		return Dispatch{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Dispatch{}, refuse("no_active_dispatch", "dispatch %s is not active", id)
	}
	return s.GetDispatch(id)
}

func (s *Store) SetUsageCapNotice(id, notice string) error {
	_, err := s.db.Exec(`UPDATE dispatches SET usage_cap_notice=? WHERE id=?`, notice, id)
	return err
}

// RecordUsageCap commits the cap, automatic hold and its single escalation together.
// An existing coordinator hold is retained; cap metadata survives its earlier expiry.
func (s *Store) RecordUsageCap(id, notice string, until int64, unknown bool, m Message) (Dispatch, error) {
	err := s.tx(func(tx *sql.Tx) error {
		d, err := scanDisp(tx.QueryRow(`SELECT `+dispCols+` FROM dispatches WHERE id=?`, id))
		if err != nil {
			return err
		}
		if !d.Active() {
			return refuse("no_active_dispatch", "dispatch is not active")
		}
		if d.UsageCapNotice == notice {
			return nil
		}
		if _, err := tx.Exec(`UPDATE dispatches SET usage_cap_notice=?,usage_cap_until=?,usage_cap_unknown=? WHERE id=?`, notice, until, unknown, id); err != nil {
			return err
		}
		if !d.Held(s.now()) {
			event := ""
			if unknown {
				event = "usage-reset"
			}
			if _, err := tx.Exec(`UPDATE dispatches SET hold_until=?,hold_event=?,hold_reason='agent usage cap',held_by=? WHERE id=?`, until, event, Daemon, id); err != nil {
				return err
			}
		}
		_, err = insertMessage(tx, m, s.now())
		return err
	})
	if err != nil {
		return Dispatch{}, err
	}
	return s.GetDispatch(id)
}
