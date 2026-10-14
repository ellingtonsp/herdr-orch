package store

import (
	"database/sql"
	"encoding/json"
)

// WorkerLifecycleMigration preserves launch configuration and responsibility transfers.
// NULL agent_args means a worker was adopted without a known launch configuration.
const WorkerLifecycleMigration = `ALTER TABLE workers ADD COLUMN agent_args TEXT;
ALTER TABLE workers ADD COLUMN lifecycle TEXT NOT NULL DEFAULT '';
ALTER TABLE workers ADD COLUMN hand_off_to TEXT;
ALTER TABLE workers ADD COLUMN hand_off_at INTEGER;
ALTER TABLE workers ADD COLUMN hand_off_worktree TEXT;`

type Worker struct {
	PaneID          string   `json:"pane_id"`
	RunID           string   `json:"run_id,omitempty"`
	Name            string   `json:"name,omitempty"`
	Agent           string   `json:"agent,omitempty"`
	Account         string   `json:"account,omitempty"`
	Profile         string   `json:"profile,omitempty"`
	Model           string   `json:"model,omitempty"`
	Worktree        string   `json:"worktree,omitempty"`
	State           string   `json:"state"`
	Retained        bool     `json:"retained"`
	FlaggedAt       int64    `json:"flagged_at,omitempty"`
	StartedAt       int64    `json:"started_at"`
	ReleasedAt      int64    `json:"released_at,omitempty"`
	ArchivePath     string   `json:"archive_path,omitempty"`
	Note            string   `json:"note,omitempty"`
	AgentArgs       []string `json:"-"`
	LaunchKnown     bool     `json:"launch_known"`
	Lifecycle       string   `json:"lifecycle,omitempty"`
	HandOffTo       string   `json:"hand_off_to,omitempty"`
	HandOffAt       int64    `json:"hand_off_at,omitempty"`
	HandOffWorktree string   `json:"hand_off_worktree,omitempty"`
}

const workerCols = `pane_id,COALESCE(run_id,''),COALESCE(name,''),COALESCE(agent,''),COALESCE(worktree,''),state,retained,COALESCE(flagged_at,0),started_at,COALESCE(released_at,0),COALESCE(archive_path,''),COALESCE(note,''),agent_args,lifecycle,COALESCE(hand_off_to,''),COALESCE(hand_off_at,0),COALESCE(hand_off_worktree,''),COALESCE(account,''),COALESCE(profile,''),COALESCE(model,'')`

func scanWorker(r interface{ Scan(...any) error }) (Worker, error) {
	var w Worker
	var args sql.NullString
	err := r.Scan(&w.PaneID, &w.RunID, &w.Name, &w.Agent, &w.Worktree, &w.State, &w.Retained, &w.FlaggedAt, &w.StartedAt, &w.ReleasedAt, &w.ArchivePath, &w.Note, &args, &w.Lifecycle, &w.HandOffTo, &w.HandOffAt, &w.HandOffWorktree, &w.Account, &w.Profile, &w.Model)
	if err == nil && args.Valid {
		w.LaunchKnown = true
		err = json.Unmarshal([]byte(args.String), &w.AgentArgs)
	}
	return w, err
}

// RegisterWorker records a live worker (insert or revive).
func (s *Store) RegisterWorker(w Worker) (Worker, error) {
	now := s.now()
	var args any
	if w.LaunchKnown {
		a := w.AgentArgs
		if a == nil {
			a = []string{}
		}
		b, err := json.Marshal(a)
		if err != nil {
			return Worker{}, err
		}
		args = string(b)
	}
	res, err := s.db.Exec(`INSERT INTO workers(pane_id,run_id,name,agent,worktree,state,started_at,agent_args,account,profile,model) VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(pane_id) DO UPDATE SET run_id=excluded.run_id, name=excluded.name, agent=excluded.agent,
		worktree=excluded.worktree, state=excluded.state, released_at=NULL, flagged_at=NULL,
		agent_args=excluded.agent_args, account=excluded.account, profile=excluded.profile, model=excluded.model WHERE workers.lifecycle=''`,
		w.PaneID, nullStr(w.RunID), nullStr(w.Name), nullStr(w.Agent), nullStr(w.Worktree), WorkerLive, now, args, nullStr(w.Account), nullStr(w.Profile), nullStr(w.Model))
	if err != nil {
		return Worker{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Worker{}, refuse("worker_lifecycle_busy", "worker %s is held for a lifecycle operation", w.PaneID)
	}
	if w.RunID != "" {
		if err := bindPane(s.db, w.PaneID, w.RunID, now); err != nil {
			return Worker{}, err
		}
	}
	return s.GetWorker(w.PaneID)
}

func (s *Store) GetWorker(pane string) (Worker, error) {
	w, err := scanWorker(s.db.QueryRow(`SELECT `+workerCols+` FROM workers WHERE pane_id=?`, pane))
	if err == sql.ErrNoRows {
		return w, refuse("unknown_worker", "pane %s is not a registered worker", pane)
	}
	return w, err
}

// FindWorker resolves a pane id or worker name.
func (s *Store) FindWorker(ref string) (Worker, error) {
	w, err := s.GetWorker(ref)
	if err == nil {
		return w, nil
	}
	w2, err2 := scanWorker(s.db.QueryRow(`SELECT `+workerCols+` FROM workers WHERE name=? ORDER BY (state='live') DESC, started_at DESC LIMIT 1`, ref))
	if err2 == sql.ErrNoRows {
		return w, err
	}
	return w2, err2
}

// ListWorkers lists workers in run ("" = all); liveOnly limits to live/release_failed.
func (s *Store) ListWorkers(run string, liveOnly bool) ([]Worker, error) {
	q := `SELECT ` + workerCols + ` FROM workers WHERE 1=1`
	var args []any
	if run != "" {
		q += ` AND run_id=?`
		args = append(args, run)
	}
	if liveOnly {
		q += ` AND state IN ('live','release_failed')`
	}
	rows, err := s.db.Query(q+` ORDER BY started_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Worker{}
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SetWorkerState moves a worker to state, stamping released_at for terminal states.
func (s *Store) SetWorkerState(pane, state, note, archive string) error {
	now := s.now()
	var released any
	switch state {
	case WorkerReleased, WorkerStopped, WorkerAbandoned, WorkerExited:
		released = now
	}
	_, err := s.db.Exec(`UPDATE workers SET state=?, note=COALESCE(?,note), archive_path=COALESCE(?,archive_path), released_at=COALESCE(?,released_at), lifecycle=CASE WHEN ? IS NOT NULL THEN '' ELSE lifecycle END WHERE pane_id=?`,
		state, nullStr(note), nullStr(archive), released, released, pane)
	return err
}

func (s *Store) SetRetained(pane string, retained bool) error {
	res, err := s.db.Exec(`UPDATE workers SET retained=? WHERE pane_id=?`, retained, pane)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return refuse("unknown_worker", "pane %s is not a registered worker", pane)
	}
	return nil
}

func (s *Store) SetFlagged(pane string, at int64) error {
	var v any
	if at > 0 {
		v = at
	}
	_, err := s.db.Exec(`UPDATE workers SET flagged_at=? WHERE pane_id=?`, v, pane)
	return err
}

// WorkerLifecycleHeld fails closed if lifecycle state cannot be read.
func (s *Store) WorkerLifecycleHeld(pane string) bool {
	var phase string
	err := s.db.QueryRow(`SELECT lifecycle FROM workers WHERE pane_id=?`, pane).Scan(&phase)
	return err != sql.ErrNoRows && (err != nil || phase != "")
}

func (s *Store) BeginWorkerLifecycle(pane, phase string) error {
	res, err := s.db.Exec(`UPDATE workers SET lifecycle=? WHERE pane_id=? AND state IN ('live','release_failed') AND (lifecycle='' OR (lifecycle='restart_failed' AND ? IN ('restart','stopping','abandoning')) OR (lifecycle='release_interrupted' AND ? IN ('releasing','stopping','abandoning')))`, phase, pane, phase, phase)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return refuse("worker_lifecycle_busy", "worker %s has another lifecycle operation or is not live", pane)
	}
	return nil
}

func (s *Store) SetWorkerLifecycle(pane, phase string) error {
	_, err := s.db.Exec(`UPDATE workers SET lifecycle=? WHERE pane_id=?`, phase, pane)
	return err
}

// ReplaceWorkerDispatch starts a new dispatch history entry without exposing the task
// to auto-dispatch or consuming an additional task attempt.
func (s *Store) ReplaceWorkerDispatch(oldID string) (Dispatch, error) {
	var id string
	err := s.tx(func(tx *sql.Tx) error {
		d, err := scanDisp(tx.QueryRow(`SELECT `+dispCols+` FROM dispatches WHERE id=?`, oldID))
		if err != nil {
			return err
		}
		if !d.Active() {
			return refuse("dispatch_settled", "dispatch %s already settled", oldID)
		}
		var phase string
		if err := tx.QueryRow(`SELECT lifecycle FROM workers WHERE pane_id=?`, d.PaneID).Scan(&phase); err != nil {
			return err
		}
		if phase != "restart" {
			return refuse("worker_lifecycle_busy", "worker is not held for restart")
		}
		id, err = nextID(tx, "d")
		if err != nil {
			return err
		}
		now := s.now()
		if _, err := tx.Exec(`UPDATE dispatches SET status=?,settled_at=?,outcome='fenced: worker restart' WHERE id=?`, DispatchFailed, now, oldID); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO dispatches(id,task_id,run_id,pane_id,agent,prompt_hash,status,started_at,account,profile,model) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, d.TaskID, d.RunID, d.PaneID, nullStr(d.Agent), nullStr(d.PromptHash), DispatchPending, now, nullStr(d.Account), nullStr(d.Profile), nullStr(d.Model))
		return err
	})
	if err != nil {
		return Dispatch{}, err
	}
	return s.GetDispatch(id)
}

func (s *Store) RecordWorkerHandOff(pane, target, root string) error {
	res, err := s.db.Exec(`UPDATE workers SET hand_off_to=?,hand_off_at=?,hand_off_worktree=? WHERE pane_id=? AND lifecycle='releasing'`, target, s.now(), root, pane)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return refuse("worker_lifecycle_busy", "worker is not held for release")
	}
	return nil
}

// RecoverWorkerLifecycles runs once when the daemon acquires ownership. Interrupted
// operations require an explicit retry; recipients themselves remain safe to supervise.
func (s *Store) RecoverWorkerLifecycles() error {
	_, err := s.db.Exec(`UPDATE workers SET lifecycle=CASE lifecycle WHEN 'restart' THEN 'restart_failed' WHEN 'releasing' THEN 'release_interrupted' WHEN 'receiving_hand_off' THEN '' WHEN 'stopping' THEN 'release_interrupted' WHEN 'abandoning' THEN 'release_interrupted' ELSE lifecycle END WHERE lifecycle<>''`)
	return err
}
