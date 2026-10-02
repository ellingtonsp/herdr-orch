package store

import (
	"database/sql"
	"strings"
)

type Message struct {
	ID        string `json:"id"`
	RunID     string `json:"run_id,omitempty"`
	From      string `json:"from"`
	To        string `json:"to"`
	Subject   string `json:"subject,omitempty"`
	Body      string `json:"body,omitempty"`
	Kind      string `json:"kind"`
	TaskID    string `json:"task_id,omitempty"`
	ReplyTo   string `json:"reply_to,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	CreatedAt int64  `json:"created_at"`
	ReadAt    int64  `json:"read_at,omitempty"`
}

const msgCols = `id,COALESCE(run_id,''),from_pane,to_pane,subject,body,kind,COALESCE(task_id,''),COALESCE(reply_to,''),COALESCE(outcome,''),created_at,COALESCE(read_at,0)`

func scanMsg(r interface{ Scan(...any) error }) (Message, error) {
	var m Message
	err := r.Scan(&m.ID, &m.RunID, &m.From, &m.To, &m.Subject, &m.Body, &m.Kind, &m.TaskID, &m.ReplyTo, &m.Outcome, &m.CreatedAt, &m.ReadAt)
	return m, err
}

func ValidKind(k string) bool {
	switch k {
	case KindNote, KindQuestion, KindReply, KindDone, KindEscalation:
		return true
	}
	return false
}

// InsertMessage stores m, assigning its id and timestamp.
func (s *Store) InsertMessage(m Message) (Message, error) {
	if !ValidKind(m.Kind) {
		return m, refuse("bad_kind", "kind must be note|question|reply|done|escalation")
	}
	if m.To == "" {
		return m, refuse("no_recipient", "message has no recipient")
	}
	err := s.tx(func(tx *sql.Tx) error {
		var err error
		m.ID, err = insertMessage(tx, m, s.now())
		return err
	})
	if err != nil {
		return m, err
	}
	return s.GetMessage(m.ID)
}

func insertMessage(q querier, m Message, now int64) (string, error) {
	id, err := nextID(q, "m")
	if err != nil {
		return "", err
	}
	// Reports to the daemon itself are consumed on arrival; they never sit unread.
	var readAt any
	if m.To == Daemon {
		readAt = now
	}
	_, err = q.Exec(`INSERT INTO messages(id,run_id,from_pane,to_pane,subject,body,kind,task_id,reply_to,outcome,created_at,read_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, nullStr(m.RunID), m.From, m.To, m.Subject, m.Body, m.Kind, nullStr(m.TaskID), nullStr(m.ReplyTo), nullStr(m.Outcome), now, readAt)
	return id, err
}

func (s *Store) GetMessage(id string) (Message, error) {
	m, err := scanMsg(s.db.QueryRow(`SELECT `+msgCols+` FROM messages WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return m, refuse("unknown_message", "no message %q", id)
	}
	return m, err
}

func (s *Store) queryMsgs(q string, args ...any) ([]Message, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Unread returns unread messages to any of recipients, oldest first, and marks them read
// when mark is true.
func (s *Store) Unread(recipients []string, mark bool) ([]Message, error) {
	if len(recipients) == 0 {
		return []Message{}, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(recipients)), ",")
	args := make([]any, len(recipients))
	for i, r := range recipients {
		args[i] = r
	}
	msgs, err := s.queryMsgs(`SELECT `+msgCols+` FROM messages WHERE read_at IS NULL AND to_pane IN (`+ph+`) ORDER BY created_at, id`, args...)
	if err != nil || !mark || len(msgs) == 0 {
		return msgs, err
	}
	now := s.now()
	err = s.tx(func(tx *sql.Tx) error {
		for i := range msgs {
			if _, err := tx.Exec(`UPDATE messages SET read_at=? WHERE id=?`, now, msgs[i].ID); err != nil {
				return err
			}
			msgs[i].ReadAt = now
		}
		return nil
	})
	return msgs, err
}

// UnreadCount counts unread messages to pane.
func (s *Store) UnreadCount(pane string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE to_pane=? AND read_at IS NULL`, pane).Scan(&n)
	return n, err
}

// Inbox lists the most recent messages to or from pane.
func (s *Store) Inbox(pane string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.queryMsgs(`SELECT `+msgCols+` FROM (SELECT * FROM messages WHERE to_pane=? OR from_pane=? ORDER BY created_at DESC, id DESC LIMIT ?) ORDER BY created_at, id`, pane, pane, limit)
}

// RecentMessages lists the most recent messages in a run (or all runs if run is "").
func (s *Store) RecentMessages(run string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 30
	}
	if run == "" {
		return s.queryMsgs(`SELECT `+msgCols+` FROM (SELECT * FROM messages ORDER BY created_at DESC, id DESC LIMIT ?) ORDER BY created_at, id`, limit)
	}
	return s.queryMsgs(`SELECT `+msgCols+` FROM (SELECT * FROM messages WHERE run_id=? ORDER BY created_at DESC, id DESC LIMIT ?) ORDER BY created_at, id`, run, limit)
}

// TakeReply returns the first reply to question id, marking it read, or ErrNotFound.
func (s *Store) TakeReply(question string) (Message, error) {
	m, err := scanMsg(s.db.QueryRow(`SELECT `+msgCols+` FROM messages WHERE reply_to=? AND kind='reply' ORDER BY created_at LIMIT 1`, question))
	if err == sql.ErrNoRows {
		return m, ErrNotFound
	}
	if err != nil {
		return m, err
	}
	if m.ReadAt == 0 {
		m.ReadAt = s.now()
		_, err = s.db.Exec(`UPDATE messages SET read_at=? WHERE id=?`, m.ReadAt, m.ID)
	}
	return m, err
}

// OpenAttention keeps unanswered questions visible even after they were read,
// while acknowledged escalations leave the attention surface. Reads consume nothing.
func (s *Store) OpenAttention() ([]Message, error) {
	return s.queryMsgs(`SELECT ` + msgCols + ` FROM messages WHERE
 (kind='question' AND NOT EXISTS (SELECT 1 FROM messages reply WHERE reply.reply_to=messages.id AND reply.kind='reply'))
 OR (kind='escalation' AND read_at IS NULL) ORDER BY created_at, id`)
}
