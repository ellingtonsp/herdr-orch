package store

import (
	"database/sql"
	"strings"
)

type Task struct {
	ID             string   `json:"id"`
	RunID          string   `json:"run_id"`
	Title          string   `json:"title"`
	Spec           string   `json:"spec,omitempty"`
	Deps           []string `json:"deps"`
	Status         string   `json:"status"`
	AssigneePaneID string   `json:"assignee_pane_id,omitempty"`
	Attempts       int      `json:"attempts"`
	Result         string   `json:"result,omitempty"`
	CreatedAt      int64    `json:"created_at"`
	UpdatedAt      int64    `json:"updated_at"`
}

const taskCols = `id,run_id,title,spec,deps,status,COALESCE(assignee_pane_id,''),attempts,COALESCE(result,''),created_at,updated_at`

func scanTask(r interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var deps string
	err := r.Scan(&t.ID, &t.RunID, &t.Title, &t.Spec, &deps, &t.Status, &t.AssigneePaneID, &t.Attempts, &t.Result, &t.CreatedAt, &t.UpdatedAt)
	t.Deps = parseList(deps)
	return t, err
}

func getTask(q querier, id string) (Task, error) {
	t, err := scanTask(q.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return t, refuse("unknown_task", "no task %q", id)
	}
	return t, err
}

func (s *Store) GetTask(id string) (Task, error) { return getTask(s.db, id) }

func (s *Store) ListTasks(run string) ([]Task, error) {
	rows, err := s.db.Query(`SELECT `+taskCols+` FROM tasks WHERE run_id=? ORDER BY created_at, id`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateTask adds a task. Deps must be existing tasks in the same run, so creation can
// never form a cycle. The task starts ready when every dep is completed, else pending.
func (s *Store) CreateTask(run, title, spec string, deps []string) (Task, error) {
	if strings.TrimSpace(title) == "" {
		return Task{}, refuse("bad_value", "task needs a title")
	}
	var id string
	err := s.tx(func(tx *sql.Tx) error {
		r, err := scanRun(tx.QueryRow(`SELECT `+runCols+` FROM runs WHERE id=?`, run))
		if err == sql.ErrNoRows {
			return refuse("unknown_run", "no run %q", run)
		} else if err != nil {
			return err
		}
		_ = r
		deps = dedupe(deps)
		for _, d := range deps {
			dt, err := getTask(tx, d)
			if err != nil {
				return err
			}
			if dt.RunID != run {
				return refuse("bad_dep", "dep %s belongs to run %s, not %s", d, dt.RunID, run)
			}
		}
		id, err = nextID(tx, "t")
		if err != nil {
			return err
		}
		now := s.now()
		if _, err := tx.Exec(`INSERT INTO tasks(id,run_id,title,spec,deps,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
			id, run, title, spec, jsonList(deps), TaskPending, now, now); err != nil {
			return err
		}
		_, err = refreshReadiness(tx, run, now)
		return err
	})
	if err != nil {
		return Task{}, err
	}
	return s.GetTask(id)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// refreshReadiness recomputes pending/ready/blocked for every undispatched task in run and
// returns the ids of tasks that just became ready.
func refreshReadiness(q querier, run string, now int64) ([]string, error) {
	rows, err := q.Query(`SELECT `+taskCols+` FROM tasks WHERE run_id=? ORDER BY created_at, id`, run)
	if err != nil {
		return nil, err
	}
	all := map[string]Task{}
	var order []string
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		all[t.ID] = t
		order = append(order, t.ID)
	}
	rows.Close()
	gated := map[string]bool{}
	grows, err := q.Query(`SELECT task_id FROM gates WHERE run_id=? AND task_id IS NOT NULL AND status IN (?,?)`, run, GatePending, GateTimeout)
	if err != nil {
		return nil, err
	}
	for grows.Next() {
		var id string
		if err := grows.Scan(&id); err != nil {
			grows.Close()
			return nil, err
		}
		gated[id] = true
	}
	grows.Close()
	var nowReady []string
	for _, id := range order {
		t := all[id]
		if t.Status != TaskPending && t.Status != TaskReady && t.Status != TaskBlocked {
			continue
		}
		want := TaskReady
		if gated[id] {
			want = TaskBlocked
		} else {
			for _, d := range t.Deps {
				if all[d].Status != TaskCompleted {
					want = TaskPending
					break
				}
			}
		}
		if want != t.Status {
			if _, err := q.Exec(`UPDATE tasks SET status=?,updated_at=? WHERE id=?`, want, now, id); err != nil {
				return nil, err
			}
			if want == TaskReady {
				nowReady = append(nowReady, id)
			}
		}
	}
	return nowReady, nil
}

type TaskUpdate struct {
	Title         *string
	Spec          *string
	Status        *string
	Result        *string
	Deps          *[]string
	ResetAttempts bool
}

// UpdateTask edits a task. Status may be set to pending|ready (re-evaluated against deps
// and gates), completed or failed. A dispatched task's status cannot be changed here;
// stop or fence its dispatch instead. Returns tasks that became ready.
func (s *Store) UpdateTask(id string, u TaskUpdate) (Task, []string, error) {
	var ready []string
	err := s.tx(func(tx *sql.Tx) error {
		t, err := getTask(tx, id)
		if err != nil {
			return err
		}
		now := s.now()
		set := func(col string, v any) error {
			_, err := tx.Exec(`UPDATE tasks SET `+col+`=?,updated_at=? WHERE id=?`, v, now, id)
			return err
		}
		if u.Title != nil {
			if err := set("title", *u.Title); err != nil {
				return err
			}
		}
		if u.Spec != nil {
			if err := set("spec", *u.Spec); err != nil {
				return err
			}
		}
		if u.Result != nil {
			if err := set("result", *u.Result); err != nil {
				return err
			}
		}
		if u.ResetAttempts {
			if err := set("attempts", 0); err != nil {
				return err
			}
		}
		if u.Deps != nil {
			deps := dedupe(*u.Deps)
			for _, d := range deps {
				dt, err := getTask(tx, d)
				if err != nil {
					return err
				}
				if dt.RunID != t.RunID {
					return refuse("bad_dep", "dep %s is in another run", d)
				}
			}
			if err := checkCycle(tx, t.RunID, id, deps); err != nil {
				return err
			}
			if err := set("deps", jsonList(deps)); err != nil {
				return err
			}
		}
		if u.Status != nil {
			if t.Status == TaskDispatched {
				return refuse("task_dispatched", "task %s is dispatched; use `horch worker stop` or wait for it to settle", id)
			}
			switch *u.Status {
			case TaskPending, TaskReady:
				// Re-evaluated below.
				if err := set("status", TaskPending); err != nil {
					return err
				}
			case TaskCompleted, TaskFailed:
				if err := set("status", *u.Status); err != nil {
					return err
				}
			default:
				return refuse("bad_status", "task status can be set to pending|ready|completed|failed")
			}
		}
		ready, err = refreshReadiness(tx, t.RunID, now)
		return err
	})
	if err != nil {
		return Task{}, nil, err
	}
	t, err := s.GetTask(id)
	return t, ready, err
}

// checkCycle refuses deps for id that would make id reachable from itself.
func checkCycle(q querier, run, id string, deps []string) error {
	rows, err := q.Query(`SELECT id,deps FROM tasks WHERE run_id=?`, run)
	if err != nil {
		return err
	}
	graph := map[string][]string{}
	for rows.Next() {
		var tid, d string
		if err := rows.Scan(&tid, &d); err != nil {
			rows.Close()
			return err
		}
		graph[tid] = parseList(d)
	}
	rows.Close()
	graph[id] = deps
	seen := map[string]bool{}
	var visit func(string) bool
	visit = func(n string) bool {
		if n == id {
			return true
		}
		if seen[n] {
			return false
		}
		seen[n] = true
		for _, d := range graph[n] {
			if visit(d) {
				return true
			}
		}
		return false
	}
	for _, d := range deps {
		if visit(d) {
			return refuse("dep_cycle", "deps %v would create a cycle through %s", deps, id)
		}
	}
	return nil
}

// ReadyTasks lists ready tasks in run, oldest first.
func (s *Store) ReadyTasks(run string) ([]Task, error) {
	ts, err := s.ListTasks(run)
	if err != nil {
		return nil, err
	}
	out := []Task{}
	for _, t := range ts {
		if t.Status == TaskReady {
			out = append(out, t)
		}
	}
	return out, nil
}
