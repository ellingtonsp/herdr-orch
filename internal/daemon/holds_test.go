package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

func heldTask(t *testing.T) (*harness, store.Dispatch) {
	t.Helper()
	x := newHarness(t)
	var r store.Run
	x.mustOp("w1:coord", "run.create", RunCreateArgs{Title: "acme lifecycle", MaxAttempts: 1}, &r)
	x.h.addPane("w1:worker", "claude", "idle", true)
	var task store.Task
	x.mustOp("w1:coord", "task.create", TaskCreateArgs{Run: r.ID, Title: "ACME-12"}, &task)
	var d store.Dispatch
	x.mustOp("w1:coord", "dispatch", DispatchArgs{Task: task.ID, To: "w1:worker"}, &d)
	x.status(d.PaneID, "working")
	x.status(d.PaneID, "idle")
	return x, d
}

func TestDispatchHoldExpiryAndResume(t *testing.T) {
	for _, until := range []string{"10m", "event:review-complete"} {
		t.Run(until, func(t *testing.T) {
			x, d := heldTask(t)
			x.mustOp("w1:coord", "dispatch.hold", DispatchHoldArgs{Task: d.TaskID, Until: until, Reason: "waiting on review"}, nil)
			x.advance(9 * time.Minute)
			if len(x.inbox("w1:coord")) != 0 {
				t.Fatal("held dispatch escalated")
			}
			// Activity oscillation must not remove the hold.
			x.status(d.PaneID, "working")
			x.status(d.PaneID, "blocked")
			x.advance(time.Minute)
			if until == "event:review-complete" {
				if len(x.inbox("w1:coord")) != 0 {
					t.Fatal("event hold expired")
				}
				x.mustOp("w1:coord", "dispatch.resume", DispatchHoldArgs{Task: d.TaskID}, nil)
			}
			x.status(d.PaneID, "idle")
			x.advance(2 * time.Minute)
			if len(x.inbox("w1:coord")) != 0 {
				t.Fatal("no fresh grace period")
			}
			x.advance(time.Minute)
			if len(x.inbox("w1:coord")) != 1 {
				t.Fatal("expected one reminder after resume")
			}
		})
	}
}

func TestDispatchHoldValidationAndDone(t *testing.T) {
	x, d := heldTask(t)
	for _, until := range []string{"", "0s", "-1m", "event:", "bogus", x.now.Add(-time.Hour).Format(time.RFC3339)} {
		_, err := x.op("w1:coord", "dispatch.hold", DispatchHoldArgs{Task: d.TaskID, Until: until, Reason: "wait"})
		wantRefusal(t, err, "bad_args")
	}
	x.mustOp("w1:coord", "dispatch.hold", DispatchHoldArgs{Task: d.TaskID, Until: x.now.Add(time.Hour).Format(time.RFC3339), Reason: "wait"}, nil)
	x.mustOp(d.PaneID, "send", SendArgs{Task: d.TaskID, Kind: store.KindDone, Body: "done"}, nil)
	if x.task(d.TaskID).Status != store.TaskCompleted {
		t.Fatal("hold blocked explicit completion")
	}
}

func TestInfraFailureRefundsAttempt(t *testing.T) {
	x, d := heldTask(t)
	var st store.Settlement
	x.mustOp("w1:coord", "dispatch.fail", DispatchActArgs{Task: d.TaskID, Reason: "account unavailable", Infra: true}, &st)
	if st.Task.Attempts != 0 || st.Task.Status != store.TaskReady || st.Dispatch.Status != store.DispatchFailed {
		t.Fatalf("infra failure: %+v", st)
	}
	x.mustOp("w1:coord", "dispatch", DispatchArgs{Task: d.TaskID, To: d.PaneID}, &d)
	x.mustOp("w1:coord", "dispatch.fail", DispatchActArgs{Task: d.TaskID, Reason: "implementation failed"}, &st)
	if st.Task.Attempts != 1 || st.Task.Status != store.TaskFailed {
		t.Fatalf("normal failure: %+v", st)
	}
}

type capScreen struct {
	*fakeHerdr
	screen string
}

func (f *capScreen) PaneRead(context.Context, string, int) (string, error) { return f.screen, nil }

func TestUsageCapOneEscalationPersistentHold(t *testing.T) {
	x, d := heldTask(t)
	f := &capScreen{fakeHerdr: x.h, screen: "Usage limit reached · limit resets in 10m"}
	x.e.h = f
	x.advance(5 * time.Second)
	first, _ := x.st.GetDispatch(d.ID)
	if first.HoldUntil == 0 {
		t.Fatal("usage cap did not hold")
	}
	if ms := x.inbox("w1:coord"); len(ms) != 1 || !strings.Contains(ms[0].Subject, "usage cap until") {
		t.Fatalf("messages: %+v", ms)
	}
	// A daemon restart must not forget the deduplication key.
	x.e = NewEngine(x.st, f, x.e.cfg)
	x.e.logf = t.Logf
	x.e.async = func(fn func()) { x.q = append(x.q, fn) }
	x.e.recomputeWatch()
	x.status(d.PaneID, "blocked")
	x.advance(9 * time.Minute)
	if len(x.inbox("w1:coord")) != 0 {
		t.Fatal("cap repeated after restart")
	}
	x.advance(time.Minute)
	got, _ := x.st.GetDispatch(d.ID)
	if got.HoldUntil != 0 || got.HoldEvent != "" {
		t.Fatal("reset did not release hold")
	}
	x.advance(5 * time.Second)
	got, _ = x.st.GetDispatch(d.ID)
	if got.HoldUntil != 0 {
		t.Fatal("stale notice re-held dispatch")
	}
}

func TestUsageCapUnknownResetAndManualHold(t *testing.T) {
	x, d := heldTask(t)
	x.e.h = &capScreen{fakeHerdr: x.h, screen: "You've hit your session limit"}
	x.advance(5 * time.Second)
	got, _ := x.st.GetDispatch(d.ID)
	if got.HoldEvent != "usage-reset" {
		t.Fatalf("unknown reset must require explicit resume: %+v", got)
	}
	x.mustOp("w1:coord", "dispatch.hold", DispatchHoldArgs{Task: d.TaskID, Until: "event:review", Reason: "review pending"}, nil)
	x.advance(24 * time.Hour)
	got, _ = x.st.GetDispatch(d.ID)
	if got.HoldEvent != "review" {
		t.Fatal("cap detection replaced manual hold")
	}
}

func TestUsageCapOutlastsExistingTimedHold(t *testing.T) {
	for _, screen := range []string{"You've hit your session limit", "Usage limit reached · limit resets in 10m"} {
		t.Run(screen, func(t *testing.T) {
			x, d := heldTask(t)
			x.mustOp("w1:coord", "dispatch.hold", DispatchHoldArgs{Task: d.TaskID, Until: "1m", Reason: "review"}, nil)
			x.e.h = &capScreen{fakeHerdr: x.h, screen: screen}
			x.advance(5 * time.Second)
			x.advance(time.Minute)
			got, _ := x.st.GetDispatch(d.ID)
			if !got.Held(x.now.UnixMilli()) || got.HeldBy != store.Daemon {
				t.Fatalf("earlier manual hold lost cap: %+v", got)
			}
			x.mustOp("w1:coord", "dispatch.resume", DispatchHoldArgs{Task: d.TaskID}, nil)
			x.advance(5 * time.Second)
			got, _ = x.st.GetDispatch(d.ID)
			if got.Held(x.now.UnixMilli()) {
				t.Fatal("explicit resume re-held stale notice")
			}
		})
	}
}

func TestHoldExpiryWhileWorkingStartsIdleGraceAtActualIdle(t *testing.T) {
	x, d := heldTask(t)
	x.mustOp("w1:coord", "dispatch.hold", DispatchHoldArgs{Task: d.TaskID, Until: "1m", Reason: "waiting"}, nil)
	x.status(d.PaneID, "working")
	x.advance(2 * time.Minute)
	x.advance(10 * time.Minute)
	x.status(d.PaneID, "idle")
	if len(x.inbox("w1:coord")) != 0 {
		t.Fatal("counted working time as idle after expiry")
	}
	x.advance(3 * time.Minute)
	if len(x.inbox("w1:coord")) != 1 {
		t.Fatal("expected idle reminder")
	}
}

func TestUsageCapPaneExitRefundsAttempt(t *testing.T) {
	x, d := heldTask(t)
	x.e.h = &capScreen{fakeHerdr: x.h, screen: "You've hit your session limit"}
	x.advance(5 * time.Second)
	x.e.paneGone(d.PaneID, "exited")
	task := x.task(d.TaskID)
	if task.Attempts != 0 || task.Status != store.TaskReady {
		t.Fatalf("cap exit consumed attempt: %+v", task)
	}
}

func TestHoldBeforeActivitySuppressesUnobservedAndResumesGrace(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:helper")
	var task store.Task
	x.mustOp(coord, "task.create", TaskCreateArgs{Title: "acme"}, &task)
	x.mustOp(coord, "dispatch", DispatchArgs{Task: task.ID, To: "w1:helper"}, nil)
	x.mustOp(coord, "dispatch.hold", DispatchHoldArgs{Task: task.ID, Until: "event:ready", Reason: "wait"}, nil)
	x.advance(10 * time.Minute)
	if len(x.inbox(coord)) != 0 {
		t.Fatal("unobserved hold escalated")
	}
	x.mustOp(coord, "dispatch.resume", DispatchHoldArgs{Task: task.ID}, nil)
	x.advance(time.Minute)
	if len(x.inbox(coord)) != 0 {
		t.Fatal("resume did not reset unobserved grace")
	}
	x.advance(time.Minute)
	if len(x.inbox(coord)) != 1 {
		t.Fatal("no unobserved reminder after fresh grace")
	}
}
