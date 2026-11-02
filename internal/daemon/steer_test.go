package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ellingtonsp/herdr-orch/internal/config"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

func TestSteerSendQueuesWithoutIdleWait(t *testing.T) {
	x := newHarness(t)
	var run store.Run
	x.mustOp(coord, "run.create", RunCreateArgs{Title: "steer"}, &run)
	x.h.addPane("w1:p2", "claude", "working", true) // busy: terminal delivery would block
	x.e.cfg.SteerDir = t.TempDir()
	if _, err := x.st.RegisterWorker(store.Worker{PaneID: "w1:p2", RunID: run.ID, Name: "w", Agent: "claude", AgentArgs: []string{"--settings", SteerSettingsPath(x.e.cfg.SteerDir)}, LaunchKnown: true}); err != nil {
		t.Fatal(err)
	}
	var out []store.Message
	x.mustOp(coord, "send", SendArgs{To: "w1:p2", Kind: store.KindSteer, Subject: "scope", Body: "skip the flaky test"}, &out)
	if len(out) != 1 || out[0].Kind != store.KindSteer || out[0].DeliveryStatus != "queued" {
		t.Fatalf("send: %+v", out)
	}
	if x.h.promptCount("w1:p2") != 0 {
		t.Fatal("steer typed into the pane")
	}
	if _, err := x.op(coord, "send", SendArgs{To: "w1:p2", Kind: store.KindSteer, Body: "x", WaitPickup: true}); err == nil {
		t.Fatal("--wait-pickup accepted for steer")
	}
	if _, err := x.op(coord, "send", SendArgs{To: "human", Kind: store.KindSteer, Body: "x"}); err == nil {
		t.Fatal("steer to a human accepted")
	}

	var pulled []store.Message
	x.mustOp("w1:p2", "steer.pull", SteerPullArgs{}, &pulled)
	if len(pulled) != 1 || pulled[0].Body != "skip the flaky test" || pulled[0].DeliveryStatus != store.SteerInjected {
		t.Fatalf("pull: %+v", pulled)
	}
	x.mustOp("w1:p2", "steer.pull", SteerPullArgs{Ack: []string{out[0].ID}}, &pulled)
	if len(pulled) != 0 {
		t.Fatalf("acked note re-pulled: %+v", pulled)
	}
	got, _ := x.st.GetMessage(out[0].ID)
	if got.DeliveryStatus != store.SteerAcknowledged || got.InjectedAt == 0 || got.AckedAt == 0 {
		t.Fatalf("receipt: %+v", got)
	}
	if _, err := x.op(coord, "steer.pull", SteerPullArgs{}); err != nil {
		t.Fatalf("coordinator pull should be an empty list, got %v", err)
	}
	if _, err := x.op("human", "steer.pull", SteerPullArgs{}); err == nil {
		t.Fatal("non-pane caller accepted")
	}
}

func TestSteerFallsBackToIdleDeliveryWithoutHook(t *testing.T) {
	x := newHarness(t)
	var run store.Run
	x.mustOp(coord, "run.create", RunCreateArgs{Title: "steer"}, &run)
	for _, agent := range []string{"codex", "claude"} {
		pane := "w1:p" + map[string]string{"codex": "3", "claude": "4"}[agent]
		x.h.addPane(pane, agent, "idle", true)
		x.e.cfg.SteerDir = t.TempDir()
		if _, err := x.st.RegisterWorker(store.Worker{PaneID: pane, RunID: run.ID, Name: agent + "-w", Agent: agent, LaunchKnown: true}); err != nil {
			t.Fatal(err)
		}
		var out []store.Message
		x.mustOp(coord, "send", SendArgs{To: pane, Kind: store.KindSteer, Subject: "scope", Body: "hello"}, &out)
		if len(out) != 1 || out[0].Kind != store.KindNote || !strings.HasPrefix(out[0].Subject, "steer: ") {
			t.Fatalf("%s fallback: %+v", agent, out)
		}
	}
}

func TestInstallSteeringIsOptIn(t *testing.T) {
	x := newHarness(t)
	x.e.cfg.SteerDir = filepath.Join(t.TempDir(), "steer")
	x.e.cfg.HorchBin = "/opt/it's/horch"
	x.e.cfg.HerdrSock = "/tmp/herdr.sock"
	base := []string{"--model", "m"}

	if got := x.e.installSteering("claude", base); !slices.Equal(got, base) {
		t.Fatalf("installed with steering off: %v", got)
	}
	x.user = config.Config{Steering: true}
	if got := x.e.installSteering("codex", base); !slices.Equal(got, base) {
		t.Fatalf("installed for codex: %v", got)
	}
	explicit := []string{"--settings", "mine.json"}
	if got := x.e.installSteering("claude", explicit); !slices.Equal(got, explicit) {
		t.Fatalf("overrode operator --settings: %v", got)
	}
	got := x.e.installSteering("claude", base)
	want := SteerSettingsPath(x.e.cfg.SteerDir)
	if !slices.Equal(got, []string{"--model", "m", "--settings", want}) || len(base) != 2 {
		t.Fatalf("args: %v (base %v)", got, base)
	}
	raw, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct{ Type, Command string }
		}
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"PostToolUse", "UserPromptSubmit"} {
		h := s.Hooks[ev]
		if len(h) != 1 || len(h[0].Hooks) != 1 || h[0].Hooks[0].Type != "command" || !strings.HasSuffix(h[0].Hooks[0].Command, `'/opt/it'\''s/horch' hook claude-steer`) || !strings.Contains(h[0].Hooks[0].Command, "HERDR_SOCKET_PATH='/tmp/herdr.sock'") {
			t.Fatalf("%s hook: %+v", ev, h)
		}
	}
}

func TestSteerableRequiresInstalledHook(t *testing.T) {
	x := newHarness(t)
	var run store.Run
	x.mustOp(coord, "run.create", RunCreateArgs{Title: "steer"}, &run)
	x.e.cfg.SteerDir = t.TempDir()
	reg := func(pane string, args []string) {
		x.h.addPane(pane, "claude", "idle", true)
		if _, err := x.st.RegisterWorker(store.Worker{PaneID: pane, RunID: run.ID, Name: "w" + pane[3:], Agent: "claude", AgentArgs: args, LaunchKnown: true}); err != nil {
			t.Fatal(err)
		}
	}
	reg("w1:p2", nil)
	reg("w1:p3", []string{"--settings", SteerSettingsPath(x.e.cfg.SteerDir)})
	if x.e.steerable("w1:p2") || !x.e.steerable("w1:p3") || x.e.steerable("w1:p9") {
		t.Fatal("steerable mismatch")
	}
}
