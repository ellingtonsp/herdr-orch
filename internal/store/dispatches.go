package store

import (
	"database/sql"
)

type Dispatch struct {
	ID                  string `json:"id"`
	TaskID              string `json:"task_id"`
	RunID               string `json:"run_id"`
	PaneID              string `json:"pane_id"`
	Agent               string `json:"agent,omitempty"`
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

const dispCols = `id,task_id,run_id,pane_id,COALESCE(agent,''),COALESCE(prompt_hash,''),status,saw_working,COALESCE(done_message_id,''),COALESCE(done_outcome,''),COALESCE(idle_since,0),unobserved_escalated,idle_escalated,nudges,started_at,COALESCE(settled_at,0),COALESCE(outcome,'')`

func scanDisp(r interface{ Scan(...any) error }) (Dispatch, error) {
	var d Dispatch
	err := r.Scan(&d.ID, &d.TaskID, &d.RunID, &d.PaneID, &d.Agent, &d.PromptHash, &d.Status, &d.SawWorking, &d.DoneMessageID, &d.DoneOutcome, &d.IdleSince, &d.UnobservedEscalated, &d.IdleEscalated, &d.Nudges, &d.StartedAt, &d.SettledAt, &d.Outcome)
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
	return s.queryDisps(`SELECT `+dispCols+` FROM dispatches WHERE task_id=? ORDER BY started_at, id`, task)
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
	var id string
	err := s.tx(func(tx *sql.Tx) error {
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
		if _, err := tx.Exec(`INSERT INTO dispatches(id,task_id,run_id,pane_id,agent,prompt_hash,status,started_at) VALUES(?,?,?,?,?,?,?,?)`,
			id, taskID, t.RunID, pane, nullStr(agent), nullStr(promptHash), DispatchPending, now); err != nil {
			return err
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
