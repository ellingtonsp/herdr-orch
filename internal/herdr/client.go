// Package herdr talks to the herdr server over its unix socket.
//
// Wire format (protocol 22): newline-delimited JSON, one request per connection.
// events.subscribe keeps its connection open and streams one event per line.
package herdr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"
)

// SupportedProtocols lists the herdr API protocols this plugin was built against.
var SupportedProtocols = []int{22}

// Error is an error response from herdr.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Client calls the herdr API.
type Client struct {
	Socket  string
	Timeout time.Duration
	seq     atomic.Uint64
}

func New(socket string) *Client {
	if socket == "" {
		socket = os.Getenv("HERDR_SOCKET_PATH")
	}
	return &Client{Socket: socket, Timeout: 30 * time.Second}
}

// Call sends one request and decodes the result into out (if non-nil).
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	if params == nil {
		params = map[string]any{}
	}
	if _, ok := ctx.Deadline(); !ok && c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stopCancel()
	id := fmt.Sprintf("horch-%d", c.seq.Add(1))
	req, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return err
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return fmt.Errorf("%s: %w", method, err)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("%s: decode: %w", method, err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out != nil {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
}

// ErrCode returns the herdr error code of err, or "".
func ErrCode(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ""
}

type Pong struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
}

func (c *Client) Ping(ctx context.Context) (Pong, error) {
	var p Pong
	err := c.Call(ctx, "ping", nil, &p)
	return p, err
}

// Pane is the subset of herdr's pane info we use.
type Pane struct {
	PaneID           string  `json:"pane_id"`
	WorkspaceID      string  `json:"workspace_id"`
	TabID            string  `json:"tab_id"`
	Agent            *string `json:"agent"`
	AgentStatus      string  `json:"agent_status"`
	Name             *string `json:"name"`
	InteractiveReady *bool   `json:"interactive_ready"` // set by agent.get once the agent accepts prompts
	Cwd              *string `json:"cwd"`
	ForegroundCwd    *string `json:"foreground_cwd"`
}

func (c *Client) PaneGet(ctx context.Context, paneID string) (Pane, error) {
	var r struct {
		Pane Pane `json:"pane"`
	}
	err := c.Call(ctx, "pane.get", map[string]any{"pane_id": paneID}, &r)
	if err == nil && r.Pane.WorkspaceID == "" {
		return Pane{}, fmt.Errorf("pane.get: pane %s has no workspace; choose a shell pane in a workspace", paneID)
	}
	return r.Pane, err
}

func (c *Client) AgentGet(ctx context.Context, target string) (Pane, error) {
	var r struct {
		Agent Pane `json:"agent"`
	}
	err := c.Call(ctx, "agent.get", map[string]any{"target": target}, &r)
	return r.Agent, err
}

type ProcessInfo struct {
	ForegroundProcessGroupID int `json:"foreground_process_group_id"`
	ShellPID                 int `json:"shell_pid"`
	ForegroundProcesses      []struct {
		PID  int    `json:"pid"`
		Name string `json:"name"`
	} `json:"foreground_processes"`
}

// AvailableShell requires a shell with no other process in the foreground.
// Widgets and incomplete process information do not establish an available shell.
func (p ProcessInfo) AvailableShell() bool {
	if p.ShellPID <= 0 || (p.ForegroundProcessGroupID != 0 && p.ForegroundProcessGroupID != p.ShellPID) {
		return false
	}
	for _, f := range p.ForegroundProcesses {
		if f.PID != p.ShellPID {
			return false
		}
	}
	return p.ForegroundProcessGroupID == p.ShellPID || len(p.ForegroundProcesses) > 0
}

// PIDs returns the shell pid and every foreground pid, deduplicated.
func (p ProcessInfo) PIDs() []int {
	seen := map[int]bool{}
	var out []int
	add := func(pid int) {
		if pid > 0 && !seen[pid] {
			seen[pid] = true
			out = append(out, pid)
		}
	}
	add(p.ShellPID)
	for _, f := range p.ForegroundProcesses {
		add(f.PID)
	}
	return out
}

func (c *Client) ProcessInfo(ctx context.Context, paneID string) (ProcessInfo, error) {
	var r struct {
		ProcessInfo ProcessInfo `json:"process_info"`
	}
	err := c.Call(ctx, "pane.process_info", map[string]any{"pane_id": paneID}, &r)
	return r.ProcessInfo, err
}

func (c *Client) PaneRead(ctx context.Context, paneID string, lines int) (string, error) {
	var r struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	params := map[string]any{"pane_id": paneID, "source": "recent_unwrapped", "format": "text"}
	if lines > 0 {
		params["lines"] = lines
	}
	err := c.Call(ctx, "pane.read", params, &r)
	return r.Read.Text, err
}

func (c *Client) PaneClose(ctx context.Context, paneID string) error {
	return c.Call(ctx, "pane.close", map[string]any{"pane_id": paneID}, nil)
}

// AgentPrompt submits text to a herdr-recognised agent without waiting.
func (c *Client) AgentPrompt(ctx context.Context, target, text string) error {
	return c.Call(ctx, "agent.prompt", map[string]any{"target": target, "text": text}, nil)
}

// SendText types text into a pane and presses Enter (fallback for agents herdr did not start).
func (c *Client) SendText(ctx context.Context, paneID, text string) error {
	if err := c.Call(ctx, "pane.send_text", map[string]any{"pane_id": paneID, "text": text}, nil); err != nil {
		return err
	}
	// Give paste buffering UIs a boundary before the submit key.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(350 * time.Millisecond):
	}
	return c.Call(ctx, "pane.send_keys", map[string]any{"pane_id": paneID, "keys": []string{"enter"}}, nil)
}

// SubmitPending retries only Enter, retaining the existing collapsed paste.
func (c *Client) SubmitPending(ctx context.Context, paneID string) error {
	p, err := c.PaneGet(ctx, paneID)
	if err != nil {
		return err
	}
	if p.Agent == nil || *p.Agent == "" || (p.AgentStatus != "idle" && p.AgentStatus != "done") || (p.InteractiveReady != nil && !*p.InteractiveReady) {
		return fmt.Errorf("pane %s no longer has an idle agent", paneID)
	}
	a, err := c.AgentGet(ctx, paneID)
	if err == nil {
		if a.InteractiveReady != nil && !*a.InteractiveReady {
			return fmt.Errorf("agent is not interactive")
		}
		if a.AgentStatus != "" && a.AgentStatus != "idle" && a.AgentStatus != "done" {
			return fmt.Errorf("agent is no longer idle")
		}
	}
	if err != nil && ErrCode(err) != "agent_not_found" && ErrCode(err) != "agent_not_ready" {
		return err
	}
	return c.Call(ctx, "pane.send_keys", map[string]any{"pane_id": paneID, "keys": []string{"enter"}}, nil)
}

func (c *Client) Notify(ctx context.Context, title, body string, urgent bool) error {
	sound := "done"
	if urgent {
		sound = "request"
	}
	return c.Call(ctx, "notification.show", map[string]any{"title": title, "body": body, "sound": sound}, nil)
}

// PaneTokens sets (or clears, with nil values) sidebar metadata tokens on a pane.
func (c *Client) PaneTokens(ctx context.Context, paneID string, tokens map[string]*string) error {
	return c.Call(ctx, "pane.report_metadata", map[string]any{
		"pane_id": paneID, "source": "herdr-orch", "tokens": tokens,
	}, nil)
}

// NewTab creates a tab and returns its root pane.
func (c *Client) NewTab(ctx context.Context, workspaceID, cwd, label string, env map[string]string) (Pane, error) {
	params := map[string]any{"focus": false}
	if len(env) > 0 {
		params["env"] = env
	}
	if workspaceID != "" {
		params["workspace_id"] = workspaceID
	}
	if cwd != "" {
		params["cwd"] = cwd
	}
	if label != "" {
		params["label"] = label
	}
	var r struct {
		RootPane Pane `json:"root_pane"`
	}
	err := c.Call(ctx, "tab.create", params, &r)
	return r.RootPane, err
}

type worktreeWorkspace struct {
	WorkspaceID string `json:"workspace_id"`
	Worktree    *struct {
		CheckoutPath string `json:"checkout_path"`
	} `json:"worktree"`
}

// sameDirectory handles cleaned paths and local symlink aliases without matching
// sibling checkouts or a shell that has changed into a different directory.
func sameDirectory(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	canonical := func(path string) string {
		abs, err := filepath.Abs(path)
		if err != nil {
			return filepath.Clean(path)
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			return resolved
		}
		return abs
	}
	return canonical(a) == canonical(b)
}

// WorktreePane finds or opens the checkout's workspace and returns an available
// shell there. The boolean says whether this call created the returned pane;
// callers must not close a reused shell when agent startup fails.
func (c *Client) WorktreePane(ctx context.Context, cwd, label string, env map[string]string) (Pane, bool, error) {
	var listed struct {
		Workspaces []worktreeWorkspace `json:"workspaces"`
	}
	if err := c.Call(ctx, "workspace.list", nil, &listed); err != nil {
		return Pane{}, false, fmt.Errorf("list worktree workspaces: %w", err)
	}
	ws := ""
	for _, w := range listed.Workspaces {
		if w.Worktree != nil && sameDirectory(w.Worktree.CheckoutPath, cwd) {
			if w.WorkspaceID == "" {
				return Pane{}, false, fmt.Errorf("worktree workspace has no workspace ID")
			}
			if ws != "" && ws != w.WorkspaceID {
				return Pane{}, false, fmt.Errorf("multiple workspaces match the worktree; choose an explicit --pane")
			}
			ws = w.WorkspaceID
		}
	}
	if ws == "" {
		var opened struct {
			Workspace *worktreeWorkspace `json:"workspace"`
		}
		if err := c.Call(ctx, "worktree.open", map[string]any{"path": cwd, "cwd": cwd, "focus": false}, &opened); err != nil {
			return Pane{}, false, fmt.Errorf("open worktree workspace: %w", err)
		}
		if opened.Workspace == nil || opened.Workspace.WorkspaceID == "" || opened.Workspace.Worktree == nil || !sameDirectory(opened.Workspace.Worktree.CheckoutPath, cwd) {
			return Pane{}, false, fmt.Errorf("worktree.open returned no matching workspace")
		}
		ws = opened.Workspace.WorkspaceID
	}
	var listedPanes struct {
		Panes []Pane `json:"panes"`
	}
	if err := c.Call(ctx, "pane.list", map[string]any{"workspace_id": ws}, &listedPanes); err != nil {
		return Pane{}, false, fmt.Errorf("list worktree panes: %w", err)
	}
	for _, p := range listedPanes.Panes {
		if p.PaneID == "" || p.WorkspaceID != ws || (p.Agent != nil && *p.Agent != "") || p.Cwd == nil || !sameDirectory(*p.Cwd, cwd) || (p.ForegroundCwd != nil && !sameDirectory(*p.ForegroundCwd, cwd)) {
			continue
		}
		info, err := c.ProcessInfo(ctx, p.PaneID)
		if err != nil {
			// Some nonterminal panes reject process inspection. A server refusal
			// establishes no usable shell, so try another candidate. Connection,
			// context, and decoding failures remain errors rather than mutations.
			var apiErr *Error
			if errors.As(err, &apiErr) {
				continue
			}
			return Pane{}, false, fmt.Errorf("check worktree shell: %w", err)
		}
		if info.AvailableShell() {
			return p, false, nil
		}
	}
	p, err := c.NewTab(ctx, ws, cwd, label, env)
	if err != nil {
		return Pane{}, false, fmt.Errorf("create worktree tab: %w", err)
	}
	if p.PaneID == "" || p.WorkspaceID != ws {
		return Pane{}, false, fmt.Errorf("tab.create returned no pane in the worktree workspace")
	}
	if err := waitForShell(ctx, func(ctx context.Context) (ProcessInfo, error) { return c.ProcessInfo(ctx, p.PaneID) }); err != nil {
		return p, true, fmt.Errorf("new worktree tab has no shell: %w", err)
	}
	return p, true, nil
}

// A new terminal may not have spawned its shell yet. Bound this check so a
// widget or a broken terminal cannot reach AgentStart or wait indefinitely.
func waitForShell(ctx context.Context, read func(context.Context) (ProcessInfo, error)) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := read(ctx)
		if err != nil {
			return err
		}
		if info.ShellPID > 0 {
			return nil
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Split splits target (or the focused pane if empty) and returns the new pane.
func (c *Client) Split(ctx context.Context, target, direction, cwd string) (Pane, error) {
	params := map[string]any{"direction": direction, "focus": false}
	if target != "" {
		params["target_pane_id"] = target
	}
	if cwd != "" {
		params["cwd"] = cwd
	}
	var r struct {
		Pane Pane `json:"pane"`
	}
	err := c.Call(ctx, "pane.split", params, &r)
	return r.Pane, err
}

// AgentStart starts an agent through the herdr CLI. The raw agent.start call returns
// before the agent is interactive and refuses a shell that is still starting up; the CLI
// waits for both (see docs/herdr-api-notes.md).
func (c *Client) AgentStart(ctx context.Context, name, kind, paneID string, args []string) (Pane, error) {
	bin := os.Getenv("HERDR_BIN_PATH")
	if bin == "" {
		bin = "herdr"
	}
	argv := []string{"agent", "start", name, "--kind", kind, "--pane", paneID, "--timeout", "60000"}
	if len(args) > 0 {
		argv = append(append(argv, "--"), args...)
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, argv...)
	cmd.Env = append(os.Environ(), "HERDR_SOCKET_PATH="+c.Socket)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var resp struct {
		Result struct {
			Agent Pane `json:"agent"`
		} `json:"result"`
		Error *Error `json:"error"`
	}
	for _, out := range [][]byte{stdout.Bytes(), stderr.Bytes()} {
		if json.Unmarshal(bytes.TrimSpace(out), &resp) == nil && (resp.Error != nil || resp.Result.Agent.PaneID != "") {
			break
		}
	}
	if resp.Error != nil {
		return Pane{}, resp.Error
	}
	if runErr != nil {
		return Pane{}, fmt.Errorf("herdr agent start: %v: %s", runErr, bytes.TrimSpace(stderr.Bytes()))
	}
	return resp.Result.Agent, nil
}

// Event is one line from an events.subscribe stream. Name is normalised to dotted form
// (herdr sends both "pane_exited" and "pane.agent_status_changed").
type Event struct {
	Name string
	Data json.RawMessage
}

// Subscription describes one events.subscribe entry.
type Subscription map[string]any

// Subscribe opens a subscription and delivers events on the returned channel until ctx is
// cancelled or the connection drops (the channel is then closed).
func (c *Client) Subscribe(ctx context.Context, subs []Subscription) (<-chan Event, error) {
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, err := d.DialContext(dctx, "unix", c.Socket)
	cancel()
	if err != nil {
		return nil, err
	}
	req, _ := json.Marshal(map[string]any{"id": "sub", "method": "events.subscribe", "params": map[string]any{"subscriptions": subs}})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		conn.Close()
		return nil, err
	}
	r := bufio.NewReaderSize(conn, 1<<20)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	first, err := r.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	var ack struct {
		Error *Error `json:"error"`
	}
	if json.Unmarshal(first, &ack) == nil && ack.Error != nil {
		conn.Close()
		return nil, ack.Error
	}
	_ = conn.SetReadDeadline(time.Time{})
	ch := make(chan Event, 256)
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	go func() {
		defer close(ch)
		defer conn.Close()
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				var env struct {
					Event string          `json:"event"`
					Data  json.RawMessage `json:"data"`
				}
				if json.Unmarshal(line, &env) == nil && env.Event != "" {
					select {
					case ch <- Event{Name: NormalizeEvent(env.Event), Data: env.Data}:
					case <-ctx.Done():
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return ch, nil
}

// NormalizeEvent maps "pane_agent_detected" → "pane.agent_detected" and leaves dotted names alone.
func NormalizeEvent(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] == '.' {
			return name
		}
		if name[i] == '_' {
			return name[:i] + "." + name[i+1:]
		}
	}
	return name
}
