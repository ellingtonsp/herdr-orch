// Package store holds all orchestration state in SQLite and owns every state transition.
//
// Only the daemon opens the store for writing. Status values match Orca's types.ts so
// skills written for Orca map one-to-one.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

// Status values (from Orca types.ts).
const (
	RunIdle      = "idle"
	RunRunning   = "running"
	RunCompleted = "completed"
	RunFailed    = "failed"

	TaskPending    = "pending"
	TaskReady      = "ready"
	TaskDispatched = "dispatched"
	TaskCompleted  = "completed"
	TaskFailed     = "failed"
	TaskBlocked    = "blocked"

	DispatchPending       = "pending"
	DispatchDispatched    = "dispatched"
	DispatchCompleted     = "completed"
	DispatchFailed        = "failed"
	DispatchCircuitBroken = "circuit_broken"

	GatePending  = "pending"
	GateResolved = "resolved"
	GateTimeout  = "timeout"

	WorkerLive          = "live"
	WorkerReleased      = "released"
	WorkerStopped       = "stopped"
	WorkerAbandoned     = "abandoned"
	WorkerExited        = "exited"
	WorkerReleaseFailed = "release_failed"

	KindNote       = "note"
	KindQuestion   = "question"
	KindReply      = "reply"
	KindDone       = "done"
	KindEscalation = "escalation"
)

// Daemon is the pseudo-pane id for messages to and from herdr-orch itself.
const Daemon = "horch"

// Refusal is a user-facing refusal (the CLI exits non-zero with Code).
type Refusal struct {
	Code    string
	Message string
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

func refuse(code, format string, a ...any) error {
	return &Refusal{Code: code, Message: fmt.Sprintf(format, a...)}
}

var ErrNotFound = errors.New("not found")

type Store struct {
	db  *sql.DB
	Now func() time.Time
}

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, Now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) now() int64 { return s.Now().UnixMilli() }

var migrations = []string{
	`CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
	CREATE TABLE runs(
		id TEXT PRIMARY KEY, title TEXT NOT NULL, coordinator_pane_id TEXT,
		status TEXT NOT NULL, auto_dispatch INTEGER NOT NULL DEFAULT 0,
		max_attempts INTEGER NOT NULL DEFAULT 3,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
	CREATE TABLE pane_runs(pane_id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id), updated_at INTEGER NOT NULL);
	CREATE TABLE messages(
		id TEXT PRIMARY KEY, run_id TEXT, from_pane TEXT NOT NULL, to_pane TEXT NOT NULL,
		subject TEXT NOT NULL DEFAULT '', body TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL,
		task_id TEXT, reply_to TEXT, outcome TEXT,
		created_at INTEGER NOT NULL, read_at INTEGER);
	CREATE INDEX messages_to ON messages(to_pane, read_at);
	CREATE INDEX messages_reply ON messages(reply_to);
	CREATE TABLE tasks(
		id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id), title TEXT NOT NULL,
		spec TEXT NOT NULL DEFAULT '', deps TEXT NOT NULL DEFAULT '[]', status TEXT NOT NULL,
		assignee_pane_id TEXT, attempts INTEGER NOT NULL DEFAULT 0, result TEXT,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
	CREATE INDEX tasks_run ON tasks(run_id, status);
	CREATE TABLE dispatches(
		id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), run_id TEXT NOT NULL,
		pane_id TEXT NOT NULL, agent TEXT, prompt_hash TEXT, status TEXT NOT NULL,
		saw_working INTEGER NOT NULL DEFAULT 0, done_message_id TEXT, done_outcome TEXT,
		idle_since INTEGER, unobserved_escalated INTEGER NOT NULL DEFAULT 0,
		started_at INTEGER NOT NULL, settled_at INTEGER, outcome TEXT);
	CREATE INDEX dispatches_task ON dispatches(task_id, status);
	CREATE INDEX dispatches_pane ON dispatches(pane_id, status);
	CREATE TABLE gates(
		id TEXT PRIMARY KEY, run_id TEXT NOT NULL, task_id TEXT, question TEXT NOT NULL,
		options TEXT NOT NULL DEFAULT '[]', status TEXT NOT NULL, decision TEXT, decided_by TEXT,
		timeout_at INTEGER, created_at INTEGER NOT NULL, resolved_at INTEGER);
	CREATE TABLE workers(
		pane_id TEXT PRIMARY KEY, run_id TEXT, name TEXT, agent TEXT, worktree TEXT,
		state TEXT NOT NULL, retained INTEGER NOT NULL DEFAULT 0, flagged_at INTEGER,
		started_at INTEGER NOT NULL, released_at INTEGER, archive_path TEXT, note TEXT);
	CREATE TABLE schedules(
		id TEXT PRIMARY KEY, cron TEXT NOT NULL, action TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1,
		last_run_at INTEGER, next_run_at INTEGER, created_at INTEGER NOT NULL);
	CREATE TABLE schedule_runs(
		id INTEGER PRIMARY KEY AUTOINCREMENT, schedule_id TEXT NOT NULL, started_at INTEGER NOT NULL,
		finished_at INTEGER, ok INTEGER, output TEXT);`,
	// 2: idle-without-report escalates instead of failing; per-run idle settings.
	`ALTER TABLE dispatches ADD COLUMN idle_escalated INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE dispatches ADD COLUMN nudges INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE runs ADD COLUMN idle_report_ms INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE runs ADD COLUMN idle_flag_ms INTEGER NOT NULL DEFAULT 0;`,
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version(v INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(v),0) FROM schema_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(v) VALUES(?)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// tx runs fn in a transaction.
func (s *Store) tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

type querier interface {
	Exec(string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

// nextID returns prefix + a per-prefix counter ("t1", "t2", ...): short ids agents can type.
func nextID(q querier, prefix string) (string, error) {
	key := "seq_" + prefix
	var cur string
	err := q.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&cur)
	n := 0
	if err == nil {
		n, _ = strconv.Atoi(cur)
	} else if err != sql.ErrNoRows {
		return "", err
	}
	n++
	if _, err := q.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, strconv.Itoa(n)); err != nil {
		return "", err
	}
	return prefix + strconv.Itoa(n), nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func jsonList(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func parseList(s string) []string {
	var v []string
	_ = json.Unmarshal([]byte(s), &v)
	if v == nil {
		v = []string{}
	}
	return v
}
