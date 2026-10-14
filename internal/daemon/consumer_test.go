package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/rpc"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

func checkConsumer(ctx context.Context, e *Engine, caller, label string, wait, peek bool, timeout int64) (any, error) {
	raw, _ := json.Marshal(map[string]any{"consumer": label, "wait": wait, "peek": peek, "timeout_ms": timeout})
	return e.opCheck(ctx, caller, raw)
}

func awaitConsumer(t *testing.T, e *Engine, caller, holder string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, err := checkConsumer(context.Background(), e, caller, "contender", false, false, 0)
		if r, ok := AsRefusal(err); ok && r.Code == "mailbox_busy" {
			if !strings.Contains(r.Message, holder) {
				t.Fatalf("holder missing from refusal: %v", err)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("active check waiter did not hold mailbox lease")
}

func TestConsumerLeaseBlocksDestructiveCheckAndAckButAllowsPeek(t *testing.T) {
	x := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := checkConsumer(ctx, x.e, coord, "host:123", true, false, 5000); result <- err }()
	awaitConsumer(t, x.e, coord, "host:123")
	_, err := checkConsumer(context.Background(), x.e, coord, "second waiter", true, false, 10)
	wantRefusal(t, err, "mailbox_busy")
	m, err := x.st.InsertMessage(store.Message{From: "sender", To: coord, Kind: store.KindNote, Body: "generic fixture"})
	if err != nil {
		t.Fatal(err)
	}
	for _, wait := range []bool{false, true} {
		out, err := checkConsumer(context.Background(), x.e, coord, "observer", wait, true, 100)
		if err != nil || len(out.([]store.Message)) != 1 {
			t.Fatalf("peek: %v %v", out, err)
		}
	}
	ack, ok := x.e.Ops()["ack"]
	if !ok {
		t.Fatal("ack operation missing")
	}
	raw, _ := json.Marshal(map[string]string{"id": m.ID, "consumer": "ack client"})
	_, err = ack(context.Background(), coord, raw)
	wantRefusal(t, err, "mailbox_busy")
	// A different recipient can consume its own mailbox concurrently.
	_, err = checkConsumer(context.Background(), x.e, "other", "other client", false, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := x.st.GetMessage(m.ID)
	if got.ReadAt != 0 {
		t.Fatalf("inspection or blocked ack consumed message: %+v", got)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	wake := x.e.notify.wait()
	out, err := checkConsumer(context.Background(), x.e, coord, "next", false, false, 0)
	if err != nil || len(out.([]store.Message)) != 1 {
		t.Fatalf("check after cancel: %v %v", out, err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("destructive check did not notify receipt waiters")
	}
}

func TestConsumerLeaseReleasedAfterTimeout(t *testing.T) {
	x := newHarness(t)
	out, err := checkConsumer(context.Background(), x.e, coord, "timeout client", true, false, 10)
	if err != nil || len(out.([]store.Message)) != 0 {
		t.Fatalf("timeout: %v %v", out, err)
	}
	_, err = checkConsumer(context.Background(), x.e, coord, "next", false, false, 0)
	if err != nil {
		t.Fatalf("lease retained after timeout: %v", err)
	}
}

func TestConsumerLeaseReleasedAfterSocketDisconnect(t *testing.T) {
	x := newHarness(t)
	dir, err := os.MkdirTemp("/tmp", "hc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "consumer.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { handle(context.Background(), server, x.e.Ops(), x.e.Streams(), t.Logf); close(done) }()
	raw, _ := json.Marshal(map[string]any{"wait": true, "timeout_ms": 5000, "consumer": "disconnect client"})
	req, _ := json.Marshal(rpc.Request{Op: "check", Caller: coord, Args: raw})
	if _, err := client.Write(append(req, '\n')); err != nil {
		t.Fatal(err)
	}
	awaitConsumer(t, x.e, coord, "disconnect client")
	client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request did not stop on disconnect")
	}
	_, err = checkConsumer(context.Background(), x.e, coord, "next", false, false, 0)
	if err != nil {
		t.Fatalf("lease retained on disconnect: %v", err)
	}
}

func TestAckRecipientScopeAndIdempotence(t *testing.T) {
	x := newHarness(t)
	m, err := x.st.InsertMessage(store.Message{From: "sender", To: coord, Kind: store.KindEscalation, Body: "generic fixture"})
	if err != nil {
		t.Fatal(err)
	}
	ack, ok := x.e.Ops()["ack"]
	if !ok {
		t.Fatal("ack operation missing")
	}
	raw, _ := json.Marshal(map[string]string{"id": m.ID, "consumer": "host:456"})
	_, err = ack(context.Background(), "other", raw)
	wantRefusal(t, err, "not_recipient")
	got, _ := x.st.GetMessage(m.ID)
	if got.ReadAt != 0 {
		t.Fatal("wrong recipient consumed message")
	}
	wake := x.e.notify.wait()
	out, err := ack(context.Background(), coord, raw)
	if err != nil {
		t.Fatal(err)
	}
	got = out.(store.Message)
	if got.ReadAt == 0 {
		t.Fatal("ack did not mark read")
	}
	select {
	case <-wake:
	default:
		t.Fatal("ack did not notify receipt waiters")
	}
	first := got.ReadAt
	x.now = x.now.Add(time.Minute)
	out, err = ack(context.Background(), coord, raw)
	if err != nil || out.(store.Message).ReadAt != first {
		t.Fatalf("duplicate ack changed pickup: %v %v", out, err)
	}
	raw = []byte(`{"id":"missing"}`)
	_, err = ack(context.Background(), coord, raw)
	wantRefusal(t, err, "unknown_message")
	_, err = ack(context.Background(), coord, []byte(`{}`))
	wantRefusal(t, err, "bad_args")
}

func TestCanceledConsumerDoesNotPickUp(t *testing.T) {
	x := newHarness(t)
	m, _ := x.st.InsertMessage(store.Message{From: "sender", To: coord, Kind: store.KindNote})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := checkConsumer(ctx, x.e, coord, "canceled", false, false, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled check: %v", err)
	}
	got, _ := x.st.GetMessage(m.ID)
	if got.ReadAt != 0 {
		t.Fatal("canceled check picked up")
	}
	ack, ok := x.e.Ops()["ack"]
	if !ok {
		t.Fatal("ack operation missing")
	}
	raw, _ := json.Marshal(map[string]string{"id": m.ID, "consumer": "canceled ack"})
	_, err = ack(ctx, coord, raw)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ack: %v", err)
	}
	got, _ = x.st.GetMessage(m.ID)
	if got.ReadAt != 0 {
		t.Fatal("canceled ack picked up")
	}
}
