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

	"github.com/ellingtonsp/herdr-orch/internal/config"
	"github.com/ellingtonsp/herdr-orch/internal/rpc"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

func planHarness(t *testing.T) *harness {
	t.Helper()
	x := newHarness(t)
	x.user = config.Config{Owner: config.Owner{Principal: "alice"}, DefaultProject: "acme"}
	md, err := os.ReadFile(filepath.Join("..", "planmd", "testdata", "example.plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	var r PlanImportResult
	x.mustOp(coord, "plan.import", PlanImportArgs{PlanRef: PlanRef{Day: "2026-03-14"}, Markdown: string(md), Ref: "1a2b3c4"}, &r)
	if !r.Changed || r.View.Plan.ID != "acme/2026-03-14" || r.View.Plan.Coordinator != coord || len(r.View.Items) != 12 {
		t.Fatalf("import = %+v", r.View.Plan)
	}
	// Re-importing the same file is a no-op.
	x.mustOp(coord, "plan.import", PlanImportArgs{PlanRef: PlanRef{Day: "2026-03-14"}, Markdown: string(md)}, &r)
	if r.Changed {
		t.Fatal("re-import changed the plan")
	}
	return x
}

func lastEvent(t *testing.T, raw json.RawMessage) store.PlanEvent {
	t.Helper()
	var w store.PlanWrite
	if err := json.Unmarshal(raw, &w); err != nil || len(w.Events) != 1 {
		t.Fatalf("write = %s", raw)
	}
	return w.Events[0]
}

func TestPlanIdentityAndApproval(t *testing.T) {
	x := planHarness(t)
	setupRun(x, "w1:p2") // w1:p2 is a registered worker

	st := "settled"
	// Orchestrator transition: not an approval.
	raw, err := x.op(coord, "plan.transition", PlanItemArgs{Item: "B1", Patch: store.ItemPatch{State: &st}, Note: "PR up"})
	if err != nil {
		t.Fatal(err)
	}
	if e := lastEvent(t, raw); e.ActorKind != store.ActorOrchestrator || e.Approval || e.Actor != coord {
		t.Fatalf("orchestrator event = %+v", e)
	}
	// A worker pane is a worker.
	pr := "#3999"
	raw, _ = x.op("w1:p2", "plan.item.update", PlanItemArgs{Item: "I1", Patch: store.ItemPatch{PR: &pr}})
	if e := lastEvent(t, raw); e.ActorKind != store.ActorWorker || e.Approval {
		t.Fatalf("worker event = %+v", e)
	}
	// A terminal outside herdr acts for the owner: an approval.
	raw, _ = x.op("human", "plan.item.hold", PlanItemArgs{Item: "I1r", Reason: "wait for sim"})
	if e := lastEvent(t, raw); e.ActorKind != store.ActorHuman || !e.Approval || e.Principal != "alice" {
		t.Fatalf("owner event = %+v", e)
	}
	// The owner acting from a pane names the principal.
	raw, _ = x.op("w1:p9", "plan.item.move", PlanItemArgs{PlanRef: PlanRef{Principal: "alice"}, Item: "W1", Position: 1})
	if e := lastEvent(t, raw); e.ActorKind != store.ActorHuman || !e.Approval || e.Actor != "w1:p9" {
		t.Fatalf("owner-from-pane event = %+v", e)
	}
	// Another human is recorded but is not an approval.
	raw, _ = x.op("human", "plan.item.release", PlanItemArgs{PlanRef: PlanRef{Principal: "guest"}, Item: "I1r"})
	if e := lastEvent(t, raw); e.ActorKind != store.ActorHuman || e.Approval || e.Principal != "guest" {
		t.Fatalf("guest event = %+v", e)
	}

	// Human edits reach the plan's coordinator; orchestrator and worker writes do not.
	var notes []string
	for _, m := range x.inbox(coord) {
		if strings.HasPrefix(m.Subject, "plan replanned by") {
			notes = append(notes, m.Subject)
		}
	}
	if len(notes) != 3 || !strings.Contains(notes[0], "plan replanned by alice: item.hold I1r") || !strings.Contains(notes[2], "guest") {
		t.Fatalf("coordinator notes = %q", notes)
	}
}

func TestPlanVersionConflictOverRPC(t *testing.T) {
	x := planHarness(t)
	var v store.PlanView
	x.mustOp("human", "plan.show", PlanRef{}, &v)
	b1, _ := v.Item("B1")
	title := "renamed"
	x.mustOp("human", "plan.item.update", PlanItemArgs{Item: "B1", IfVersion: b1.Version, Patch: store.ItemPatch{Title: &title}}, nil)
	stale := "stale"
	_, err := x.op("human", "plan.item.update", PlanItemArgs{Item: "B1", IfVersion: b1.Version, Patch: store.ItemPatch{Title: &stale}})
	wantRefusal(t, err, "version_conflict")
	_, err = x.op("human", "plan.item.add", PlanItemArgs{Item: "B9", IfPlanVersion: v.Plan.Version})
	wantRefusal(t, err, "version_conflict")
}

func TestPlanActorFieldsAreDerivedByDaemon(t *testing.T) {
	x := planHarness(t)
	setupRun(x, "w1:p2")
	// An ordinary client can select its caller/principal in the existing local
	// trust model, but cannot send an independent actor kind or approval flag.
	raw, err := x.op("w1:p2", "plan.item.update", map[string]any{
		"item": "B1", "patch": map[string]any{"title": "worker change"},
		"actor": "human", "actor_kind": store.ActorHuman, "approval": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ev := lastEvent(t, raw)
	if ev.Actor != "w1:p2" || ev.ActorKind != store.ActorWorker || ev.Approval || ev.Principal != "" {
		t.Fatalf("client supplied actor fields were trusted: %+v", ev)
	}
}

func TestRejectedConfigCannotGrantApprovalOrBePublished(t *testing.T) {
	x := planHarness(t)
	x.e.cfg.UserConfig = func() (config.Config, error) {
		return config.Config{
			Owner:          config.Owner{Principal: "alice", Name: "rejected-private-value"},
			DefaultProject: "acme",
		}, errors.New("invalid config")
	}
	// Explicit project selection still finds the existing plan, but a rejected
	// owner identity must never turn a named principal into an approval.
	raw, err := x.op("human", "plan.item.hold", PlanItemArgs{
		PlanRef: PlanRef{Project: "acme", Principal: "alice"}, Item: "B1", Reason: "hold",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev := lastEvent(t, raw); ev.Approval || ev.Principal != "alice" {
		t.Fatalf("rejected config granted approval: %+v", ev)
	}
	raw, err = x.op("human", "plan.config", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "alice") || strings.Contains(string(raw), "rejected-private-value") || strings.Contains(string(raw), "acme") {
		t.Fatalf("rejected config was published: %s", raw)
	}
}

func TestPlanEventsLongPollWakes(t *testing.T) {
	x := planHarness(t)
	var evs []store.PlanEvent
	x.mustOp("human", "plan.events", PlanEventsArgs{}, &evs)
	since := evs[len(evs)-1].Seq
	got := make(chan []store.PlanEvent, 1)
	go func() {
		var out []store.PlanEvent
		b, _ := json.Marshal(PlanEventsArgs{Since: since, Wait: true, TimeoutMS: 5000})
		res, _ := x.e.Ops()["plan.events"](context.Background(), "human", b)
		raw, _ := json.Marshal(res)
		_ = json.Unmarshal(raw, &out)
		got <- out
	}()
	time.Sleep(50 * time.Millisecond)
	x.mustOp("human", "plan.item.hold", PlanItemArgs{Item: "B1", Reason: "x"}, nil)
	select {
	case out := <-got:
		if len(out) != 1 || out[0].Op != "item.hold" {
			t.Fatalf("events = %+v", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("events --wait did not wake")
	}
}

// TestPlanSubscribeDeliversOverSocket covers `horch plan events --follow`: the backlog,
// then live pushes, over the real wire protocol.
func TestPlanSubscribeDeliversOverSocket(t *testing.T) {
	x := planHarness(t)
	sock := filepath.Join(t.TempDir(), "s.sock")
	if len(sock) > 100 {
		sock = filepath.Join(os.TempDir(), "horch-test-plan.sock")
		_ = os.Remove(sock)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer ln.Close()
	go serve(ctx, ln, x.e.Ops(), x.e.Streams(), t.Logf)

	got := make(chan store.PlanEvent, 16)
	sctx, scancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- rpc.Stream(sctx, sock, "plan.subscribe", "w1:p5", PlanEventsArgs{}, func(raw json.RawMessage) error {
			var e store.PlanEvent
			if err := json.Unmarshal(raw, &e); err != nil {
				return err
			}
			got <- e
			return nil
		})
	}()
	next := func() store.PlanEvent {
		t.Helper()
		select {
		case e := <-got:
			return e
		case <-time.After(3 * time.Second):
			t.Fatal("no event delivered")
		}
		return store.PlanEvent{}
	}
	if e := next(); e.Op != "import" {
		t.Fatalf("backlog = %+v", e)
	}
	// Live writes from two identities arrive in order, pushed (no polling).
	st := "merged"
	x.mustOp(coord, "plan.transition", PlanItemArgs{Item: "B1", Patch: store.ItemPatch{State: &st}}, nil)
	x.mustOp("human", "plan.item.hold", PlanItemArgs{Item: "I1", Reason: "sim"}, nil)
	a, b := next(), next()
	if a.Op != "transition" || a.Item != "B1" || a.ActorKind != store.ActorOrchestrator || b.Op != "item.hold" || !b.Approval || b.Seq <= a.Seq {
		t.Fatalf("live = %+v / %+v", a, b)
	}
	// A second subscriber resuming after a's seq gets only b.
	var resumed []store.PlanEvent
	rctx, rcancel := context.WithCancel(ctx)
	go func() {
		_ = rpc.Stream(rctx, sock, "plan.subscribe", "human", PlanEventsArgs{Since: a.Seq}, func(raw json.RawMessage) error {
			var e store.PlanEvent
			_ = json.Unmarshal(raw, &e)
			resumed = append(resumed, e)
			rcancel()
			return nil
		})
	}()
	<-rctx.Done()
	if len(resumed) != 1 || resumed[0].Seq != b.Seq {
		t.Fatalf("resumed = %+v", resumed)
	}
	scancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not end when the client hung up")
	}
	// The daemon dropped the subscriber.
	deadline := time.Now().Add(2 * time.Second)
	for {
		x.e.hub.mu.Lock()
		n := len(x.e.hub.subs)
		x.e.hub.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d subscribers left", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPlanExportFinalize(t *testing.T) {
	x := planHarness(t)
	x.mustOp("human", "plan.item.hold", PlanItemArgs{Item: "B1", Reason: "slot"}, nil)
	var out PlanExport
	x.mustOp("human", "plan.export", PlanExportArgs{Finalize: true}, &out)
	if out.Plan.Status != store.PlanFinal || !strings.Contains(out.Markdown, "## Event log") || !strings.Contains(out.Markdown, "| B1 | slot |") && !strings.Contains(out.Markdown, "slot") {
		t.Fatalf("export = %s", out.Markdown)
	}
	_, err := x.op("human", "plan.item.release", PlanItemArgs{Item: "B1"})
	wantRefusal(t, err, "plan_final")
}

func TestPlanHubSlowSubscriberDoesNotBlockOthers(t *testing.T) {
	var h planHub
	slow, cancelSlow := h.subscribe()
	defer cancelSlow()
	fast, cancelFast := h.subscribe()
	defer cancelFast()
	for seq := int64(1); seq <= 300; seq++ {
		h.publish([]store.PlanEvent{{Seq: seq}})
		select {
		case ev, ok := <-fast:
			if !ok || ev.Seq != seq {
				t.Fatalf("healthy subscriber got %+v, open=%v, want %d", ev, ok, seq)
			}
		default:
			t.Fatal("healthy subscriber lost an event")
		}
	}
	count := 0
	for range slow {
		count++
	}
	if count != 256 {
		t.Fatalf("lagging subscriber buffered %d events", count)
	}
	h.mu.Lock()
	remaining := len(h.subs)
	h.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("hub retained %d subscribers", remaining)
	}
}

// The production deadline is 10 seconds. Shorten it on this test connection so
// the test proves a writer blocked by its peer is reaped without a long sleep.
type shortPlanWriteDeadline struct{ net.Conn }

func (c shortPlanWriteDeadline) SetWriteDeadline(time.Time) error {
	return c.Conn.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
}

func TestPlanSubscribeBlockedWriterIsReaped(t *testing.T) {
	x := planHarness(t)
	server, peer := net.Pipe()
	defer peer.Close()
	done := make(chan struct{})
	go func() {
		handle(context.Background(), shortPlanWriteDeadline{server}, x.e.Ops(), x.e.Streams(), t.Logf)
		close(done)
	}()
	request, _ := json.Marshal(rpc.Request{Op: "plan.subscribe", Caller: "human", Args: json.RawMessage(`{"all":true}`)})
	if _, err := peer.Write(append(request, '\n')); err != nil {
		t.Fatal(err)
	}
	// Never read the response: the import backlog blocks the server's first write.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked stream survived its write deadline")
	}
	x.e.hub.mu.Lock()
	remaining := len(x.e.hub.subs)
	x.e.hub.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("blocked writer retained %d subscriptions", remaining)
	}
}

func TestPlanSubscribeRestartReplaysFromCursor(t *testing.T) {
	x := planHarness(t)
	// /tmp keeps the Unix socket path within macOS's length limit.
	dir, err := os.MkdirTemp("", "horch-resume-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	start := func(e *Engine) func() {
		t.Helper()
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		go Serve(ctx, ln, e)
		return func() { cancel(); ln.Close() }
	}
	stop := start(x.e)
	got := make(chan store.PlanEvent, 2)
	streamDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		streamDone <- rpc.Stream(ctx, sock, "plan.subscribe", "human", PlanEventsArgs{}, func(raw json.RawMessage) error {
			var ev store.PlanEvent
			if err := json.Unmarshal(raw, &ev); err != nil {
				return err
			}
			got <- ev
			return nil
		})
	}()
	var cursor int64
	select {
	case ev := <-got:
		cursor = ev.Seq
	case <-ctx.Done():
		stop()
		t.Fatal("initial stream did not deliver the backlog")
	}
	stop()
	select {
	case err := <-streamDone:
		if !errors.Is(err, rpc.ErrLost) {
			t.Fatalf("restart yielded %v, want ErrLost", err)
		}
	case <-ctx.Done():
		t.Fatal("shutdown did not disconnect the subscriber")
	}
	// A fresh engine has no prior subscribers. Events written while disconnected
	// must still be replayed once the client reconnects to the new socket.
	x.e = NewEngine(x.st, x.h, x.e.cfg)
	x.e.logf = t.Logf
	x.mustOp("human", "plan.item.hold", PlanItemArgs{Item: "B1", Reason: "during restart"}, nil)
	stop = start(x.e)
	defer stop()
	finished := errors.New("received resumed event")
	var resumed store.PlanEvent
	err = rpc.Stream(ctx, sock, "plan.subscribe", "human", PlanEventsArgs{Since: cursor}, func(raw json.RawMessage) error {
		if err := json.Unmarshal(raw, &resumed); err != nil {
			return err
		}
		return finished
	})
	if !errors.Is(err, finished) || resumed.Seq <= cursor || resumed.Item != "B1" || resumed.Note != "during restart" {
		t.Fatalf("resume = %+v, error %v", resumed, err)
	}
}
