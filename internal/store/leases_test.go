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
