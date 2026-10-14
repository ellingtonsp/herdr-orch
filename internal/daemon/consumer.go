package daemon

import (
	"context"
	"encoding/json"
	"strings"
)

// consumerLease lasts for one destructive mailbox RPC. A label identifies the
// holder to competing clients; it is diagnostic metadata, not an identity.
type consumerLease struct{ label string }

func (e *Engine) acquireConsumer(ctx context.Context, caller, label, operation string) (func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if holder := e.consumers[caller]; holder != nil {
		return nil, refusal("mailbox_busy", "mailbox %s is being consumed by %s; use check --peek to inspect", caller, holder.label)
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = caller
	}
	lease := &consumerLease{label: operation + " (" + label + ")"}
	if e.consumers == nil {
		e.consumers = map[string]*consumerLease{}
	}
	e.consumers[caller] = lease
	return func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.consumers[caller] == lease {
			delete(e.consumers, caller)
		}
	}, nil
}

type AckArgs struct {
	ID       string `json:"id"`
	Consumer string `json:"consumer,omitempty"`
}

func (e *Engine) opAck(ctx context.Context, caller string, raw json.RawMessage) (any, error) {
	a, err := decode[AckArgs](raw)
	if err != nil {
		return nil, err
	}
	if a.ID == "" {
		return nil, refusal("bad_args", "--id is required")
	}
	release, err := e.acquireConsumer(ctx, caller, a.Consumer, "ack")
	if err != nil {
		return nil, err
	}
	defer release()
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, err := e.st.AckMessage(a.ID, caller)
	if err != nil {
		return nil, err
	}
	e.badgeMail(caller)
	e.notify.broadcast()
	return m, nil
}
