package daemon

import (
	"context"
	"encoding/json"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

// WebActivity reads across runs without consuming mail or changing selections.
// Status is the latest observation from the daemon's existing herdr subscription.
type WebActivity struct {
	Workers  []WebWorker     `json:"workers"`
	Needs    []store.Message `json:"needs"`
	Gates    []store.Gate    `json:"gates"`
	Messages []store.Message `json:"messages"`
}
type WebWorker struct {
	store.Worker
	LiveStatus string          `json:"live_status"`
	Task       *store.Task     `json:"task,omitempty"`
	Dispatch   *store.Dispatch `json:"dispatch,omitempty"`
}

func (e *Engine) opWebActivity(_ context.Context, _ string, _ json.RawMessage) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := WebActivity{Workers: []WebWorker{}}
	ws, err := e.st.ListWorkers("", false)
	if err != nil {
		return nil, err
	}
	for _, w := range ws {
		row := WebWorker{Worker: w, LiveStatus: e.status[w.PaneID]}
		if d, err := e.st.ActiveDispatchForPane(w.PaneID); err == nil {
			row.Dispatch = &d
			if task, err := e.st.GetTask(d.TaskID); err == nil {
				row.Task = &task
			}
		}
		out.Workers = append(out.Workers, row)
	}
	if out.Needs, err = e.st.OpenAttention(); err != nil {
		return nil, err
	}
	if out.Gates, err = e.st.ListGates("", true); err != nil {
		return nil, err
	}
	if out.Messages, err = e.st.RecentMessages("", 50); err != nil {
		return nil, err
	}
	return out, nil
}
