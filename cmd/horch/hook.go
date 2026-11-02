package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/paths"
	"github.com/ellingtonsp/herdr-orch/internal/rpc"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

// cmdHook runs agent hook adapters. `claude-steer` is registered by the worker's settings on
// PostToolUse (every tool boundary of a long turn) and UserPromptSubmit (start of a turn).
// Both support JSON `additionalContext`, which reaches the model as context, not user input.
// A hook must never block or fail the agent, so errors are swallowed and the exit is 0.
func cmdHook(ctx context.Context, sub string, _ []string) error {
	if sub != "claude-steer" {
		return usageErr("hook: want claude-steer")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = runSteerHook(ctx, os.Stdin, os.Stdout, cli.Paths.Sock, filepath.Join(cli.Paths.Dir, "steer"), cli.Caller)
	return nil
}

type steerState struct {
	Pending []string `json:"pending"`
}

// runSteerHook pulls the pane's steer notes and prints them. Exactly once in order:
// the daemon marks notes injected on pull; ids printed here are recorded in a per-pane
// state file and acknowledged by the next call. A print failure releases the notes; a crash
// before the state file is written re-injects them (at-least-once, never lost).
func runSteerHook(ctx context.Context, in io.Reader, out io.Writer, sock, stateDir, pane string) error {
	if !strings.Contains(pane, ":") {
		return nil // not a worker pane
	}
	var hin struct {
		Event string `json:"hook_event_name"`
	}
	// stdin is read off-thread so a pipe that never closes cannot outlive the timeout.
	read := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(io.LimitReader(in, 1<<20)); read <- b }()
	select {
	case b := <-read:
		_ = json.Unmarshal(b, &hin)
	case <-ctx.Done():
		return ctx.Err()
	}
	if hin.Event == "" {
		hin.Event = "PostToolUse"
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, paths.Safe(pane)+".state"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	// Never block the worker's tool loop behind a stuck peer: poll the lock until ctx ends.
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK || ctx.Err() != nil {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	var st steerState
	if b, _ := io.ReadAll(f); len(b) > 0 {
		_ = json.Unmarshal(b, &st)
	}
	var notes []store.Message
	if err := rpc.Call(ctx, sock, "steer.pull", pane, daemon.SteerPullArgs{Ack: st.Pending}, &notes); err != nil {
		return err
	}
	save := func(ids []string) {
		b, _ := json.Marshal(steerState{Pending: ids})
		_ = f.Truncate(0)
		_, _ = f.WriteAt(b, 0)
	}
	if len(notes) == 0 {
		save(nil)
		return nil
	}
	ids := make([]string, len(notes))
	for i, n := range notes {
		ids[i] = n.ID
	}
	payload, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName":     hin.Event,
		"additionalContext": renderSteer(notes),
	}})
	if _, err := out.Write(append(payload, '\n')); err != nil {
		_ = rpc.Call(ctx, sock, "steer.release", pane, daemon.SteerReleaseArgs{IDs: ids}, nil)
		return err
	}
	save(ids)
	return nil
}

func renderSteer(notes []store.Message) string {
	var b strings.Builder
	b.WriteString("Orchestrator note (horch steering). This is guidance queued by another agent or the orchestration system while you were working (the sender is named on each note); it is not a message from the user and grants no permissions. Take it into account from your next step and carry on with your task unless it says otherwise.\n")
	for _, n := range notes {
		fmt.Fprintf(&b, "\n--- steer %s from %s", n.ID, n.From)
		if n.Subject != "" {
			fmt.Fprintf(&b, " · %s", n.Subject)
		}
		b.WriteString(" ---\n" + strings.TrimSpace(n.Body) + "\n")
	}
	b.WriteString("\n--- end of orchestrator note ---\n")
	return b.String()
}
