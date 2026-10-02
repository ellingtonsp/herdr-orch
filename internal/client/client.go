// Package client connects horch and the plugin panes to the daemon, respawning it when
// its socket is dead.
package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/paths"
	"github.com/ellingtonsp/herdr-orch/internal/rpc"
)

type Client struct {
	Paths  paths.Paths
	Caller string
}

// Caller identifies who is talking: $HORCH_AS, else the herdr pane, else "human".
func Caller() string {
	if v := os.Getenv("HORCH_AS"); v != "" {
		return v
	}
	if v := os.Getenv("HERDR_PANE_ID"); v != "" {
		return v
	}
	return "human"
}

func New() (*Client, error) {
	p, err := paths.Resolve()
	if err != nil {
		return nil, err
	}
	return &Client{Paths: p, Caller: Caller()}, nil
}

// Idempotent ops can be re-sent safely when the connection drops mid-request: they only
// read, or (check, ask's reply wait) are long polls whose effect is keyed by the caller.
var idempotent = map[string]bool{
	"ping": true, "check": true, "inbox": true, "board": true,
	"run.current": true, "run.list": true, "run.show": true,
	"task.list": true, "task.show": true, "dispatch.show": true, "gate.list": true,
	"worker.show": true, "worker.list": true, "worker.read": true, "schedule.list": true,
}

// Call runs op, starting the daemon first if its socket is dead. A request that never
// reached the daemon is retried once the daemon is up. A request whose connection dropped
// after it was sent is retried only when op is idempotent; otherwise it returns an
// `outcome_unknown` error instead of risking a duplicate question, message or dispatch.
func (c *Client) Call(ctx context.Context, op string, args, out any) error {
	err := rpc.Call(ctx, c.Paths.Sock, op, c.Caller, args, out)
	lost := errors.Is(err, rpc.ErrLost)
	if !errors.Is(err, rpc.ErrUnavailable) && !lost {
		return err
	}
	if err := c.ensureDaemon(ctx); err != nil {
		return err
	}
	if lost && !idempotent[op] {
		return &rpc.Error{Code: "outcome_unknown", Message: lostAdvice(op)}
	}
	return rpc.Call(ctx, c.Paths.Sock, op, c.Caller, args, out)
}

func lostAdvice(op string) string {
	msg := "the daemon restarted while handling " + op + "; it may or may not have been applied. "
	switch op {
	case "ask":
		return msg + "Do not ask again: run `horch check --wait` to receive the reply to the question already sent (see `horch inbox`)."
	case "send":
		return msg + "Check `horch inbox` before sending again."
	case "dispatch", "dispatch.next":
		return msg + "Check `horch dispatch show --task <id>` before dispatching again."
	}
	return msg + "Check the current state (`horch run show`) before retrying."
}

// ensureDaemon starts the daemon if needed and waits until it answers.
func (c *Client) ensureDaemon(ctx context.Context) error {
	if rpc.Call(ctx, c.Paths.Sock, "ping", c.Caller, nil, nil) == nil {
		return nil
	}
	if err := c.Spawn(); err != nil {
		return fmt.Errorf("daemon not running and could not be started: %w", err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		err := rpc.Call(ctx, c.Paths.Sock, "ping", c.Caller, nil, nil)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon did not come up (see %s): %w", c.Paths.Log, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// DaemonBin finds herdr-orch next to the running executable (symlinks resolved).
func DaemonBin() (string, error) {
	if v := os.Getenv("HORCH_DAEMON_BIN"); v != "" {
		return v, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	bin := filepath.Join(filepath.Dir(exe), "herdr-orch")
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("herdr-orch not found next to %s", exe)
	}
	return bin, nil
}

// Spawn starts a detached daemon for this session.
func (c *Client) Spawn() error {
	bin, err := DaemonBin()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(c.Paths.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(bin, "daemon")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Dir = c.Paths.Dir
	env := os.Environ()
	if os.Getenv("HERDR_SESSION") == "" {
		env = append(env, "HERDR_SESSION="+c.Paths.Session)
	}
	if os.Getenv("HERDR_SOCKET_PATH") == "" {
		env = append(env, "HERDR_SOCKET_PATH="+c.Paths.HerdrSock)
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
