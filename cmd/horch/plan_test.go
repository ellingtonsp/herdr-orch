package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/client"
	"github.com/ellingtonsp/herdr-orch/internal/config"
	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

// nopHerdr satisfies the engine's herdr side effects for plan ops (badges, notifications).
type nopHerdr struct{ daemon.Herdr }

func (nopHerdr) PaneTokens(context.Context, string, map[string]*string) error { return nil }
func (nopHerdr) Notify(context.Context, string, string, bool) error           { return nil }

// startDaemon serves a real engine on the socket horch resolves for a throwaway session.
func startDaemon(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{"HERDR_PLUGIN_STATE_DIR", "HERDR_SESSION", "HORCH_AS", "HERDR_PANE_ID", "HORCH_PRINCIPAL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("HORCH_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(dir, "sessions", "horch-cli-test", "herdr.sock"))
	t.Setenv("HORCH_DAEMON_BIN", "/nonexistent") // never spawn a real daemon
	c, err := client.New()
	if err != nil {
		t.Fatal(err)
	}
	cli = c
	st, err := store.Open(c.Paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	cfg := daemon.DefaultConfig()
	cfg.UserConfig = func() (config.Config, error) {
		return config.Config{Owner: config.Owner{Principal: "alice"}, DefaultProject: "acme"}, nil
	}
	e := daemon.NewEngine(st, nopHerdr{}, cfg)
	_ = os.Remove(c.Paths.Sock)
	ln, err := net.Listen("unix", c.Paths.Sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go daemon.Serve(ctx, ln, e)
	t.Cleanup(func() { cancel(); ln.Close(); st.Close(); os.Remove(c.Paths.Sock) })
}

// horch runs one command line as caller and returns its stdout.
func horch(t *testing.T, caller string, args ...string) (string, error) {
	t.Helper()
	cli.Caller = caller
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	err := dispatch(context.Background(), args)
	w.Close()
	os.Stdout = old
	return <-done, err
}

func mustHorch(t *testing.T, caller string, args ...string) string {
	t.Helper()
	out, err := horch(t, caller, args...)
	if err != nil {
		t.Fatalf("horch %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func TestPlanCLI(t *testing.T) {
	startDaemon(t)
	fix := filepath.Join("..", "..", "internal", "planmd", "testdata", "example.plan.md")
	out := mustHorch(t, "w1:p1", "plan", "import", "--day", "2026-03-14", "--file", fix, "--json")
	var imp daemon.PlanImportResult
	if err := json.Unmarshal([]byte(out), &imp); err != nil || !imp.Changed || len(imp.View.Items) != 12 {
		t.Fatalf("import: %s", out)
	}
	if out := mustHorch(t, "w1:p1", "plan", "import", "--day", "2026-03-14", "--file", fix); !strings.Contains(out, "unchanged") {
		t.Fatalf("re-import: %s", out)
	}

	// Follow from the start in the background.
	fctx, fcancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	followed := make(chan []store.PlanEvent, 1)
	go func() {
		var evs []store.PlanEvent
		dec := json.NewDecoder(pr)
		for {
			var e store.PlanEvent
			if dec.Decode(&e) != nil {
				break
			}
			evs = append(evs, e)
			if len(evs) == 4 {
				fcancel()
			}
		}
		followed <- evs
	}()
	// Its own client copy, so the foreground commands can change cli.Caller.
	c := *cli
	c.Caller = "w1:p7"
	go func() {
		_ = c.Stream(fctx, "plan.subscribe", daemon.PlanEventsArgs{}, func(raw json.RawMessage) error {
			_, err := pw.Write(append(raw, '\n'))
			return err
		})
		pw.Close()
	}()

	var v store.PlanView
	_ = json.Unmarshal([]byte(mustHorch(t, "human", "plan", "show", "--json")), &v)
	b1, _ := v.Item("B1")
	ver := itoa(b1.Version)
	mustHorch(t, "w1:p1", "plan", "transition", "--item", "B1", "--state", "settled", "--pr", "#214", "--note", "PR up", "--if-version", ver, "--json")
	if _, err := horch(t, "human", "plan", "item", "update", "--item", "B1", "--title", "stale", "--if-version", ver, "--json"); err == nil || !strings.Contains(err.Error(), "version_conflict") {
		t.Fatalf("stale update: %v", err)
	}
	mustHorch(t, "human", "plan", "item", "hold", "--item", "I1", "--reason", "sim down", "--json")
	out = mustHorch(t, "w1:p9", "plan", "item", "add", "--item", "B2", "--issues", "ACME-51", "--title", "slice b", "--position", "2", "--principal", "alice", "--json")
	var w store.PlanWrite
	_ = json.Unmarshal([]byte(out), &w)
	if e := w.Events[0]; !e.Approval || e.Actor != "w1:p9" || e.Principal != "alice" || w.View.Items[1].ID != "B2" {
		t.Fatalf("add: %+v", w)
	}

	select {
	case evs := <-followed:
		ops := []string{}
		for _, e := range evs {
			ops = append(ops, e.Op)
		}
		if strings.Join(ops, ",") != "import,transition,item.hold,item.add" {
			t.Fatalf("followed %v", ops)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("events --follow stream delivered nothing")
	}

	exp := filepath.Join(t.TempDir(), "final.md")
	mustHorch(t, "human", "plan", "export", "--out", exp)
	md, _ := os.ReadFile(exp)
	if !bytes.Contains(md, []byte("## Event log")) || !bytes.Contains(md, []byte("| B1 | ACME-50 | build | local | claude-sonnet-5-5 | settled | #214 |")) {
		t.Fatalf("export:\n%s", md)
	}
	mustHorch(t, "human", "plan", "import", "--day", "2026-10-03", "--file", exp)
	var a, b store.PlanView
	_ = json.Unmarshal([]byte(mustHorch(t, "human", "plan", "show", "--day", "2026-03-14", "--json")), &a)
	_ = json.Unmarshal([]byte(mustHorch(t, "human", "plan", "show", "--day", "2026-10-03", "--json")), &b)
	if len(a.Items) != len(b.Items) {
		t.Fatalf("round trip: %d vs %d items", len(a.Items), len(b.Items))
	}
	for i := range a.Items {
		x, y := a.Items[i], b.Items[i]
		if x.ID != y.ID || x.State != y.State || x.Held != y.Held || x.Title != y.Title || x.PR != y.PR || x.HeldReason != y.HeldReason {
			t.Fatalf("round trip item %d: %+v vs %+v", i, x, y)
		}
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
