package store

import (
	"database/sql"
)

type Worker struct {
	PaneID      string `json:"pane_id"`
	RunID       string `json:"run_id,omitempty"`
	Name        string `json:"name,omitempty"`
	Agent       string `json:"agent,omitempty"`
	Worktree    string `json:"worktree,omitempty"`
	State       string `json:"state"`
	Retained    bool   `json:"retained"`
	FlaggedAt   int64  `json:"flagged_at,omitempty"`
	StartedAt   int64  `json:"started_at"`
	ReleasedAt  int64  `json:"released_at,omitempty"`
	ArchivePath string `json:"archive_path,omitempty"`
	Note        string `json:"note,omitempty"`
}

const workerCols = `pane_id,COALESCE(run_id,''),COALESCE(name,''),COALESCE(agent,''),COALESCE(worktree,''),state,retained,COALESCE(flagged_at,0),started_at,COALESCE(released_at,0),COALESCE(archive_path,''),COALESCE(note,'')`

func scanWorker(r interface{ Scan(...any) error }) (Worker, error) {
	var w Worker
	err := r.Scan(&w.PaneID, &w.RunID, &w.Name, &w.Agent, &w.Worktree, &w.State, &w.Retained, &w.FlaggedAt, &w.StartedAt, &w.ReleasedAt, &w.ArchivePath, &w.Note)
	return w, err
}

// RegisterWorker records a live worker (insert or revive).
func (s *Store) RegisterWorker(w Worker) (Worker, error) {
	now := s.now()
	_, err := s.db.Exec(`INSERT INTO workers(pane_id,run_id,name,agent,worktree,state,started_at) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(pane_id) DO UPDATE SET run_id=excluded.run_id, name=excluded.name, agent=excluded.agent,
		worktree=excluded.worktree, state=excluded.state, released_at=NULL, flagged_at=NULL`,
		w.PaneID, nullStr(w.RunID), nullStr(w.Name), nullStr(w.Agent), nullStr(w.Worktree), WorkerLive, now)
	if err != nil {
		return Worker{}, err
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
	_, err := s.db.Exec(`UPDATE workers SET state=?, note=COALESCE(?,note), archive_path=COALESCE(?,archive_path), released_at=COALESCE(?,released_at) WHERE pane_id=?`,
		state, nullStr(note), nullStr(archive), released, pane)
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
