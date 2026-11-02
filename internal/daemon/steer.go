package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

// steerSettingsName is the shared Claude Code settings override registering the steering hook.
const steerSettingsName = "claude-steer.json"

// SteerSettingsPath is where the hook settings for steerDir are written.
func SteerSettingsPath(steerDir string) string { return filepath.Join(steerDir, steerSettingsName) }

// SteerPullArgs is the hook's request: Ack lists ids it printed on an earlier call.
type SteerPullArgs struct {
	Ack []string `json:"ack,omitempty"`
}

// SteerReleaseArgs returns notes whose print failed to the queue.
type SteerReleaseArgs struct {
	IDs []string `json:"ids"`
}

func (e *Engine) opSteerPull(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[SteerPullArgs](raw)
	if err != nil {
		return nil, err
	}
	if !isPane(caller) {
		return nil, refusal("bad_caller", "steering is pulled by a worker pane")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ms, err := e.st.SteerPull(caller, a.Ack)
	if err == nil {
		e.notify.broadcast()
	}
	return ms, err
}

func (e *Engine) opSteerRelease(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[SteerReleaseArgs](raw)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	err = e.st.SteerRelease(caller, a.IDs)
	e.notify.broadcast()
	return map[string]bool{"ok": err == nil}, err
}

// steerable reports whether pane is a worker launched with the steering hook installed.
func (e *Engine) steerable(pane string) bool {
	w, err := e.st.GetWorker(pane)
	if err != nil || w.Agent != "claude" || e.cfg.SteerDir == "" {
		return false
	}
	want := SteerSettingsPath(e.cfg.SteerDir)
	for i := 0; i+1 < len(w.AgentArgs); i++ {
		if w.AgentArgs[i] == "--settings" && w.AgentArgs[i+1] == want {
			return true
		}
	}
	return false
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// installSteering writes the hook settings and returns args with `--settings` added.
// It is opt-in (`steering = true`), claude-only, and leaves operator-supplied --settings alone.
func (e *Engine) installSteering(agent string, args []string) []string {
	if agent != "claude" || e.cfg.SteerDir == "" || !e.userConfig().Steering {
		return args
	}
	for _, x := range args {
		if x == "--settings" || strings.HasPrefix(x, "--settings=") {
			return args
		}
	}
	bin := "horch"
	if e.cfg.HorchBin != "" {
		bin = e.cfg.HorchBin
	}
	cmd := shQuote(bin) + " hook claude-steer"
	if e.cfg.HerdrSock != "" {
		cmd = "HERDR_SOCKET_PATH=" + shQuote(e.cfg.HerdrSock) + " " + cmd
	}
	hook := []map[string]any{{"type": "command", "command": cmd, "timeout": 10}}
	settings := map[string]any{"hooks": map[string]any{
		"PostToolUse":      []any{map[string]any{"matcher": "*", "hooks": hook}},
		"UserPromptSubmit": []any{map[string]any{"hooks": hook}},
	}}
	b, _ := json.MarshalIndent(settings, "", "  ")
	path := SteerSettingsPath(e.cfg.SteerDir)
	tmp := path + ".tmp"
	if err := os.MkdirAll(e.cfg.SteerDir, 0o755); err == nil {
		if err = os.WriteFile(tmp, append(b, '\n'), 0o644); err == nil {
			err = os.Rename(tmp, path)
		}
		if err != nil {
			e.logf("steering: %v", err)
			return args
		}
	} else {
		e.logf("steering: %v", err)
		return args
	}
	return append(append([]string(nil), args...), "--settings", path)
}

// sendSteer queues a steering note for each pane recipient. Panes without the hook
// (other agents, workers started before steering was enabled) fall back to ordinary
// idle delivery as a note.
func (e *Engine) sendSteer(run store.Run, caller string, a SendArgs, to []string) (any, error) {
	if a.WaitPickup {
		return nil, refusal("bad_args", "--wait-pickup does not apply to steering; the receipt moves queued → injected → acknowledged (see horch inbox)")
	}
	var out []store.Message
	for _, r := range to {
		if !isPane(r) {
			return nil, refusal("no_recipient", "steering needs a worker pane, not %q", r)
		}
		m := store.Message{RunID: run.ID, From: caller, To: r, Subject: a.Subject, Body: a.Body, Kind: store.KindSteer, TaskID: a.Task, DeliveryStatus: "queued"}
		fallback := !e.steerable(r)
		if fallback {
			m.Kind = store.KindNote
			if m.Subject == "" {
				m.Subject = "steer"
			} else {
				m.Subject = "steer: " + m.Subject
			}
		}
		e.mu.Lock()
		got, err := e.st.InsertMessage(m)
		if err == nil && fallback {
			e.badgeMail(r)
		}
		e.mu.Unlock()
		if err != nil {
			return nil, err
		}
		out = append(out, got)
		if fallback {
			e.startDelivery(got)
		}
	}
	e.notify.broadcast()
	return out, nil
}
