package store

import (
	"path/filepath"
	"testing"
)

func TestReceiptsSurviveReopenAndLateRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orch.db")
	s := must[*Store](t)(Open(path))
	m := must[Message](t)(s.InsertMessage(Message{From: "w1:p1", To: "w1:p2", Kind: KindNote, Body: "hello", DeliveryStatus: "queued"}))
	if _, err := s.DeliveryAttempt(m.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delivered(m.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Undelivered(m.ID, "no pickup"); err != nil {
		t.Fatal(err)
	}
	before := must[Message](t)(s.GetMessage(m.ID))
	s.Close()
	s = must[*Store](t)(Open(path))
	defer s.Close()
	after := must[Message](t)(s.GetMessage(m.ID))
	if before != after {
		t.Fatalf("receipt lost: %+v %+v", before, after)
	}
	ack := must[Message](t)(s.AckMessage(m.ID, m.To))
	if ack.DeliveryStatus != "picked_up" || ack.PickupVia != "read" || ack.PickedUpAt == 0 || ack.DeliveredAt == 0 || ack.DeliveryAttempts != 1 || ack.DeliveryError != "" {
		t.Fatalf("late ack: %+v", ack)
	}
	if pending := must[[]Message](t)(s.PendingDeliveries()); len(pending) != 0 {
		t.Fatalf("read pending: %+v", pending)
	}
}
func TestWorkingPickupDoesNotConsumeMailbox(t *testing.T) {
	s := open(t)
	m := must[Message](t)(s.InsertMessage(Message{From: "w1:p1", To: "w1:p2", Kind: KindNote, DeliveryStatus: "queued"}))
	if err := s.PickedUp(m.ID, "working"); err != nil {
		t.Fatal(err)
	}
	unread := must[[]Message](t)(s.Unread([]string{m.To}, false))
	if len(unread) != 1 || unread[0].ReadAt != 0 || unread[0].PickupVia != "working" {
		t.Fatalf("%+v", unread)
	}
}

func TestReadBeforeDeliveryAttemptPreventsSubmission(t *testing.T) {
	s := open(t)
	m := must[Message](t)(s.InsertMessage(Message{From: "w1:p1", To: "w1:p2", Kind: KindNote, DeliveryStatus: "queued"}))
	must[Message](t)(s.AckMessage(m.ID, m.To))
	attempted, err := s.DeliveryAttempt(m.ID)
	if err != nil || attempted {
		t.Fatalf("attempted consumed message: %v %v", attempted, err)
	}
	got := must[Message](t)(s.GetMessage(m.ID))
	if got.DeliveryAttempts != 0 {
		t.Fatalf("%+v", got)
	}
}
