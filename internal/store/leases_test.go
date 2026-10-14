package store

import (
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLeasesPersistenceExpiryAndHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orch.db")
	s := must[*Store](t)(Open(path))
	now := time.Unix(1000, 0)
	s.Now = func() time.Time { return now }
	must[Worker](t)(s.RegisterWorker(Worker{PaneID: "w1:p1", Name: "builder"}))
	must[Worker](t)(s.RegisterWorker(Worker{PaneID: "w1:p2", Name: "reviewer"}))
	must[Lease](t)(s.AcquireLease("port:07171", "w1:p1", time.Minute))
	_, err := s.AcquireLease("port:7171", "w1:p2", time.Minute)
	refusal(t, err, "lease_held")
	if err == nil || !strings.Contains(err.Error(), "builder") {
		t.Fatalf("holder not named: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = must[*Store](t)(Open(path))
	defer s.Close()
	s.Now = func() time.Time { return now }
	if got := must[[]Lease](t)(s.ListLeases()); len(got) != 1 {
		t.Fatalf("restart lost leases: %+v", got)
	}
	now = now.Add(time.Minute)
	must[Lease](t)(s.AcquireLease("port:7171", "w1:p2", time.Minute))
	refusal(t, s.ReleaseLease("port:7171", "w1:p1"), "lease_held")
	if err := s.ReleaseWorkerWithLeases("w1:p2", "verified", ""); err != nil {
		t.Fatal(err)
	}
	if got := must[[]Lease](t)(s.ListLeases()); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestLeaseConcurrentAcquireAndAtomicDispatch(t *testing.T) {
	s := open(t)
	r := must[Run](t)(s.CreateRun("run", ""))
	for _, p := range []string{"w1:p1", "w1:p2"} {
		must[Worker](t)(s.RegisterWorker(Worker{PaneID: p, RunID: r.ID}))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, p := range []string{"w1:p1", "w1:p2"} {
		wg.Add(1)
		go func(p string) { defer wg.Done(); _, err := s.AcquireLease("sim:device-1", p, time.Hour); errs <- err }(p)
	}
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		} else {
			refusal(t, err, "lease_held")
		}
	}
	if successes != 1 {
		t.Fatalf("winners %d", successes)
	}
	ls := must[[]Lease](t)(s.ListLeases())
	holder := ls[0].Worker
	other := "w1:p1"
	if holder == other {
		other = "w1:p2"
	}
	task := must[Task](t)(s.CreateTask(r.ID, "task", "", nil))
	_, err := s.BeginDispatchWithLeases(task.ID, other, "", "", []string{"db:test", "sim:device-1"}, time.Hour)
	refusal(t, err, "lease_held")
	if got := must[[]Lease](t)(s.ListLeases()); len(got) != 1 {
		t.Fatalf("partial acquisition: %+v", got)
	}
	if got := must[Task](t)(s.GetTask(task.ID)); got.Attempts != 0 || got.Status != TaskReady {
		t.Fatal(got)
	}
	must[Lease](t)(s.AcquireLease("port:7171", other, time.Hour))
	d := must[Dispatch](t)(s.BeginDispatchWithLeases(task.ID, other, "", "", []string{"db:test", "port:7171"}, time.Hour))
	must[Settlement](t)(s.FenceUnsent(d.ID, "send failed"))
	ls = must[[]Lease](t)(s.ListLeases())
	if len(ls) != 2 {
		t.Fatalf("rollback must preserve preheld lease: %+v", ls)
	}
	for _, l := range ls {
		if l.Resource == "db:test" {
			t.Fatal("dispatch lease leaked")
		}
	}
}

func TestLeaseValidation(t *testing.T) {
	s := open(t)
	must[Worker](t)(s.RegisterWorker(Worker{PaneID: "w1:p1"}))
	for _, resource := range []string{"", "port:0", "port:65536", "port:no", "db:", "file:x", "db:two words", "sim:a,b"} {
		_, err := s.AcquireLease(resource, "w1:p1", time.Hour)
		refusal(t, err, "bad_resource")
	}
	_, err := s.AcquireLease("db:test", "missing", time.Hour)
	refusal(t, err, "unknown_worker")
	_, err = s.AcquireLease("db:test", "w1:p1", 0)
	refusal(t, err, "bad_ttl")
}

func TestLeasesAcrossSessionsAndRestart(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "leases.db")
	a := must[*Store](t)(OpenWithLeaseRegistry(filepath.Join(dir, "a.db"), registry))
	defer a.Close()
	bpath := filepath.Join(dir, "b.db")
	b := must[*Store](t)(OpenWithLeaseRegistry(bpath, registry))
	defer func() { b.Close() }()
	// Session-local pane IDs and dispatch counters may be identical.
	for _, s := range []*Store{a, b} {
		must[Worker](t)(s.RegisterWorker(Worker{PaneID: "w1:p1", Name: "builder"}))
	}
	must[Lease](t)(a.AcquireLease("sim:device-1", "w1:p1", time.Hour))
	_, err := b.AcquireLease("sim:device-1", "w1:p1", time.Hour)
	refusal(t, err, "lease_held")
	refusal(t, b.ReleaseLease("sim:device-1", "w1:p1"), "lease_held")
	if err := b.ReleaseWorkerWithLeases("w1:p1", "verified", ""); err != nil {
		t.Fatal(err)
	}
	if ls := must[[]Lease](t)(a.ListLeases()); len(ls) != 1 {
		t.Fatalf("release freed other session holder: %+v", ls)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b = must[*Store](t)(OpenWithLeaseRegistry(bpath, registry))
	must[Worker](t)(b.RegisterWorker(Worker{PaneID: "w1:p1", Name: "builder"}))
	_, err = b.AcquireLease("sim:device-1", "w1:p1", time.Hour)
	refusal(t, err, "lease_held")
	if err := a.ReleaseWorkerWithLeases("w1:p1", "verified", ""); err != nil {
		t.Fatal(err)
	}
	must[Lease](t)(b.AcquireLease("sim:device-1", "w1:p1", time.Hour))
}

func TestSharedLeaseConcurrentDispatchAndRollback(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "leases.db")
	a := must[*Store](t)(OpenWithLeaseRegistry(filepath.Join(dir, "a.db"), registry))
	defer a.Close()
	b := must[*Store](t)(OpenWithLeaseRegistry(filepath.Join(dir, "b.db"), registry))
	defer b.Close()
	var tasks []Task
	for _, s := range []*Store{a, b} {
		r := must[Run](t)(s.CreateRun("run", ""))
		must[Worker](t)(s.RegisterWorker(Worker{PaneID: "w1:p1"}))
		tasks = append(tasks, must[Task](t)(s.CreateTask(r.ID, "task", "", nil)))
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i, s := range []*Store{a, b} {
		wg.Add(1)
		go func(s *Store, task Task) {
			defer wg.Done()
			<-start
			_, err := s.BeginDispatchWithLeases(task.ID, "w1:p1", "", "", []string{"db:shared"}, time.Hour)
			errs <- err
		}(s, tasks[i])
	}
	close(start)
	wg.Wait()
	close(errs)
	wins := 0
	for err := range errs {
		if err == nil {
			wins++
		} else {
			refusal(t, err, "lease_held")
		}
	}
	if wins != 1 {
		t.Fatalf("dispatch winners=%d", wins)
	}
	// Failed dispatch on loser must not erase winner's same-numbered dispatch lease.
	winner, loser := a, b
	if must[Task](t)(a.GetTask(tasks[0].ID)).Status == TaskReady {
		winner, loser = b, a
	}
	d := must[Dispatch](t)(loser.BeginDispatchWithLeases("t1", "w1:p1", "", "", []string{"port:7171"}, time.Hour))
	must[Settlement](t)(loser.FenceUnsent(d.ID, "not delivered"))
	ls := must[[]Lease](t)(winner.ListLeases())
	if len(ls) != 1 || ls[0].Resource != "db:shared" {
		t.Fatalf("cross-session fence changed winner: %+v", ls)
	}
}

func TestReleaseAndLeaseRollbackOnDatabaseError(t *testing.T) {
	s := open(t)
	must[Worker](t)(s.RegisterWorker(Worker{PaneID: "w1:p1"}))
	must[Lease](t)(s.AcquireLease("db:test", "w1:p1", time.Hour))
	if _, err := s.db.Exec(`CREATE TRIGGER lease_registry.fail_delete BEFORE DELETE ON leases BEGIN SELECT RAISE(ABORT,'test failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseWorkerWithLeases("w1:p1", "verified", ""); err == nil {
		t.Fatal("database failure reported success")
	}
	w := must[Worker](t)(s.GetWorker("w1:p1"))
	if w.State != WorkerLive {
		t.Fatalf("partially released worker: %+v", w)
	}
	if ls := must[[]Lease](t)(s.ListLeases()); len(ls) != 1 {
		t.Fatal(ls)
	}
}

func TestWorkerFenceRetainsLeasesUntilVerifiedRelease(t *testing.T) {
	s := open(t)
	r := must[Run](t)(s.CreateRun("run", ""))
	must[Worker](t)(s.RegisterWorker(Worker{PaneID: "w1:p1"}))
	task := must[Task](t)(s.CreateTask(r.ID, "task", "", nil))
	d := must[Dispatch](t)(s.BeginDispatchWithLeases(task.ID, "w1:p1", "", "", []string{"sim:device-1"}, time.Hour))
	must[Settlement](t)(s.Fence(d.ID, "worker close requested"))
	if ls := must[[]Lease](t)(s.ListLeases()); len(ls) != 1 {
		t.Fatal("fence released unverified worker lease")
	}
	if err := s.ReleaseWorkerWithLeases("w1:p1", "verified", ""); err != nil {
		t.Fatal(err)
	}
	if ls := must[[]Lease](t)(s.ListLeases()); len(ls) != 0 {
		t.Fatal(ls)
	}
}

func TestAdmissionSettingsValidationAndAtomicUpdate(t *testing.T) {
	s := open(t)
	r := must[Run](t)(s.CreateRun("run", ""))
	max, free := 2.5, 20.0
	got := must[Run](t)(s.UpdateRun(r.ID, RunUpdate{MaxLoad: &max, MinFreeMem: &free}))
	if got.MaxLoad != max || got.MinFreeMem != free {
		t.Fatal(got)
	}
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1)} {
		title := "must roll back"
		_, err := s.UpdateRun(r.ID, RunUpdate{Title: &title, MaxLoad: &bad})
		refusal(t, err, "bad_value")
	}
	for _, bad := range []float64{-1, 101, math.NaN(), math.Inf(1)} {
		_, err := s.UpdateRun(r.ID, RunUpdate{MinFreeMem: &bad})
		refusal(t, err, "bad_value")
	}
	bad, changed := 101.0, 7.0
	_, err := s.UpdateRun(r.ID, RunUpdate{MaxLoad: &changed, MinFreeMem: &bad})
	refusal(t, err, "bad_value")
	got = must[Run](t)(s.GetRun(r.ID))
	if got.Title != "run" || got.MaxLoad != max || got.MinFreeMem != free {
		t.Fatal(got)
	}
	zero := 0.0
	got = must[Run](t)(s.UpdateRun(r.ID, RunUpdate{MaxLoad: &zero, MinFreeMem: &zero}))
	if got.MaxLoad != 0 || got.MinFreeMem != 0 {
		t.Fatal(got)
	}
}

func TestVerifiedHandoffTransfersLeasesAndClearsLifecycle(t *testing.T) {
	s := open(t)
	r := must[Run](t)(s.CreateRun("run", ""))
	for _, w := range []Worker{{PaneID: "w1:source", Name: "source", RunID: r.ID}, {PaneID: "w1:target", Name: "recipient", RunID: r.ID}} {
		must[Worker](t)(s.RegisterWorker(w))
	}
	task := must[Task](t)(s.CreateTask(r.ID, "task", "", nil))
	must[Dispatch](t)(s.BeginDispatchWithLeases(task.ID, "w1:source", "", "", []string{"sim:device-1", "db:test"}, time.Hour))
	before := must[[]Lease](t)(s.ListLeases())
	if err := s.BeginWorkerLifecycle("w1:source", "releasing"); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginWorkerLifecycle("w1:target", "receiving_hand_off"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseWorkerWithLeases("w1:source", "verified", "", "w1:target"); err != nil {
		t.Fatal(err)
	}
	after := must[[]Lease](t)(s.ListLeases())
	if len(after) != len(before) {
		t.Fatal(after)
	}
	for i, l := range after {
		if l.Worker != "w1:target" || l.HolderName != "recipient" || l.ExpiresAt != before[i].ExpiresAt || l.AcquiredAt != before[i].AcquiredAt || l.DispatchID != "" || l.Scope != before[i].Scope {
			t.Fatalf("handoff altered lease contract: %+v -> %+v", before[i], l)
		}
	}
	source := must[Worker](t)(s.GetWorker("w1:source"))
	if source.State != WorkerReleased || source.Lifecycle != "" {
		t.Fatal(source)
	}
	target := must[Worker](t)(s.GetWorker("w1:target"))
	if target.State != WorkerLive || target.Lifecycle != "receiving_hand_off" {
		t.Fatalf("recipient lifecycle changed: %+v", target)
	}
	if err := s.ReleaseLease("sim:device-1", "w1:target"); err != nil {
		t.Fatalf("explicit release blocked during lifecycle: %v", err)
	}
}

func TestLeaseHandoffFailureRollsBackRelease(t *testing.T) {
	s := open(t)
	for _, p := range []string{"w1:source", "w1:target"} {
		must[Worker](t)(s.RegisterWorker(Worker{PaneID: p}))
	}
	must[Lease](t)(s.AcquireLease("port:7171", "w1:source", time.Hour))
	if err := s.BeginWorkerLifecycle("w1:source", "releasing"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER lease_registry.fail_transfer BEFORE UPDATE ON leases BEGIN SELECT RAISE(ABORT,'test failure');END;`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseWorkerWithLeases("w1:source", "verified", "", "w1:target"); err == nil {
		t.Fatal("reported success on registry error")
	}
	source := must[Worker](t)(s.GetWorker("w1:source"))
	if source.State != WorkerLive || source.Lifecycle != "releasing" {
		t.Fatalf("partial release: %+v", source)
	}
	if leases := must[[]Lease](t)(s.ListLeases()); len(leases) != 1 || leases[0].Worker != "w1:source" {
		t.Fatalf("partial handoff: %+v", leases)
	}
}

func TestLifecycleHoldsBlockClaimsButPermitLeaseRelease(t *testing.T) {
	for _, phase := range []string{"releasing", "restart", "receiving_hand_off", "restart_failed"} {
		t.Run(phase, func(t *testing.T) {
			s := open(t)
			r := must[Run](t)(s.CreateRun("run", ""))
			must[Worker](t)(s.RegisterWorker(Worker{PaneID: "w1:worker"}))
			must[Lease](t)(s.AcquireLease("port:7171", "w1:worker", time.Hour))
			if err := s.SetWorkerLifecycle("w1:worker", phase); err != nil {
				t.Fatal(err)
			}
			_, err := s.AcquireLease("db:new", "w1:worker", time.Hour)
			refusal(t, err, "worker_lifecycle_busy")
			task := must[Task](t)(s.CreateTask(r.ID, "task", "", nil))
			_, err = s.BeginDispatchWithLeases(task.ID, "w1:worker", "", "", []string{"db:new"}, time.Hour)
			refusal(t, err, "worker_lifecycle_busy")
			_, err = s.BeginDispatch(task.ID, "w1:worker", "", "")
			refusal(t, err, "worker_lifecycle_busy")
			if err := s.ReleaseLease("port:7171", "w1:worker"); err != nil {
				t.Fatal(err)
			}
			got := must[Task](t)(s.GetTask(task.ID))
			if got.Status != TaskReady || got.Attempts != 0 {
				t.Fatal(got)
			}
		})
	}
}

func TestNormalVerifiedReleaseClearsLifecycleAndLeases(t *testing.T) {
	s := open(t)
	must[Worker](t)(s.RegisterWorker(Worker{PaneID: "w1:worker"}))
	must[Lease](t)(s.AcquireLease("db:test", "w1:worker", time.Hour))
	if err := s.BeginWorkerLifecycle("w1:worker", "releasing"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseWorkerWithLeases("w1:worker", "verified", ""); err != nil {
		t.Fatal(err)
	}
	got := must[Worker](t)(s.GetWorker("w1:worker"))
	if got.State != WorkerReleased || got.Lifecycle != "" {
		t.Fatal(got)
	}
	if ls := must[[]Lease](t)(s.ListLeases()); len(ls) != 0 {
		t.Fatal(ls)
	}
}

func TestRestartReplacementPreservesOriginalLeaseUntilRelease(t *testing.T) {
	s := open(t)
	r := must[Run](t)(s.CreateRun("run", ""))
	must[Worker](t)(s.RegisterWorker(Worker{PaneID: "w1:worker"}))
	task := must[Task](t)(s.CreateTask(r.ID, "task", "", nil))
	old := must[Dispatch](t)(s.BeginDispatchWithLeases(task.ID, "w1:worker", "", "", []string{"db:test"}, time.Hour))
	if err := s.MarkSent(old.ID, "sent"); err != nil {
		t.Fatal(err)
	}
	before := must[[]Lease](t)(s.ListLeases())
	if err := s.BeginWorkerLifecycle("w1:worker", "restart"); err != nil {
		t.Fatal(err)
	}
	next := must[Dispatch](t)(s.ReplaceWorkerDispatch(old.ID))
	must[Settlement](t)(s.FenceUnsent(next.ID, "replacement rejected"))
	after := must[[]Lease](t)(s.ListLeases())
	if len(after) != 1 || after[0].Worker != "w1:worker" || after[0].ExpiresAt != before[0].ExpiresAt {
		t.Fatalf("restart rejection freed old responsibility: %+v", after)
	}
}
