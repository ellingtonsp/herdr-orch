package store

import "database/sql"

// AckMessage marks one message to recipient read. Retrying preserves its original
// read timestamp, and a different recipient cannot acknowledge the message.
func (s *Store) AckMessage(id, recipient string) (Message, error) {
	var m Message
	err := s.tx(func(tx *sql.Tx) error {
		var err error
		m, err = scanMsg(tx.QueryRow(`SELECT `+msgCols+` FROM messages WHERE id=?`, id))
		if err == sql.ErrNoRows {
			return refuse("unknown_message", "no message %q", id)
		}
		if err != nil {
			return err
		}
		if m.To != recipient {
			return refuse("not_recipient", "message %s is not addressed to %s", id, recipient)
		}
		if m.ReadAt != 0 {
			return nil
		}
		m.ReadAt = s.now()
		_, err = tx.Exec(`UPDATE messages SET read_at=? WHERE id=? AND read_at IS NULL`, m.ReadAt, id)
		return err
	})
	if err != nil {
		return Message{}, err
	}
	// Re-read so projections derived from read_at reflect this acknowledgment.
	return s.GetMessage(m.ID)
}
