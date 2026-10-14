package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ellingtonsp/herdr-orch/internal/client"
	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/herdr"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

func TestLeaseCLIAndAdmissionFlags(t *testing.T) {
	startLeaseCLIDaemon(t)
	var run store.Run
	if err := json.Unmarshal([]byte(mustHorch(t, "human", "run", "create", "--title", "test", "--json")), &run); err != nil {
		t.Fatal(err)
	}
	// Registration is synthetic and only writes to the throwaway daemon DB.
	for _, pane := range []string{"w1:p2", "w1:p3"} {
		if err := cli.Call(context.Background(), "worker.register", daemon.WorkerRegisterArgs{Pane: pane}, nil); err != nil {
			t.Fatal(err)
		}
	}
	var l store.Lease
	if err := json.Unmarshal([]byte(mustHorch(t, "human", "lease", "acquire", "--resource", "port:7171", "--worker", "w1:p2", "--ttl", "10m", "--json")), &l); err != nil || l.Worker != "w1:p2" {
		t.Fatalf("lease %+v %v", l, err)
	}
	if _, err := horch(t, "human", "lease", "acquire", "--resource", "port:7171", "--worker", "w1:p3"); err == nil {
		t.Fatal("CLI did not refuse held lease")
	}
	var ls []store.Lease
	if err := json.Unmarshal([]byte(mustHorch(t, "human", "lease", "list", "--json")), &ls); err != nil || len(ls) != 1 {
		t.Fatalf("list %+v %v", ls, err)
	}
	mustHorch(t, "human", "lease", "release", "--resource", "port:7171", "--worker", "w1:p2", "--json")
	out := mustHorch(t, "human", "run", "update", "--max-load", "2.5", "--min-free-mem", "20", "--json")
	if err := json.Unmarshal([]byte(out), &run); err != nil || run.MaxLoad != 2.5 || run.MinFreeMem != 20 {
		t.Fatalf("thresholds %s %v", out, err)
	}
	run = store.Run{}
	out = mustHorch(t, "human", "run", "update", "--max-load", "0", "--min-free-mem", "0", "--json")
	if err := json.Unmarshal([]byte(out), &run); err != nil || run.MaxLoad != 0 || run.MinFreeMem != 0 {
		t.Fatalf("disable %s %v", out, err)
	}
	mustHorch(t, "human", "task", "create", "--title", "lease task", "--json")
	mustHorch(t, "human", "dispatch", "--task", "t1", "--to", "w1:p2", "--lease", "db:test,port:7171", "--lease", "sim:device-1", "--json")
	if err := json.Unmarshal([]byte(mustHorch(t, "human", "lease", "list", "--json")), &ls); err != nil || len(ls) != 3 {
		t.Fatalf("declared leases %+v %v", ls, err)
	}
	for _, args := range [][]string{{"lease", "acquire", "--resource", "db:test", "--worker", "w1:p2", "--ttl", "0"}, {"lease", "acquire", "--resource", "port:0", "--worker", "w1:p2"}, {"dispatch", "--task", "t1", "--to", "w1:p2", "--lease", "db:test,,port:7171"}} {
		if _, err := horch(t, "human", args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

type leaseCLIHerdr struct{ nopHerdr }

func (leaseCLIHerdr) PaneRead(context.Context, string, int) (string, error) { return "", nil }

func (leaseCLIHerdr) PaneGet(_ context.Context, id string) (herdr.Pane, error) {
	return herdr.Pane{PaneID: id, AgentStatus: "idle"}, nil
}
func (leaseCLIHerdr) AgentGet(_ context.Context, id string) (herdr.Pane, error) {
	return herdr.Pane{PaneID: id, AgentStatus: "idle"}, nil
}
func (leaseCLIHerdr) AgentPrompt(context.Context, string, string) error { return nil }
func startLeaseCLIDaemon(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{"HERDR_PLUGIN_STATE_DIR", "HERDR_SESSION", "HORCH_AS", "HERDR_PANE_ID"} {
		t.Setenv(k, "")
	}
	t.Setenv("HORCH_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(dir, "sessions", "lease-test", "herdr.sock"))
	t.Setenv("HORCH_DAEMON_BIN", "/nonexistent")
	c, err := client.New()
	if err != nil {
		t.Fatal(err)
	}
	old := cli
	cli = c
	t.Cleanup(func() { cli = old })
	st, err := store.Open(c.Paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	e := daemon.NewEngine(st, leaseCLIHerdr{}, daemon.DefaultConfig())
	ln, err := net.Listen("unix", c.Paths.Sock)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go daemon.Serve(ctx, ln, e)
	t.Cleanup(func() { cancel(); ln.Close(); st.Close(); os.Remove(c.Paths.Sock) })
}

func TestSweepCLIReportsNonDisposableRoot(t *testing.T) {
	startLeaseCLIDaemon(t)
	mustHorch(t, "human", "run", "create", "--title", "acme cleanup", "--json")
	root := t.TempDir()
	if err := cli.Call(context.Background(), "worker.register", daemon.WorkerRegisterArgs{Pane: "w1:old", Worktree: root}, nil); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cli.Paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetWorkerState("w1:old", store.WorkerReleased, "", ""); err != nil {
		t.Fatal(err)
	}
	out := mustHorch(t, "human", "sweep", "--dry-run")
	if !strings.Contains(out, "0 process(es) considered") || !strings.Contains(out, "not a disposable worktree") || !strings.Contains(out, root) {
		t.Fatalf("missing dry-run skip report: %s", out)
	}
	var result daemon.SweepResult
	out = mustHorch(t, "human", "sweep", "--dry-run", "--json")
	if err := json.Unmarshal([]byte(out), &result); err != nil || len(result.Processes) != 0 || len(result.SkippedWorktrees) != 1 || result.SkippedWorktrees[0].Reason != "not a disposable worktree" {
		t.Fatalf("skip JSON: %s %v", out, err)
	}
}
