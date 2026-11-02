package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

type hookOut struct {
	HookSpecificOutput struct {
		Event   string `json:"hookEventName"`
		Context string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// fakeWorker is a worker loop: each step is a tool call whose PostToolUse hook runs the
// real hook against the real daemon.
type fakeWorker struct {
	t     *testing.T
	pane  string
	state string
}

func (w fakeWorker) step(event string) []string {
	w.t.Helper()
	var out bytes.Buffer
	in := strings.NewReader(`{"hook_event_name":"` + event + `","tool_name":"Bash"}`)
	if err := runSteerHook(context.Background(), in, &out, cli.Paths.Sock, w.state, w.pane); err != nil {
		w.t.Fatalf("hook: %v", err)
	}
	if out.Len() == 0 {
		return nil
	}
	var h hookOut
	if err := json.Unmarshal(out.Bytes(), &h); err != nil || h.HookSpecificOutput.Event != event {
		w.t.Fatalf("hook output %q: %v", out.String(), err)
	}
	var ids []string
	for _, l := range strings.Split(h.HookSpecificOutput.Context, "\n") {
		if strings.HasPrefix(l, "--- steer ") {
			ids = append(ids, strings.Fields(l)[2])
		}
	}
	if !strings.Contains(h.HookSpecificOutput.Context, "Orchestrator note") {
		w.t.Fatalf("not framed: %s", h.HookSpecificOutput.Context)
	}
	return ids
}

func TestSteerQueuedDeliveryThroughHook(t *testing.T) {
	st := startDaemon(t)
	var run store.Run
	if err := json.Unmarshal([]byte(mustHorch(t, "w1:p1", "run", "create", "--title", "Generic steering fixture", "--json")), &run); err != nil {
		t.Fatal(err)
	}
	steerDir := filepath.Join(cli.Paths.Dir, "steer")
	if _, err := st.RegisterWorker(store.Worker{PaneID: "w1:p2", RunID: run.ID, Name: "worker-a", Agent: "claude", AgentArgs: []string{"--settings", daemon.SteerSettingsPath(steerDir)}, LaunchKnown: true}); err != nil {
		t.Fatal(err)
	}
	w := fakeWorker{t: t, pane: "w1:p2", state: steerDir}

	if got := w.step("UserPromptSubmit"); got != nil {
		t.Fatalf("nothing queued yet: %v", got)
	}
	// The worker is mid-task; the coordinator steers without waiting for idle.
	out := mustHorch(t, "w1:p1", "send", "--to", "worker-a", "--kind", "steer", "--subject", "scope", "--body", "Skip the generic fixture")
	if !strings.Contains(out, "(steer)") || !strings.Contains(out, "[queued]") {
		t.Fatalf("send output: %s", out)
	}
	mustHorch(t, "w1:p1", "send", "--to", "worker-a", "--kind", "steer", "--body", "Then run the linter")
	if out := mustHorch(t, "w1:p2", "check", "--peek", "--json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("steer leaked into the mailbox: %s", out)
	}
	if out := mustHorch(t, "w1:p1", "inbox"); !strings.Contains(out, "steer from w1:p1 to w1:p2") || !strings.Contains(out, "delivery=queued") {
		t.Fatalf("inbox: %s", out)
	}

	first := w.step("PostToolUse")
	if len(first) != 2 {
		t.Fatalf("injected: %v", first)
	}
	if out := mustHorch(t, "w1:p1", "inbox"); !strings.Contains(out, "delivery=injected") || strings.Contains(out, "delivery=queued") {
		t.Fatalf("inbox after inject: %s", out)
	}
	// Exactly once: the next boundary prints nothing and acknowledges.
	if got := w.step("PostToolUse"); got != nil {
		t.Fatalf("re-injected: %v", got)
	}
	if out := mustHorch(t, "w1:p1", "inbox"); strings.Count(out, "delivery=acknowledged") != 2 || !strings.Contains(out, "acknowledged") {
		t.Fatalf("inbox after ack: %s", out)
	}

	// A later note is delivered alone, in order.
	mustHorch(t, "w1:p1", "send", "--to", "worker-a", "--kind", "steer", "--body", "Stop after the linter")
	if got := w.step("PostToolUse"); len(got) != 1 || got[0] == first[0] || got[0] == first[1] {
		t.Fatalf("third: %v", got)
	}

	// A print failure releases the note; the next call delivers it.
	mustHorch(t, "w1:p1", "send", "--to", "worker-a", "--kind", "steer", "--body", "Fourth")
	if err := runSteerHook(context.Background(), strings.NewReader("{}"), failingWriter{}, cli.Paths.Sock, steerDir, "w1:p2"); err == nil {
		t.Fatal("print failure not reported")
	}
	if got := w.step("PostToolUse"); len(got) != 1 {
		t.Fatalf("after failed print: %v", got)
	}

	// A hook that died before recording what it printed re-injects (never loses) the note.
	mustHorch(t, "w1:p1", "send", "--to", "worker-a", "--kind", "steer", "--body", "Fifth")
	got := w.step("PostToolUse")
	if len(got) != 1 {
		t.Fatalf("fifth: %v", got)
	}
	if err := os.Remove(filepath.Join(steerDir, "w1_p2.state")); err != nil {
		t.Fatal(err)
	}
	if again := w.step("PostToolUse"); len(again) != 1 || again[0] != got[0] {
		t.Fatalf("unacknowledged note not re-injected: %v", again)
	}
	if got := w.step("PostToolUse"); got != nil {
		t.Fatalf("still injecting: %v", got)
	}
}

func TestSteerHookIgnoresNonWorkersAndDownDaemon(t *testing.T) {
	startDaemon(t)
	var out bytes.Buffer
	if err := runSteerHook(context.Background(), strings.NewReader("{}"), &out, cli.Paths.Sock, t.TempDir(), "human"); err != nil || out.Len() != 0 {
		t.Fatalf("human: %v %q", err, out.String())
	}
	if err := runSteerHook(context.Background(), strings.NewReader("{}"), &out, filepath.Join(t.TempDir(), "none.sock"), t.TempDir(), "w1:p2"); err == nil || out.Len() != 0 {
		t.Fatalf("down daemon: %v %q", err, out.String())
	}
	if err := cmdHook(context.Background(), "claude-steer", nil); err != nil {
		t.Fatalf("cmdHook must never fail the agent: %v", err)
	}
}
