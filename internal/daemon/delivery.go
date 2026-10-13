package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/herdr"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

// CloseDeliveries stops input jobs before the store is closed.
func (e *Engine) CloseDeliveries() {
	e.deliveryMu.Lock()
	e.deliveryCancel()
	e.deliveryMu.Unlock()
	e.deliveryWG.Wait()
}

func (e *Engine) startDelivery(m store.Message) {
	if !isPane(m.To) {
		return
	} // Human/daemon mail is acknowledged through the mailbox.
	e.deliveryLaunch(func() {
		e.deliveryMu.Lock()
		if e.deliveryCtx.Err() != nil {
			e.deliveryMu.Unlock()
			return
		}
		e.deliveryWG.Add(1)
		lock := e.deliveryPanes[m.To]
		if lock == nil {
			lock = &sync.Mutex{}
			e.deliveryPanes[m.To] = lock
		}
		e.deliveryMu.Unlock()
		defer e.deliveryWG.Done()
		lock.Lock()
		defer lock.Unlock()
		e.deliverMessage(e.deliveryCtx, m)
	})
}

// RecoverDeliveries never blindly replays a write whose result was lost at shutdown.
func (e *Engine) RecoverDeliveries() error {
	ms, err := e.st.PendingDeliveries()
	if err != nil {
		return err
	}
	for _, m := range ms {
		if !isPane(m.To) {
			continue
		}
		if m.DeliveryAttempts == 0 {
			e.startDelivery(m)
		} else {
			e.failDelivery(m, "daemon restarted before pickup was confirmed; inspect the pane before resending")
		}
	}
	return nil
}

func (e *Engine) receipt(id string) (store.Message, error) { return e.st.GetMessage(id) }
func picked(m store.Message) bool                          { return m.PickedUpAt != 0 || m.ReadAt != 0 }

func (e *Engine) failDelivery(m store.Message, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	latest, err := e.receipt(m.ID)
	if err != nil || picked(latest) {
		return
	}
	if err := e.st.Undelivered(m.ID, reason); err != nil {
		e.logf("delivery %s: %v", m.ID, err)
		return
	}
	e.escalateLocked(m.RunID, "message "+m.ID+" undelivered", fmt.Sprintf("Message %s to %s has no confirmed pickup: %s. Check `horch inbox` and inspect the pane before resending.", m.ID, m.To, reason), m.TaskID)
	e.notify.broadcast()
}

func (e *Engine) waitPickup(ctx context.Context, messages []store.Message, timeout time.Duration) ([]store.Message, error) {
	if timeout <= 0 {
		timeout = 2*(e.deliveryReady+e.deliveryPickup) + 5*time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		wake := e.notify.wait()
		all := true
		for i := range messages {
			m, err := e.receipt(messages[i].ID)
			if err != nil {
				return nil, err
			}
			messages[i] = m
			if picked(m) {
				continue
			}
			if m.DeliveryStatus == "undelivered" {
				return nil, refusal("undelivered", "message %s to %s: %s (see horch inbox)", m.ID, m.To, m.DeliveryError)
			}
			all = false
		}
		if all {
			return messages, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, refusal("pickup_timeout", "pickup not yet confirmed; delivery continues, inspect horch inbox before resending")
		case <-wake:
		}
	}
}

func (e *Engine) readyPane(ctx context.Context, pane string) (bool, error) {
	p, err := e.h.PaneGet(ctx, pane)
	if err != nil {
		return false, err
	}
	if p.Agent == nil || *p.Agent == "" {
		return false, nil
	}
	if p.AgentStatus != "idle" && p.AgentStatus != "done" {
		return false, nil
	}
	if p.InteractiveReady != nil && !*p.InteractiveReady {
		return false, nil
	}
	a, err := e.h.AgentGet(ctx, pane)
	if err == nil {
		if a.InteractiveReady != nil && !*a.InteractiveReady {
			return false, nil
		}
		if a.AgentStatus != "" && a.AgentStatus != "idle" && a.AgentStatus != "done" {
			return false, nil
		}
	}
	// Report-agent panes have no named agent record; their idle state is the readiness contract.
	if err != nil && herdr.ErrCode(err) != "agent_not_found" && herdr.ErrCode(err) != "agent_not_ready" {
		return false, err
	}
	return true, nil
}

func (e *Engine) awaitReady(ctx context.Context, m store.Message) error {
	ctx, cancel := context.WithTimeout(ctx, e.deliveryReady)
	defer cancel()
	stable := false
	for {
		r, err := e.receipt(m.ID)
		if err != nil {
			return err
		}
		if picked(r) {
			return nil
		}
		ready, err := e.readyPane(ctx, m.To)
		if err != nil {
			return err
		}
		if ready && stable {
			return nil
		}
		stable = ready
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(e.deliveryPoll):
		}
	}
}

func (e *Engine) workingGeneration(pane string) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.workingSeq[pane]
}

func (e *Engine) awaitPickup(ctx context.Context, m store.Message, seq uint64) bool {
	ctx, cancel := context.WithTimeout(ctx, e.deliveryPickup)
	defer cancel()
	timer := time.NewTimer(e.deliveryPickup)
	defer timer.Stop()
	for {
		r, err := e.receipt(m.ID)
		if err != nil {
			return false
		}
		if picked(r) {
			e.notify.broadcast()
			return true
		}
		p, err := e.h.PaneGet(ctx, m.To)
		if ctx.Err() != nil {
			return false
		}
		if e.workingGeneration(m.To) > seq || (err == nil && p.AgentStatus == "working") {
			if e.st.Delivered(m.ID) != nil {
				return false
			}
			if e.st.PickedUp(m.ID, "working") != nil {
				return false
			}
			e.notify.broadcast()
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		case <-time.After(e.deliveryPoll):
		}
	}
}

// pendingComposer recognizes common Claude and Codex collapsed-paste indicators.
// A retry submits the existing composer instead of pasting a duplicate message.
func pendingComposer(screen, id string) bool {
	s := strings.ToLower(screen)
	return strings.Contains(s, "paste again to expand") || strings.Contains(s, "[pasted text") || strings.Contains(s, "[pasted content") || strings.Contains(s, "[paste #") || strings.Contains(screen, "[horch message "+id+"]")
}

type pendingSubmitter interface {
	SubmitPending(context.Context, string) error
}

func (e *Engine) deliverMessage(ctx context.Context, m store.Message) {
	prompt := fmt.Sprintf("[horch message %s] from %s\n%s\n%s\n\nAcknowledge receipt with: %s ack --id %s", m.ID, m.From, m.Subject, m.Body, e.horchCmd(m.To), m.ID)
	reason := "pickup not observed after two attempts"
	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			return
		}
		if err := e.awaitReady(ctx, m); err != nil {
			reason = "agent did not become interactive and idle: " + err.Error()
			continue
		}
		r, err := e.receipt(m.ID)
		if err != nil {
			return
		}
		if picked(r) {
			e.notify.broadcast()
			return
		}
		seq := e.workingGeneration(m.To)
		attempted, err := e.st.DeliveryAttempt(m.ID)
		if err != nil {
			return
		}
		if !attempted {
			e.notify.broadcast()
			return
		}
		var sendErr error
		if attempt > 0 {
			screen, err := e.h.PaneRead(ctx, m.To, 30)
			if err != nil {
				reason = "cannot inspect composer before retry: " + err.Error()
				break
			}
			if pendingComposer(screen, m.ID) {
				if sender, ok := e.h.(pendingSubmitter); ok {
					sendErr = sender.SubmitPending(ctx, m.To)
				} else {
					sendErr = fmt.Errorf("pending composer cannot be submitted by this transport")
				}
			} else {
				_, sendErr = e.submitDelivery(ctx, m.To, prompt)
			}
		} else {
			_, sendErr = e.submitDelivery(ctx, m.To, prompt)
		}
		if sendErr == nil {
			if err := e.st.Delivered(m.ID); err != nil {
				return
			}
			e.notify.broadcast()
		} else {
			reason = "submission error: " + sendErr.Error()
		}
		// A transport error can follow a successful write. Observe pickup before retrying.
		if e.awaitPickup(ctx, m, seq) {
			return
		}
	}
	if ctx.Err() == nil {
		e.failDelivery(m, reason)
	}
}

// submitDelivery makes one transport attempt. Unlike dispatch startup handshakes,
// retries belong to the receipt job so their count remains visible and bounded.
func (e *Engine) submitDelivery(ctx context.Context, pane, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := e.h.AgentPrompt(ctx, pane, prompt)
	if err == nil {
		return "agent.prompt", nil
	}
	notNamed := herdr.ErrCode(err) == "agent_not_found" || (herdr.ErrCode(err) == "agent_not_ready" && strings.Contains(err.Error(), "not an active named agent"))
	if !notNamed {
		return "", err
	}
	if a, getErr := e.h.AgentGet(ctx, pane); getErr == nil && a.Name != nil && *a.Name != "" {
		return "", err
	}
	ready, readyErr := e.readyPane(ctx, pane)
	if readyErr != nil {
		return "", readyErr
	}
	if !ready {
		return "", fmt.Errorf("agent is no longer idle and ready")
	}
	// Legacy report-agent integrations use line input rather than an agent paste UI.
	flat := strings.Join(strings.Fields(prompt), " ")
	if err := e.h.SendText(ctx, pane, flat); err != nil {
		return "", err
	}
	return "pane.send_text", nil
}
