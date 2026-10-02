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
	"fmt"
	"net"
	"os"
	"os/exec"
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
	ShellPID            int `json:"shell_pid"`
	ForegroundProcesses []struct {
		PID  int    `json:"pid"`
		Name string `json:"name"`
	} `json:"foreground_processes"`
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
