package daemon

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

type LeaseArgs struct {
	Resource string `json:"resource,omitempty"`
	Worker   string `json:"worker,omitempty"`
	TTLMS    *int64 `json:"ttl_ms,omitempty"`
}

func (e *Engine) opLeaseAcquire(_ context.Context, _ string, raw json.RawMessage) (any, error) {
	a, err := decode[LeaseArgs](raw)
	if err != nil {
		return nil, err
	}
	ttl := store.DefaultLeaseTTL
	if a.TTLMS != nil {
		if *a.TTLMS <= 0 || *a.TTLMS > int64((time.Duration(1<<63-1))/time.Millisecond) {
			return nil, refusal("bad_ttl", "TTL must be a positive duration")
		}
		ttl = time.Duration(*a.TTLMS) * time.Millisecond
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w, err := e.st.FindWorker(a.Worker)
	if err != nil {
		return nil, err
	}
	if e.closing[w.PaneID] {
		return nil, refusal("worker_closing", "worker %s is being released", w.PaneID)
	}
	if e.st.WorkerLifecycleHeld(w.PaneID) {
		return nil, refusal("worker_lifecycle_busy", "worker is held for a lifecycle operation")
	}
	l, err := e.st.AcquireLease(a.Resource, w.PaneID, ttl)
	if err == nil {
		e.notify.broadcast()
	}
	return l, err
}
func (e *Engine) opLeaseRelease(_ context.Context, _ string, raw json.RawMessage) (any, error) {
	a, err := decode[LeaseArgs](raw)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w, err := e.st.FindWorker(a.Worker)
	if err != nil {
		return nil, err
	}
	if err := e.st.ReleaseLease(a.Resource, w.PaneID); err != nil {
		return nil, err
	}
	e.notify.broadcast()
	return map[string]bool{"released": true}, nil
}
func (e *Engine) opLeaseList(context.Context, string, json.RawMessage) (any, error) {
	return e.st.ListLeases()
}
