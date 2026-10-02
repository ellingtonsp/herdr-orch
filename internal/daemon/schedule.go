package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

// Action is what a schedule does when it fires.
//
//	{"type":"horch","args":["dispatch","next","--run","r1"],"as":"w1:p1"}
//	{"type":"prompt","to":"reviewer","text":"Re-run the review."}
type Action struct {
	Type string   `json:"type"`
	Args []string `json:"args,omitempty"`
	As   string   `json:"as,omitempty"`
	To   string   `json:"to,omitempty"`
	Text string   `json:"text,omitempty"`
}

func nextRun(spec string, from time.Time) (time.Time, error) {
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return time.Time{}, refusal("bad_cron", "%v", err)
	}
	return sched.Next(from), nil
}

type ScheduleAddArgs struct {
	Cron   string `json:"cron"`
	Action Action `json:"action"`
}

func (e *Engine) opScheduleAdd(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[ScheduleAddArgs](raw)
	if err != nil {
		return nil, err
	}
	switch a.Action.Type {
	case "horch":
		if len(a.Action.Args) == 0 {
			return nil, refusal("bad_action", "horch action needs args")
		}
		if a.Action.As == "" {
			a.Action.As = caller
		}
	case "prompt":
		if a.Action.To == "" || a.Action.Text == "" {
			return nil, refusal("bad_action", "prompt action needs --prompt-to and --prompt")
		}
	default:
		return nil, refusal("bad_action", "action type must be horch or prompt")
	}
	next, err := nextRun(a.Cron, e.st.Now())
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(a.Action)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.st.AddSchedule(a.Cron, b, next.UnixMilli())
}

func (e *Engine) opScheduleList(context.Context, string, json.RawMessage) (any, error) {
	return e.st.ListSchedules()
}

type ScheduleRef struct {
	ID  string `json:"id"`
	Off bool   `json:"off,omitempty"`
}

func (e *Engine) opScheduleRm(_ context.Context, _ string, raw json.RawMessage) (any, error) {
	a, err := decode[ScheduleRef](raw)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return map[string]string{"removed": a.ID}, e.st.RemoveSchedule(a.ID)
}

func (e *Engine) opScheduleEnable(_ context.Context, _ string, raw json.RawMessage) (any, error) {
	a, err := decode[ScheduleRef](raw)
	if err != nil {
		return nil, err
	}
	s, err := e.st.GetSchedule(a.ID)
	if err != nil {
		return nil, err
	}
	next, err := nextRun(s.Cron, e.st.Now())
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.st.SetScheduleEnabled(a.ID, !a.Off, next.UnixMilli()); err != nil {
		return nil, err
	}
	return e.st.GetSchedule(a.ID)
}

// opScheduleRun fires a schedule now and returns its run history.
func (e *Engine) opScheduleRun(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
	a, err := decode[ScheduleRef](raw)
	if err != nil {
		return nil, err
	}
	s, err := e.st.GetSchedule(a.ID)
	if err != nil {
		return nil, err
	}
	e.fire(ctx, s)
	return e.st.ScheduleHistory(a.ID, 10)
}

// RunDueSchedules fires every schedule whose time has come.
func (e *Engine) RunDueSchedules(ctx context.Context) {
	due, err := e.st.DueSchedules()
	if err != nil {
		return
	}
	for _, s := range due {
		e.fire(ctx, s)
	}
}

func (e *Engine) fire(ctx context.Context, s store.Schedule) {
	next, err := nextRun(s.Cron, e.st.Now())
	if err != nil {
		return
	}
	e.mu.Lock()
	runID, err := e.st.StartScheduleRun(s.ID, next.UnixMilli())
	e.mu.Unlock()
	if err != nil {
		e.logf("schedule %s: %v", s.ID, err)
		return
	}
	var a Action
	_ = json.Unmarshal(s.Action, &a)
	out, ok := e.runAction(ctx, a)
	e.mu.Lock()
	_ = e.st.FinishScheduleRun(runID, ok, out)
	e.mu.Unlock()
	e.logf("schedule %s fired ok=%v", s.ID, ok)
}

func (e *Engine) runAction(ctx context.Context, a Action) (string, bool) {
	switch a.Type {
	case "prompt":
		if err := e.h.AgentPrompt(ctx, a.To, a.Text); err != nil {
			return err.Error(), false
		}
		return "prompted " + a.To, true
	case "horch":
		bin := e.cfg.HorchBin
		if bin == "" {
			return "horch binary not found next to the daemon", false
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(cctx, bin, a.Args...)
		env := os.Environ()
		if a.As != "" {
			env = append(env, "HORCH_AS="+a.As)
		}
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err == nil
	}
	return "unknown action " + a.Type, false
}
