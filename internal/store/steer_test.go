package store

import (
	"reflect"
	"testing"
)

func steerMsg(t *testing.T, s *Store, to, body string) Message {
	t.Helper()
	return must[Message](t)(s.InsertMessage(Message{From: "w1:p1", To: to, Kind: KindSteer, Subject: "s", Body: body, DeliveryStatus: SteerQueued}))
}

func ids(ms []Message) []string {
	out := []string{}
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func TestSteerStateTransitionsAndOrdering(t *testing.T) {
	s := open(t)
	if _, err := s.RegisterWorker(Worker{PaneID: "w1:p2", Agent: "claude"}); err != nil {
		t.Fatal(err)
	}
	a := steerMsg(t, s, "w1:p2", "first")
	b := steerMsg(t, s, "w1:p2", "second")
	other := steerMsg(t, s, "w1:p3", "elsewhere")

	got := must[[]Message](t)(s.SteerPull("w1:p2", nil))
	if !reflect.DeepEqual(ids(got), []string{a.ID, b.ID}) {
		t.Fatalf("order: %v", ids(got))
	}
	for _, m := range got {
		if m.DeliveryStatus != SteerInjected || m.InjectedAt == 0 || m.InjectCount != 1 || m.AckedAt != 0 {
			t.Fatalf("not injected: %+v", m)
		}
	}
	if o := must[Message](t)(s.GetMessage(other.ID)); o.DeliveryStatus != SteerQueued {
		t.Fatalf("other pane touched: %+v", o)
	}

	// Never acknowledged: the unacknowledged notes come back, in order.
	again := must[[]Message](t)(s.SteerPull("w1:p2", nil))
	if !reflect.DeepEqual(ids(again), []string{a.ID, b.ID}) || again[0].InjectCount != 2 {
		t.Fatalf("re-inject: %+v", again)
	}

	// Acknowledging a is idempotent and leaves b pending; c queues behind b.
	c := steerMsg(t, s, "w1:p2", "third")
	for i := 0; i < 2; i++ {
		got = must[[]Message](t)(s.SteerPull("w1:p2", []string{a.ID}))
		if !reflect.DeepEqual(ids(got), []string{b.ID, c.ID}) {
			t.Fatalf("pass %d: %v", i, ids(got))
		}
	}
	first := must[Message](t)(s.GetMessage(a.ID))
	if first.DeliveryStatus != SteerAcknowledged || first.AckedAt == 0 || first.InjectCount != 2 {
		t.Fatalf("ack: %+v", first)
	}
	got = must[[]Message](t)(s.SteerPull("w1:p2", ids(got)))
	if len(got) != 0 {
		t.Fatalf("acknowledged notes returned: %v", ids(got))
	}
}

func TestSteerAckIsRecipientScopedAndNeedsInjection(t *testing.T) {
	s := open(t)
	if _, err := s.RegisterWorker(Worker{PaneID: "w1:p2", Agent: "claude"}); err != nil {
		t.Fatal(err)
	}
	m := steerMsg(t, s, "w1:p2", "x")
	// Not yet injected: an ack for it is a no-op.
	must[[]Message](t)(s.SteerPull("w1:p3", []string{m.ID}))
	must[[]Message](t)(s.SteerPull("w1:p2", []string{m.ID}))
	if got := must[Message](t)(s.GetMessage(m.ID)); got.DeliveryStatus != SteerInjected {
		t.Fatalf("acked before it was injected: %+v", got)
	}
}

func TestSteerReleaseRequeues(t *testing.T) {
	s := open(t)
	if _, err := s.RegisterWorker(Worker{PaneID: "w1:p2", Agent: "claude"}); err != nil {
		t.Fatal(err)
	}
	m := steerMsg(t, s, "w1:p2", "x")
	must[[]Message](t)(s.SteerPull("w1:p2", nil))
	if err := s.SteerRelease("w1:p3", []string{m.ID}); err != nil {
		t.Fatal(err)
	}
	if got := must[Message](t)(s.GetMessage(m.ID)); got.DeliveryStatus != SteerInjected {
		t.Fatalf("other pane released it: %+v", got)
	}
	if err := s.SteerRelease("w1:p2", []string{m.ID}); err != nil {
		t.Fatal(err)
	}
	if got := must[Message](t)(s.GetMessage(m.ID)); got.DeliveryStatus != SteerQueued || got.InjectedAt != 0 {
		t.Fatalf("release: %+v", got)
	}
}

func TestSteerStaysOutOfMailboxAndTerminalDelivery(t *testing.T) {
	s := open(t)
	steerMsg(t, s, "w1:p2", "x")
	if n := must[int](t)(s.UnreadCount("w1:p2")); n != 0 {
		t.Fatalf("unread count %d", n)
	}
	if ms := must[[]Message](t)(s.Unread([]string{"w1:p2"}, true)); len(ms) != 0 {
		t.Fatalf("consumed by check: %+v", ms)
	}
	if ms := must[[]Message](t)(s.PendingDeliveries()); len(ms) != 0 {
		t.Fatalf("scheduled for typing: %+v", ms)
	}
	if in := must[[]Message](t)(s.Inbox("w1:p1", 10)); len(in) != 1 || in[0].Kind != KindSteer {
		t.Fatalf("not visible in inbox: %+v", in)
	}
}

func TestSteerPullBoundedAndExpiredOnRelease(t *testing.T) {
	s := open(t)
	if _, err := s.RegisterWorker(Worker{PaneID: "w1:p2", Agent: "claude"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < SteerPullMax+2; i++ {
		if _, err := s.InsertMessage(Message{From: "c:p1", To: "w1:p2", Kind: KindSteer, Body: "n", DeliveryStatus: SteerQueued}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.SteerPull("w1:p2", nil)
	if err != nil || len(got) != SteerPullMax {
		t.Fatalf("first pull: %d %v", len(got), err)
	}
	if err := s.SetWorkerState("w1:p2", WorkerReleased, "", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.SteerPull("w1:p2", nil); len(got) != 0 {
		t.Fatalf("released pane still pulls %d notes", len(got))
	}
	// a new tenant of the same pane id starts clean
	if _, err := s.RegisterWorker(Worker{PaneID: "w1:p2", Agent: "claude"}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.SteerPull("w1:p2", nil); len(got) != 0 {
		t.Fatalf("new tenant received %d stale notes", len(got))
	}
}
