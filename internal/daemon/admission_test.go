package daemon

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"testing"

	"github.com/ellingtonsp/herdr-orch/internal/herdr"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

func TestAdmissionThresholdsAndDefault(t *testing.T) {
	x := newHarness(t)
	run := setupRun(x, "w1:p2")
	task := store.Task{}
	x.mustOp(coord, "task.create", TaskCreateArgs{Title: "task"}, &task)
	calls := 0
	x.e.cfg.SystemMetrics = func(context.Context) (HostMetrics, error) { calls++; return HostMetrics{}, fmt.Errorf("unavailable") }
	x.mustOp(coord, "dispatch", DispatchArgs{Task: task.ID, To: "w1:p2"}, nil)
	if calls != 0 {
		t.Fatal("disabled admission collected metrics")
	}
	// Separate ready tasks and idle panes make each refusal independently observable.
	x = newHarness(t)
	run = setupRun(x, "w1:p2")
	x.mustOp(coord, "task.create", TaskCreateArgs{Title: "task"}, &task)
	max, minimum := 2.0, 20.0
	x.mustOp(coord, "run.update", RunUpdateArgs{Run: run.ID, MaxLoad: &max, MinFreeMem: &minimum}, nil)
	metrics := HostMetrics{Load: 3, FreeMemPercent: 50}
	var collectErr error
	x.e.cfg.SystemMetrics = func(context.Context) (HostMetrics, error) { return metrics, collectErr }
	_, err := x.op(coord, "dispatch", DispatchArgs{Task: task.ID, To: "w1:p2"})
	wantRefusal(t, err, "admission_load")
	metrics = HostMetrics{Load: 1, FreeMemPercent: 19}
	_, err = x.op(coord, "dispatch", DispatchArgs{Task: task.ID, To: "w1:p2"})
	wantRefusal(t, err, "admission_memory")
	collectErr = fmt.Errorf("cannot read capacity")
	_, err = x.op(coord, "dispatch", DispatchArgs{Task: task.ID, To: "w1:p2"})
	wantRefusal(t, err, "admission_unavailable")
	collectErr = nil
	metrics = HostMetrics{Load: math.NaN(), FreeMemPercent: 50}
	_, err = x.op(coord, "dispatch", DispatchArgs{Task: task.ID, To: "w1:p2"})
	wantRefusal(t, err, "admission_unavailable")
	if x.h.promptCount("w1:p2") != 0 || x.task(task.ID).Attempts != 0 {
		t.Fatal("refusal submitted or consumed task")
	}
	metrics = HostMetrics{Load: 2, FreeMemPercent: 20}
	x.mustOp(coord, "dispatch", DispatchArgs{Task: task.ID, To: "w1:p2"}, nil)
	if x.h.promptCount("w1:p2") != 1 {
		t.Fatal("boundary should dispatch")
	}
}

func TestAdmissionAutoDispatchAndNewWorker(t *testing.T) {
	x := newHarness(t)
	run := setupRun(x, "w1:p2")
	max := 1.0
	x.mustOp(coord, "run.update", RunUpdateArgs{Run: run.ID, MaxLoad: &max}, nil)
	x.e.cfg.SystemMetrics = func(context.Context) (HostMetrics, error) { return HostMetrics{Load: 2, FreeMemPercent: 50}, nil }
	on := true
	x.mustOp(coord, "run.update", RunUpdateArgs{AutoDispatch: &on}, nil)
	var task store.Task
	x.mustOp(coord, "task.create", TaskCreateArgs{Title: "task"}, &task)
	if x.h.promptCount("w1:p2") != 0 || task.Status != store.TaskReady {
		t.Fatal("auto dispatch bypassed capacity")
	}
	_, err := x.op(coord, "dispatch", DispatchArgs{Task: task.ID, To: "new:pi"})
	wantRefusal(t, err, "admission_load")
	if x.h.nextPane != 0 {
		t.Fatal("capacity refusal spawned worker")
	}
}

func TestHostMetricsParsing(t *testing.T) {
	linux, err := parseLinuxMetrics("1.50 0.1 0.2 1/99 1", "MemTotal: 1000 kB\nMemFree: 10 kB\nMemAvailable: 250 kB\n")
	if err != nil || linux.Load != 1.5 || linux.FreeMemPercent != 25 {
		t.Fatalf("linux: %+v %v", linux, err)
	}
	vm := "Mach Virtual Memory Statistics: (page size of 4096 bytes)\nPages free: 10.\nPages inactive: 30.\nPages speculative: 10.\nPages purgeable: 10.\n"
	mac, err := parseDarwinMetrics("{ 1.25 2.3 3.4 }", "409600", vm)
	if err != nil || mac.Load != 1.25 || mac.FreeMemPercent != 50 {
		t.Fatalf("darwin: %+v %v", mac, err)
	}
	for _, mem := range []string{"", "MemTotal: 10 kB\nMemAvailable: 20 kB", "MemTotal: NaN kB\nMemAvailable: 2 kB", "MemTotal: 1000 kB\nMemFree: 500 kB", "MemTotal: 1000 B\nMemAvailable: 200 kB"} {
		if _, err := parseLinuxMetrics("1", mem); err == nil {
			t.Fatalf("accepted malformed memory %q", mem)
		}
	}
	for _, load := range []string{"", "garbage", "NaN", "-1"} {
		if _, err := parseLinuxMetrics(load, "MemTotal: 1000 kB\nMemAvailable: 200 kB"); err == nil {
			t.Fatalf("accepted load %q", load)
		}
	}
	if _, err := parseDarwinMetrics("1", "409600", "wrong page header"); err == nil {
		t.Fatal("accepted malformed VM stats")
	}
}

func TestLeaseDispatchRefusalAndClosing(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:p2", "w1:p3")
	x.mustOp(coord, "lease.acquire", LeaseArgs{Resource: "sim:device-1", Worker: "w1:p2"}, nil)
	var task store.Task
	x.mustOp(coord, "task.create", TaskCreateArgs{Title: "task"}, &task)
	_, err := x.op(coord, "dispatch", DispatchArgs{Task: task.ID, To: "w1:p3", Leases: []string{"port:7171", "sim:device-1"}})
	wantRefusal(t, err, "lease_held")
	if x.h.promptCount("w1:p3") != 0 || x.task(task.ID).Attempts != 0 {
		t.Fatal("lease refusal submitted or consumed task")
	}
	x.e.closing["w1:p3"] = true
	_, err = x.op(coord, "lease.acquire", LeaseArgs{Resource: "db:test", Worker: "w1:p3"})
	wantRefusal(t, err, "worker_closing")
	_, err = x.op(coord, "dispatch", DispatchArgs{Task: task.ID, To: "w1:p3"})
	wantRefusal(t, err, "worker_closing")
	delete(x.e.closing, "w1:p3")
	x.mustOp(coord, "lease.release", LeaseArgs{Resource: "sim:device-1", Worker: "w1:p2"}, nil)
	x.mustOp(coord, "dispatch", DispatchArgs{Task: task.ID, To: "w1:p3", Leases: []string{"port:7171", "db:test"}}, nil)
	ls, err := x.st.ListLeases()
	if err != nil || len(ls) != 2 {
		t.Fatalf("leases %+v %v", ls, err)
	}
}

type failingPrompt struct {
	Herdr
	err error
}

func (f failingPrompt) AgentPrompt(context.Context, string, string) error { return f.err }
func TestPromptFailureLeaseRollbackCertainty(t *testing.T) {
	for _, tc := range []struct {
		code      string
		remaining int
	}{{"agent_blocked", 1}, {"agent_prompt_stalled", 2}, {"timeout", 2}} {
		t.Run(tc.code, func(t *testing.T) {
			x := newHarness(t)
			setupRun(x, "w1:p2")
			x.mustOp(coord, "lease.acquire", LeaseArgs{Resource: "port:7171", Worker: "w1:p2"}, nil)
			var task store.Task
			x.mustOp(coord, "task.create", TaskCreateArgs{Title: "task"}, &task)
			x.e.h = failingPrompt{Herdr: x.h, err: &herdr.Error{Code: tc.code, Message: "synthetic failure"}}
			_, err := x.op(coord, "dispatch", DispatchArgs{Task: task.ID, To: "w1:p2", Leases: []string{"db:test", "port:7171"}})
			wantRefusal(t, err, tc.code)
			ls, err := x.st.ListLeases()
			if err != nil || len(ls) != tc.remaining {
				t.Fatalf("leases %+v %v", ls, err)
			}
		})
	}
}

func TestNativeHostMetrics(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("host admission supports macOS and Linux")
	}
	m, err := systemMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if math.IsNaN(m.Load) || math.IsInf(m.Load, 0) || m.Load < 0 || math.IsNaN(m.FreeMemPercent) || m.FreeMemPercent < 0 || m.FreeMemPercent > 100 {
		t.Fatalf("invalid host metrics: %+v", m)
	}
}
