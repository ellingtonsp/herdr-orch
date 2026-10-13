package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/herdr"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

type deliveryHerdr struct {
	*fakeHerdr
	muDelivery     sync.Mutex
	sends, submits int
	pickupOn       int
	screen         string
	sendError      bool
}

func (h *deliveryHerdr) AgentPrompt(ctx context.Context, pane, text string) error {
	if err := h.fakeHerdr.AgentPrompt(ctx, pane, text); err != nil {
		return err
	}
	h.muDelivery.Lock()
	h.sends++
	n := h.sends
	h.muDelivery.Unlock()
	if h.pickupOn == n {
		h.mu.Lock()
		h.panes[pane].AgentStatus = "working"
		h.mu.Unlock()
	}
	if h.sendError {
		return errors.New("response lost")
	}
	return nil
}
func (h *deliveryHerdr) PaneRead(context.Context, string, int) (string, error) { return h.screen, nil }
func (h *deliveryHerdr) SubmitPending(_ context.Context, pane string) error {
	h.muDelivery.Lock()
	h.submits++
	h.muDelivery.Unlock()
	h.mu.Lock()
	h.panes[pane].AgentStatus = "working"
	h.mu.Unlock()
	return nil
}
func setupDelivery(t *testing.T) (*harness, *deliveryHerdr, store.Message) {
	t.Helper()
	x := newHarness(t)
	x.h.addPane("w1:p2", "claude", "idle", true)
	h := &deliveryHerdr{fakeHerdr: x.h}
	x.e.h = h
	x.e.deliveryReady = 200 * time.Millisecond
	x.e.deliveryPickup = 30 * time.Millisecond
	x.e.deliveryPoll = time.Millisecond
	var run store.Run
	x.mustOp("w1:p1", "run.create", RunCreateArgs{Title: "delivery"}, &run)
	m, err := x.st.InsertMessage(store.Message{RunID: run.ID, From: "w1:p1", To: "w1:p2", Kind: store.KindNote, Body: strings.Repeat("long text\n", 500)})
	if err != nil {
		t.Fatal(err)
	}
	m, err = x.st.QueueDelivery(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	return x, h, m
}
func TestDeliveryWaitsForInteractiveIdle(t *testing.T) {
	for _, status := range []string{"unknown", "working", "idle"} {
		t.Run(status, func(t *testing.T) {
			x, h, m := setupDelivery(t)
			h.pickupOn = 1
			h.mu.Lock()
			h.panes[m.To].AgentStatus = status
			ready := false
			h.panes[m.To].InteractiveReady = &ready
			h.mu.Unlock()
			done := make(chan struct{})
			go func() { x.e.deliverMessage(context.Background(), m); close(done) }()
			time.Sleep(5 * time.Millisecond)
			if h.promptCount(m.To) != 0 {
				t.Fatal("typed before interactive readiness")
			}
			h.mu.Lock()
			ready2 := true
			h.panes[m.To].InteractiveReady = &ready2
			h.panes[m.To].AgentStatus = "idle"
			h.mu.Unlock()
			<-done
			got, _ := x.st.GetMessage(m.ID)
			if got.DeliveryStatus != "picked_up" || got.PickupVia != "working" || got.DeliveredAt == 0 || got.DeliveryAttempts != 1 {
				t.Fatalf("receipt: %+v", got)
			}
			if !strings.Contains(h.prompts[m.To][0], m.Body) {
				t.Fatal("multiline body was changed")
			}
		})
	}
}
func TestDeliveryRetriesCollapsedPasteWithoutRepasting(t *testing.T) {
	for _, marker := range []string{"paste again to expand", "[Pasted text #1 +500 lines]", "[Pasted Content 1000 chars]"} {
		t.Run(marker, func(t *testing.T) {
			x, h, m := setupDelivery(t)
			h.screen = marker
			x.e.deliverMessage(context.Background(), m)
			got, _ := x.st.GetMessage(m.ID)
			if h.sends != 1 || h.submits != 1 || got.DeliveryAttempts != 2 || !picked(got) {
				t.Fatalf("sends=%d submits=%d receipt=%+v", h.sends, h.submits, got)
			}
		})
	}
}
func TestDeliveryRetriesSwallowedPromptOnce(t *testing.T) {
	x, h, m := setupDelivery(t)
	h.pickupOn = 2
	x.e.deliverMessage(context.Background(), m)
	got, _ := x.st.GetMessage(m.ID)
	if h.sends != 2 || !picked(got) {
		t.Fatalf("sends=%d receipt=%+v", h.sends, got)
	}
}
func TestDeliveryEscalatesAfterTwoUnconfirmedSubmissions(t *testing.T) {
	x, h, m := setupDelivery(t)
	x.e.deliverMessage(context.Background(), m)
	got, _ := x.st.GetMessage(m.ID)
	if h.sends != 2 || got.DeliveryStatus != "undelivered" || got.DeliveredAt == 0 || got.PickedUpAt != 0 {
		t.Fatalf("sends=%d receipt=%+v", h.sends, got)
	}
	ms, _ := x.st.Inbox("w1:p1", 50)
	n := 0
	for _, message := range ms {
		if message.Kind == store.KindEscalation && strings.Contains(message.Subject, m.ID) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("escalations=%d", n)
	}
	_, err := x.e.waitPickup(context.Background(), []store.Message{m}, time.Second)
	wantRefusal(t, err, "undelivered")
}
func TestDeliveryReadSuppressesTyping(t *testing.T) {
	x, h, m := setupDelivery(t)
	if _, err := x.st.Unread([]string{m.To}, true); err != nil {
		t.Fatal(err)
	}
	x.e.deliverMessage(context.Background(), m)
	got, _ := x.st.GetMessage(m.ID)
	if h.sends != 0 || got.DeliveryStatus != "picked_up" || got.PickupVia != "read" || got.DeliveredAt != 0 {
		t.Fatalf("receipt: %+v", got)
	}
}
func TestDeliveryBusyNeverTypes(t *testing.T) {
	x, h, m := setupDelivery(t)
	h.mu.Lock()
	h.panes[m.To].AgentStatus = "blocked"
	h.mu.Unlock()
	x.e.deliverMessage(context.Background(), m)
	got, _ := x.st.GetMessage(m.ID)
	if h.sends != 0 || got.DeliveryStatus != "undelivered" || got.DeliveryAttempts != 0 {
		t.Fatalf("receipt: %+v", got)
	}
}
func TestDeliveryObservesPickupAfterTransportError(t *testing.T) {
	x, h, m := setupDelivery(t)
	h.pickupOn = 1
	h.sendError = true
	x.e.deliverMessage(context.Background(), m)
	got, _ := x.st.GetMessage(m.ID)
	if h.sends != 1 || !picked(got) {
		t.Fatalf("sends=%d receipt=%+v", h.sends, got)
	}
}
func TestSendWaitPickupAndWaitTimeout(t *testing.T) {
	x, h, _ := setupDelivery(t)
	h.pickupOn = 1
	x.e.deliveryLaunch = func(f func()) { go f() }
	out, err := x.e.opSend(context.Background(), "w1:p1", mustJSON(SendArgs{To: "w1:p2", Body: "hello", WaitPickup: true, TimeoutMS: 1000}))
	if err != nil {
		t.Fatal(err)
	}
	ms := out.([]store.Message)
	if len(ms) != 1 || !picked(ms[0]) {
		t.Fatalf("%+v", out)
	}
	// Human delivery remains queued for a mailbox read; a wait timeout does not discard it.
	_, err = x.e.opSend(context.Background(), "w1:p1", mustJSON(SendArgs{To: "human", Body: "review", WaitPickup: true, TimeoutMS: 1}))
	wantRefusal(t, err, "pickup_timeout")
	unread, _ := x.st.Unread([]string{"human"}, false)
	if len(unread) != 1 {
		t.Fatal("timeout lost message")
	}
}
func TestReplyPickupViaAskAvoidsTerminalInput(t *testing.T) {
	x, h, m := setupDelivery(t)
	q, err := x.st.InsertMessage(store.Message{RunID: m.RunID, From: m.To, To: m.From, Kind: store.KindQuestion, Body: "which option?"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := x.e.opReply(context.Background(), m.From, mustJSON(ReplyArgs{ID: q.ID, Body: "option A"}))
	if err != nil {
		t.Fatal(err)
	}
	reply := out.(store.Message)
	if _, err = x.e.awaitReply(context.Background(), q, time.Second); err != nil {
		t.Fatal(err)
	}
	x.e.deliverMessage(context.Background(), reply)
	got, _ := x.st.GetMessage(reply.ID)
	if h.sends != 0 || !picked(got) {
		t.Fatalf("%+v", got)
	}
	if _, err = x.e.opReply(context.Background(), m.From, json.RawMessage(`{"`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}
func TestRecoverDeliveryDoesNotReplayUncertainWrite(t *testing.T) {
	x, h, m := setupDelivery(t)
	if _, err := x.st.DeliveryAttempt(m.ID); err != nil {
		t.Fatal(err)
	}
	if err := x.e.RecoverDeliveries(); err != nil {
		t.Fatal(err)
	}
	got, _ := x.st.GetMessage(m.ID)
	if h.sends != 0 || got.DeliveryStatus != "undelivered" {
		t.Fatalf("%+v", got)
	}
}

func TestCanceledDeliveryRemainsRecoverable(t *testing.T) {
	x, h, m := setupDelivery(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	x.e.deliverMessage(ctx, m)
	got, _ := x.st.GetMessage(m.ID)
	if h.sends != 0 || got.DeliveryStatus != "queued" {
		t.Fatalf("%+v", got)
	}
	h.pickupOn = 1
	x.e.deliveryLaunch = func(f func()) { f() }
	if err := x.e.RecoverDeliveries(); err != nil {
		t.Fatal(err)
	}
	got, _ = x.st.GetMessage(m.ID)
	if h.sends != 1 || !picked(got) {
		t.Fatalf("recovery: %+v", got)
	}
}

type deadlinePickupHerdr struct {
	*fakeHerdr
	engine *Engine
}

func (h *deadlinePickupHerdr) PaneGet(ctx context.Context, pane string) (herdr.Pane, error) {
	<-ctx.Done()
	h.engine.setStatus(pane, "working")
	return herdr.Pane{}, ctx.Err()
}
func TestPickupRejectsWorkingEvidenceAfterDeadline(t *testing.T) {
	x, _, m := setupDelivery(t)
	x.e.h = &deadlinePickupHerdr{fakeHerdr: x.h, engine: x.e}
	if x.e.awaitPickup(context.Background(), m, 0) {
		t.Fatal("accepted working event after deadline")
	}
	got, _ := x.st.GetMessage(m.ID)
	if picked(got) {
		t.Fatalf("persisted late evidence: %+v", got)
	}
}
