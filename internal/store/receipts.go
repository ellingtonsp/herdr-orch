package store

// readReceipt exposes a mailbox read as positive pickup evidence, including for
// messages that were read before a terminal delivery job started.
func readReceipt(m Message) Message {
	if m.ReadAt != 0 && m.DeliveryStatus != "" {
		if m.PickedUpAt == 0 {
			m.PickedUpAt = m.ReadAt
			m.PickupVia = "read"
		}
		m.DeliveryStatus = "picked_up"
		m.DeliveryError = ""
	}
	return m
}

func (s *Store) QueueDelivery(id string) (Message, error) {
	_, err := s.db.Exec(`UPDATE messages SET delivery_status='queued' WHERE id=?`, id)
	if err != nil {
		return Message{}, err
	}
	return s.GetMessage(id)
}

func (s *Store) DeliveryAttempt(id string) (bool, error) {
	result, err := s.db.Exec(`UPDATE messages SET delivery_status='submitting',delivery_attempts=delivery_attempts+1 WHERE id=? AND read_at IS NULL AND picked_up_at IS NULL`, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

func (s *Store) Delivered(id string) error {
	_, err := s.db.Exec(`UPDATE messages SET delivered_at=COALESCE(delivered_at,?),delivery_status=CASE WHEN picked_up_at IS NULL THEN 'delivered' ELSE 'picked_up' END,delivery_error='' WHERE id=?`, s.now(), id)
	return err
}

func (s *Store) PickedUp(id, via string) error {
	_, err := s.db.Exec(`UPDATE messages SET picked_up_at=COALESCE(picked_up_at,?),pickup_via=CASE WHEN picked_up_at IS NULL THEN ? ELSE pickup_via END,delivery_status='picked_up',delivery_error='' WHERE id=?`, s.now(), via, id)
	return err
}

func (s *Store) Undelivered(id, reason string) error {
	_, err := s.db.Exec(`UPDATE messages SET delivery_status='undelivered',delivery_error=? WHERE id=? AND read_at IS NULL AND picked_up_at IS NULL`, reason, id)
	return err
}

func (s *Store) PendingDeliveries() ([]Message, error) {
	return s.queryMsgs(`SELECT ` + msgCols + ` FROM messages WHERE delivery_status IN ('queued','submitting','delivered') AND read_at IS NULL AND picked_up_at IS NULL ORDER BY created_at,id`)
}
