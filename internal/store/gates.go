package store

import (
	"database/sql"
	"slices"
)

type Gate struct {
	ID         string   `json:"id"`
	RunID      string   `json:"run_id"`
	TaskID     string   `json:"task_id,omitempty"`
	Question   string   `json:"question"`
	Options    []string `json:"options"`
	Status     string   `json:"status"`
	Decision   string   `json:"decision,omitempty"`
	DecidedBy  string   `json:"decided_by,omitempty"`
	TimeoutAt  int64    `json:"timeout_at,omitempty"`
	CreatedAt  int64    `json:"created_at"`
	ResolvedAt int64    `json:"resolved_at,omitempty"`
}

const gateCols = `id,run_id,COALESCE(task_id,''),question,options,status,COALESCE(decision,''),COALESCE(decided_by,''),COALESCE(timeout_at,0),created_at,COALESCE(resolved_at,0)`

func scanGate(r interface{ Scan(...any) error }) (Gate, error) {
	var g Gate
	var opts string
	err := r.Scan(&g.ID, &g.RunID, &g.TaskID, &g.Question, &opts, &g.Status, &g.Decision, &g.DecidedBy, &g.TimeoutAt, &g.CreatedAt, &g.ResolvedAt)
	g.Options = parseList(opts)
	return g, err
}

func (s *Store) GetGate(id string) (Gate, error) {
	g, err := scanGate(s.db.QueryRow(`SELECT `+gateCols+` FROM gates WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return g, refuse("unknown_gate", "no gate %q", id)
	}
	return g, err
}

// ListGates lists gates in run ("" = all runs); pendingOnly limits to pending and timed-out.
func (s *Store) ListGates(run string, pendingOnly bool) ([]Gate, error) {
	q := `SELECT ` + gateCols + ` FROM gates WHERE 1=1`
	var args []any
	if run != "" {
		q += ` AND run_id=?`
		args = append(args, run)
	}
	if pendingOnly {
		q += ` AND status IN ('pending','timeout')`
	}
	rows, err := s.db.Query(q+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Gate{}
	for rows.Next() {
		g, err := scanGate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CreateGate adds a pending decision gate. With a task, the task is blocked until the gate
// is resolved; the task must not be dispatched or finished.
func (s *Store) CreateGate(run, task, question string, options []string, timeoutAt int64) (Gate, error) {
	if question == "" {
		return Gate{}, refuse("bad_value", "gate needs a question")
	}
	var id string
	err := s.tx(func(tx *sql.Tx) error {
		if task != "" {
			t, err := getTask(tx, task)
			if err != nil {
				return err
			}
			if t.Status == TaskDispatched || t.Status == TaskCompleted || t.Status == TaskFailed {
				return refuse("task_not_gateable", "task %s is %s; gates go in front of undispatched tasks", task, t.Status)
			}
			run = t.RunID
		}
		var err error
		id, err = nextID(tx, "g")
		if err != nil {
			return err
		}
		now := s.now()
		var to any
		if timeoutAt > 0 {
			to = timeoutAt
		}
		if _, err := tx.Exec(`INSERT INTO gates(id,run_id,task_id,question,options,status,timeout_at,created_at) VALUES(?,?,?,?,?,?,?,?)`,
			id, run, nullStr(task), question, jsonList(dedupe(options)), GatePending, to, now); err != nil {
			return err
		}
		_, err = refreshReadiness(tx, run, now)
		return err
	})
	if err != nil {
		return Gate{}, err
	}
	return s.GetGate(id)
}

// ResolveGate records a decision (must be one of the options, when options were given) and
// unblocks the gate's task. Timed-out gates can still be resolved.
func (s *Store) ResolveGate(id, decision, by string) (Gate, []string, error) {
	var ready []string
	err := s.tx(func(tx *sql.Tx) error {
		g, err := scanGate(tx.QueryRow(`SELECT `+gateCols+` FROM gates WHERE id=?`, id))
		if err == sql.ErrNoRows {
			return refuse("unknown_gate", "no gate %q", id)
		} else if err != nil {
			return err
		}
		if g.Status == GateResolved {
			return refuse("gate_resolved", "gate %s was already resolved: %s", id, g.Decision)
		}
		if decision == "" {
			return refuse("bad_value", "a decision is required")
		}
		if len(g.Options) > 0 && !slices.Contains(g.Options, decision) {
			return refuse("bad_decision", "decision must be one of %v", g.Options)
		}
		now := s.now()
		if _, err := tx.Exec(`UPDATE gates SET status=?, decision=?, decided_by=?, resolved_at=? WHERE id=?`, GateResolved, decision, nullStr(by), now, id); err != nil {
			return err
		}
		ready, err = refreshReadiness(tx, g.RunID, now)
		return err
	})
	if err != nil {
		return Gate{}, nil, err
	}
	g, err := s.GetGate(id)
	return g, ready, err
}

// ExpireGates moves pending gates past their timeout to `timeout` and returns them. Their
// tasks stay blocked until someone resolves the gate.
func (s *Store) ExpireGates() ([]Gate, error) {
	now := s.now()
	rows, err := s.db.Query(`SELECT `+gateCols+` FROM gates WHERE status=? AND timeout_at IS NOT NULL AND timeout_at<=?`, GatePending, now)
	if err != nil {
		return nil, err
	}
	var out []Gate
	for rows.Next() {
		g, err := scanGate(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, g)
	}
	rows.Close()
	for i := range out {
		if _, err := s.db.Exec(`UPDATE gates SET status=? WHERE id=?`, GateTimeout, out[i].ID); err != nil {
			return nil, err
		}
		out[i].Status = GateTimeout
	}
	return out, nil
}
