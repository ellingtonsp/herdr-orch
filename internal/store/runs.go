package store

import (
	"database/sql"
)

type Run struct {
	ID                string `json:"id"`
	Title             string `json:"title"`
	CoordinatorPaneID string `json:"coordinator_pane_id,omitempty"`
	Status            string `json:"status"`
	AutoDispatch      bool   `json:"auto_dispatch"`
	MaxAttempts       int    `json:"max_attempts"`
	// IdleReportMS overrides how long a dispatched worker may sit idle without reporting
	// before the coordinator is told (0 = daemon default).
	IdleReportMS int64 `json:"idle_report_ms,omitempty"`
	// IdleFlagMS > 0 flags live, unretained workers that sit that long with no dispatch.
	IdleFlagMS int64 `json:"idle_flag_ms,omitempty"`
	CreatedAt  int64 `json:"created_at"`
	UpdatedAt  int64 `json:"updated_at"`
}

const runCols = `id,title,COALESCE(coordinator_pane_id,''),status,auto_dispatch,max_attempts,idle_report_ms,idle_flag_ms,created_at,updated_at`

func scanRun(r interface{ Scan(...any) error }) (Run, error) {
	var x Run
	err := r.Scan(&x.ID, &x.Title, &x.CoordinatorPaneID, &x.Status, &x.AutoDispatch, &x.MaxAttempts, &x.IdleReportMS, &x.IdleFlagMS, &x.CreatedAt, &x.UpdatedAt)
	return x, err
}

// CreateRun creates a running run coordinated by coordinator (may be "") and binds the
// coordinator pane to it.
func (s *Store) CreateRun(title, coordinator string) (Run, error) {
	var run Run
	err := s.tx(func(tx *sql.Tx) error {
		id, err := nextID(tx, "r")
		if err != nil {
			return err
		}
		now := s.now()
		if _, err := tx.Exec(`INSERT INTO runs(id,title,coordinator_pane_id,status,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
			id, title, nullStr(coordinator), RunRunning, now, now); err != nil {
			return err
		}
		if coordinator != "" {
			if err := bindPane(tx, coordinator, id, now); err != nil {
				return err
			}
		}
		run, err = scanRun(tx.QueryRow(`SELECT `+runCols+` FROM runs WHERE id=?`, id))
		return err
	})
	return run, err
}

func bindPane(q querier, pane, run string, now int64) error {
	_, err := q.Exec(`INSERT INTO pane_runs(pane_id,run_id,updated_at) VALUES(?,?,?)
		ON CONFLICT(pane_id) DO UPDATE SET run_id=excluded.run_id, updated_at=excluded.updated_at`, pane, run, now)
	return err
}

func (s *Store) GetRun(id string) (Run, error) {
	r, err := scanRun(s.db.QueryRow(`SELECT `+runCols+` FROM runs WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return r, refuse("unknown_run", "no run %q", id)
	}
	return r, err
}

func (s *Store) ListRuns() ([]Run, error) {
	rows, err := s.db.Query(`SELECT ` + runCols + ` FROM runs ORDER BY created_at DESC, rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UseRun binds pane to run so later commands from that pane default to it.
func (s *Store) UseRun(pane, run string) error {
	if _, err := s.GetRun(run); err != nil {
		return err
	}
	return bindPane(s.db, pane, run, s.now())
}

// CurrentRun resolves the default run for pane: its binding, else the newest running run.
func (s *Store) CurrentRun(pane string) (Run, error) {
	var id string
	err := s.db.QueryRow(`SELECT run_id FROM pane_runs WHERE pane_id=?`, pane).Scan(&id)
	if err == nil {
		return s.GetRun(id)
	}
	if err != sql.ErrNoRows {
		return Run{}, err
	}
	r, err := scanRun(s.db.QueryRow(`SELECT ` + runCols + ` FROM runs WHERE status='running' ORDER BY created_at DESC, rowid DESC LIMIT 1`))
	if err == sql.ErrNoRows {
		return r, refuse("no_run", "no current run; create one with `horch run create --title ...`")
	}
	return r, err
}

type RunUpdate struct {
	Status       *string
	Coordinator  *string
	AutoDispatch *bool
	MaxAttempts  *int
	Title        *string
	IdleReportMS *int64
	IdleFlagMS   *int64
}

func (s *Store) UpdateRun(id string, u RunUpdate) (Run, error) {
	if _, err := s.GetRun(id); err != nil {
		return Run{}, err
	}
	now := s.now()
	err := s.tx(func(tx *sql.Tx) error {
		if u.Status != nil {
			switch *u.Status {
			case RunIdle, RunRunning, RunCompleted, RunFailed:
			default:
				return refuse("bad_status", "run status must be idle|running|completed|failed")
			}
			if _, err := tx.Exec(`UPDATE runs SET status=?,updated_at=? WHERE id=?`, *u.Status, now, id); err != nil {
				return err
			}
		}
		if u.Coordinator != nil {
			if _, err := tx.Exec(`UPDATE runs SET coordinator_pane_id=?,updated_at=? WHERE id=?`, nullStr(*u.Coordinator), now, id); err != nil {
				return err
			}
			if *u.Coordinator != "" {
				if err := bindPane(tx, *u.Coordinator, id, now); err != nil {
					return err
				}
			}
		}
		if u.AutoDispatch != nil {
			if _, err := tx.Exec(`UPDATE runs SET auto_dispatch=?,updated_at=? WHERE id=?`, *u.AutoDispatch, now, id); err != nil {
				return err
			}
		}
		if u.MaxAttempts != nil {
			if *u.MaxAttempts < 1 {
				return refuse("bad_value", "max attempts must be >= 1")
			}
			if _, err := tx.Exec(`UPDATE runs SET max_attempts=?,updated_at=? WHERE id=?`, *u.MaxAttempts, now, id); err != nil {
				return err
			}
		}
		if u.IdleReportMS != nil {
			if *u.IdleReportMS < 0 {
				return refuse("bad_value", "idle report delay must be >= 0")
			}
			if _, err := tx.Exec(`UPDATE runs SET idle_report_ms=?,updated_at=? WHERE id=?`, *u.IdleReportMS, now, id); err != nil {
				return err
			}
		}
		if u.IdleFlagMS != nil {
			if *u.IdleFlagMS < 0 {
				return refuse("bad_value", "idle flag delay must be >= 0")
			}
			if _, err := tx.Exec(`UPDATE runs SET idle_flag_ms=?,updated_at=? WHERE id=?`, *u.IdleFlagMS, now, id); err != nil {
				return err
			}
		}
		if u.Title != nil {
			if _, err := tx.Exec(`UPDATE runs SET title=?,updated_at=? WHERE id=?`, *u.Title, now, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Run{}, err
	}
	return s.GetRun(id)
}

// RunMembers returns the coordinator and the panes of live workers in the run.
func (s *Store) RunMembers(run string) ([]string, error) {
	r, err := s.GetRun(run)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	if r.CoordinatorPaneID != "" {
		seen[r.CoordinatorPaneID] = true
		out = append(out, r.CoordinatorPaneID)
	}
	rows, err := s.db.Query(`SELECT pane_id FROM workers WHERE run_id=? AND state=?`, run, WorkerLive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, rows.Err()
}
