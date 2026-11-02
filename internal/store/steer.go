package store

import "database/sql"

// Steering receipts. A steer note is never typed into a terminal and never joins the
// consuming mailbox; the worker's turn-boundary hook pulls it:
// queued -> injected (printed to the model) -> acknowledged (a later hook call confirms).
const (
	SteerQueued       = "queued"
	SteerInjected     = "injected"
	SteerAcknowledged = "acknowledged"
)

// SteerPull acknowledges the injected notes in ack (ids the caller proved it emitted),
// then returns pane's pending notes in send order and marks them injected. Notes that were
// injected but never acknowledged are returned again.
func (s *Store) SteerPull(pane string, ack []string) ([]Message, error) {
	out := []Message{}
	err := s.tx(func(tx *sql.Tx) error {
		now := s.now()
		for _, id := range ack {
			if _, err := tx.Exec(`UPDATE messages SET delivery_status=?,acked_at=COALESCE(acked_at,?) WHERE id=? AND to_pane=? AND kind='steer' AND delivery_status=?`, SteerAcknowledged, now, id, pane, SteerInjected); err != nil {
				return err
			}
		}
		rows, err := tx.Query(`SELECT id FROM messages WHERE to_pane=? AND kind='steer' AND delivery_status IN (?,?) ORDER BY created_at,id`, pane, SteerQueued, SteerInjected)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE messages SET delivery_status=?,injected_at=?,inject_count=inject_count+1 WHERE id=?`, SteerInjected, now, id); err != nil {
				return err
			}
			m, err := scanMsg(tx.QueryRow(`SELECT `+msgCols+` FROM messages WHERE id=?`, id))
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SteerRelease returns injected, unacknowledged notes to queued after a failed emit.
func (s *Store) SteerRelease(pane string, ids []string) error {
	return s.tx(func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE messages SET delivery_status=?,injected_at=NULL WHERE id=? AND to_pane=? AND kind='steer' AND delivery_status=?`, SteerQueued, id, pane, SteerInjected); err != nil {
				return err
			}
		}
		return nil
	})
}
