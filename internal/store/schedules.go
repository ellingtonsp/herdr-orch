package store

import (
	"database/sql"
	"encoding/json"
)

type Schedule struct {
	ID        string          `json:"id"`
	Cron      string          `json:"cron"`
	Action    json.RawMessage `json:"action"`
	Enabled   bool            `json:"enabled"`
	LastRunAt int64           `json:"last_run_at,omitempty"`
	NextRunAt int64           `json:"next_run_at,omitempty"`
	CreatedAt int64           `json:"created_at"`
}

type ScheduleRun struct {
	ID         int64  `json:"id"`
	ScheduleID string `json:"schedule_id"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at,omitempty"`
	OK         bool   `json:"ok"`
	Output     string `json:"output,omitempty"`
}

const schedCols = `id,cron,action,enabled,COALESCE(last_run_at,0),COALESCE(next_run_at,0),created_at`

func scanSched(r interface{ Scan(...any) error }) (Schedule, error) {
	var x Schedule
	var action string
	err := r.Scan(&x.ID, &x.Cron, &action, &x.Enabled, &x.LastRunAt, &x.NextRunAt, &x.CreatedAt)
	x.Action = json.RawMessage(action)
	return x, err
}

func (s *Store) AddSchedule(cron string, action json.RawMessage, next int64) (Schedule, error) {
	var id string
	err := s.tx(func(tx *sql.Tx) error {
		var err error
		id, err = nextID(tx, "s")
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO schedules(id,cron,action,enabled,next_run_at,created_at) VALUES(?,?,?,1,?,?)`, id, cron, string(action), next, s.now())
		return err
	})
	if err != nil {
		return Schedule{}, err
	}
	return s.GetSchedule(id)
}

func (s *Store) GetSchedule(id string) (Schedule, error) {
	x, err := scanSched(s.db.QueryRow(`SELECT `+schedCols+` FROM schedules WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return x, refuse("unknown_schedule", "no schedule %q", id)
	}
	return x, err
}

func (s *Store) ListSchedules() ([]Schedule, error) {
	rows, err := s.db.Query(`SELECT ` + schedCols + ` FROM schedules ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Schedule{}
	for rows.Next() {
		x, err := scanSched(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) RemoveSchedule(id string) error {
	res, err := s.db.Exec(`DELETE FROM schedules WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return refuse("unknown_schedule", "no schedule %q", id)
	}
	return nil
}

func (s *Store) SetScheduleEnabled(id string, on bool, next int64) error {
	_, err := s.db.Exec(`UPDATE schedules SET enabled=?, next_run_at=? WHERE id=?`, on, next, id)
	return err
}

// DueSchedules lists enabled schedules whose next run is at or before now.
func (s *Store) DueSchedules() ([]Schedule, error) {
	rows, err := s.db.Query(`SELECT `+schedCols+` FROM schedules WHERE enabled=1 AND next_run_at IS NOT NULL AND next_run_at<=?`, s.now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Schedule{}
	for rows.Next() {
		x, err := scanSched(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// StartScheduleRun stamps last_run_at/next_run_at and opens a history row.
func (s *Store) StartScheduleRun(id string, next int64) (int64, error) {
	now := s.now()
	if _, err := s.db.Exec(`UPDATE schedules SET last_run_at=?, next_run_at=? WHERE id=?`, now, next, id); err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`INSERT INTO schedule_runs(schedule_id,started_at) VALUES(?,?)`, id, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishScheduleRun(runID int64, ok bool, output string) error {
	if len(output) > 8000 {
		output = output[:8000]
	}
	_, err := s.db.Exec(`UPDATE schedule_runs SET finished_at=?, ok=?, output=? WHERE id=?`, s.now(), ok, output, runID)
	return err
}

func (s *Store) ScheduleHistory(id string, limit int) ([]ScheduleRun, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id,schedule_id,started_at,COALESCE(finished_at,0),COALESCE(ok,0),COALESCE(output,'') FROM schedule_runs WHERE schedule_id=? ORDER BY id DESC LIMIT ?`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScheduleRun{}
	for rows.Next() {
		var r ScheduleRun
		if err := rows.Scan(&r.ID, &r.ScheduleID, &r.StartedAt, &r.FinishedAt, &r.OK, &r.Output); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
