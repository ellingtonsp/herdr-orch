package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

type DispatchHoldArgs struct {
	Task   string `json:"task"`
	Until  string `json:"until,omitempty"`
	Reason string `json:"reason,omitempty"`
}

var holdEventName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]*$`)

func (e *Engine) opDispatchHold(_ context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[DispatchHoldArgs](raw)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.activeDispatchForTask(a.Task)
	if err != nil {
		return nil, err
	}
	if e.st.WorkerLifecycleHeld(d.PaneID) {
		return nil, refusal("worker_lifecycle_busy", "worker is held for a lifecycle operation")
	}
	if strings.TrimSpace(a.Reason) == "" {
		return nil, refusal("bad_args", "--reason is required")
	}
	var until int64
	var event string
	if strings.HasPrefix(a.Until, "event:") {
		event = strings.TrimPrefix(a.Until, "event:")
		if !holdEventName.MatchString(event) {
			return nil, refusal("bad_args", "event requires a name (letters, digits, dots, dashes, underscores or colons)")
		}
	} else if at, err := time.Parse(time.RFC3339, a.Until); err == nil {
		until = at.UnixMilli()
	} else if duration, err := time.ParseDuration(a.Until); err == nil && duration > 0 {
		until = e.st.Now().Add(duration).UnixMilli()
	} else {
		return nil, refusal("bad_args", "--until must be a future RFC3339 timestamp, positive duration, or event:<name>")
	}
	d, err = e.st.HoldDispatch(d.ID, until, event, a.Reason, caller)
	e.notify.broadcast()
	return d, err
}

func (e *Engine) resumeDispatchLocked(d store.Dispatch) (store.Dispatch, error) {
	d, err := e.st.ResumeDispatch(d.ID)
	if err == nil {
		if status := e.status[d.PaneID]; status == "working" || status == "blocked" {
			d, err = e.st.ObserveStatus(d.ID, status)
		}
		if _, ok := e.blocked[d.PaneID]; ok {
			e.blocked[d.PaneID] = e.st.Now()
		}
		delete(e.blockedTold, d.PaneID)
		e.notify.broadcast()
	}
	return d, err
}

func (e *Engine) opDispatchResume(_ context.Context, _ string, raw json.RawMessage) (any, error) {
	a, err := decode[DispatchHoldArgs](raw)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.activeDispatchForTask(a.Task)
	if err != nil {
		return nil, err
	}
	if e.st.WorkerLifecycleHeld(d.PaneID) {
		return nil, refusal("worker_lifecycle_busy", "worker is held for a lifecycle operation")
	}
	return e.resumeDispatchLocked(d)
}

// detectCapLocked runs only for inactive panes, at most once every five seconds.
// The notice is persisted so reconnects do not emit the same escalation again.
func (e *Engine) detectCapLocked(d store.Dispatch) store.Dispatch {
	now := e.st.Now()
	status := e.status[d.PaneID]
	if status != "idle" && status != "done" && status != "blocked" {
		return d
	}
	if last, ok := e.capChecked[d.ID]; ok && now.Sub(last) < 5*time.Second {
		return d
	}
	e.capChecked[d.ID] = now
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	screen, err := e.h.PaneRead(ctx, d.PaneID, 12)
	cancel()
	if err != nil {
		return d
	}
	notice, reset, ok := detectUsageCap(screen, now)
	if !ok {
		// While held, keep the signature even if the pane is temporarily redrawn.
		if d.UsageCapNotice != "" && !d.Held(now.UnixMilli()) {
			_ = e.st.SetUsageCapNotice(d.ID, "")
			d.UsageCapNotice = ""
		}
		return d
	}
	if notice == d.UsageCapNotice {
		return d
	}
	until, event, description := int64(0), "usage-reset", "unknown (explicit resume required)"
	if !reset.IsZero() && reset.After(now) {
		until, event, description = reset.UnixMilli(), "", reset.Format(time.RFC3339)
	}
	to := e.coordinatorOf(d.RunID)
	updated, err := e.st.RecordUsageCap(d.ID, notice, until, event != "", store.Message{
		RunID: d.RunID, From: store.Daemon, To: to, Kind: store.KindEscalation, TaskID: d.TaskID,
		Subject: fmt.Sprintf("worker %s usage cap until %s", d.PaneID, description),
		Body:    fmt.Sprintf("Task %s (dispatch %s) reached an agent usage cap. Hold until %s; inspect with `horch worker read --worker %s`. Use `horch dispatch resume --task %s` after an account switch or an unknown reset.", d.TaskID, d.ID, description, d.PaneID, d.TaskID),
	})
	if err != nil {
		return d
	}
	d = updated
	e.badgeMail(to)
	e.notify.broadcast()
	return d
}

// expireHoldLocked retains a usage cap that outlasted an earlier coordinator hold.
func (e *Engine) expireHoldLocked(d store.Dispatch) (store.Dispatch, error) {
	if d.UsageCapUnknown || d.UsageCapUntil > e.st.Now().UnixMilli() {
		event := ""
		if d.UsageCapUnknown {
			event = "usage-reset"
		}
		return e.st.HoldDispatch(d.ID, d.UsageCapUntil, event, "agent usage cap", store.Daemon)
	}
	return e.resumeDispatchLocked(d)
}
