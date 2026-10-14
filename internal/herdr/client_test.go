package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type apiStep struct {
	method string
	params map[string]any
	result any
}

// scriptedAPI never connects to a user's server. Each request has its own socket connection.
func scriptedAPI(t *testing.T, steps []apiStep) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "pane-api-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, step := range steps {
			conn, err := ln.Accept()
			if err != nil {
				t.Errorf("missing request %s: %v", step.method, err)
				return
			}
			var req struct {
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&req); err != nil {
				t.Errorf("decode request: %v", err)
			}
			if req.Method != step.method || !reflect.DeepEqual(req.Params, step.params) {
				t.Errorf("request %s %+v; want %s %+v", req.Method, req.Params, step.method, step.params)
			}
			response := map[string]any{"result": step.result}
			if apiErr, ok := step.result.(*Error); ok {
				response = map[string]any{"error": apiErr}
			}
			_ = json.NewEncoder(conn).Encode(response)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); <-done })
	return New(socket)
}

func TestPaneGetRejectsMissingWorkspace(t *testing.T) {
	for _, result := range []any{
		map[string]any{"pane": nil},
		map[string]any{"pane": map[string]any{"pane_id": "w1:p1", "workspace_id": nil}},
		map[string]any{"pane": map[string]any{"pane_id": "w1:p1"}},
	} {
		t.Run("missing workspace", func(t *testing.T) {
			c := scriptedAPI(t, []apiStep{{"pane.get", map[string]any{"pane_id": "w1:p1"}, result}})
			_, err := c.PaneGet(context.Background(), "w1:p1")
			if err == nil || !strings.Contains(err.Error(), "workspace") {
				t.Fatalf("want clear workspace error, got %v", err)
			}
		})
	}
}

func TestWorktreePaneReusesOnlyAvailableShell(t *testing.T) {
	cwd := t.TempDir()
	other := t.TempDir()
	panes := []Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", Cwd: &cwd},                        // wrong workspace
		{PaneID: "w2:p5", WorkspaceID: "w2", Cwd: &cwd, ForegroundCwd: &other}, // shell moved elsewhere
		{PaneID: "w2:p0", WorkspaceID: "w2", Cwd: &cwd},                        // widget: no shell PID
		{PaneID: "w2:p1", WorkspaceID: "w2", Cwd: &cwd},                        // widget: API rejects process inspection
		{PaneID: "w2:p2", WorkspaceID: "w2", Cwd: &cwd},                        // foreground command
		{PaneID: "w2:p3", WorkspaceID: "w2", Cwd: &other},
		{PaneID: "w2:p4", WorkspaceID: "w2", Cwd: &cwd},
	}
	c := scriptedAPI(t, []apiStep{
		{"workspace.list", map[string]any{}, map[string]any{"workspaces": []any{
			map[string]any{"workspace_id": "w1", "worktree": nil},
			map[string]any{"workspace_id": "w2", "worktree": map[string]any{"checkout_path": cwd}},
		}}},
		{"pane.list", map[string]any{"workspace_id": "w2"}, map[string]any{"panes": panes}},
		{"pane.process_info", map[string]any{"pane_id": "w2:p0"}, map[string]any{"process_info": map[string]any{"shell_pid": nil}}},
		{"pane.process_info", map[string]any{"pane_id": "w2:p1"}, &Error{Code: "fixture_refusal", Message: "synthetic nonterminal pane"}},
		{"pane.process_info", map[string]any{"pane_id": "w2:p2"}, map[string]any{"process_info": map[string]any{"shell_pid": 10, "foreground_processes": []any{map[string]any{"pid": 11}}}}},
		{"pane.process_info", map[string]any{"pane_id": "w2:p4"}, map[string]any{"process_info": map[string]any{"shell_pid": 20, "foreground_process_group_id": 20}}},
	})
	p, created, err := c.WorktreePane(context.Background(), cwd, "you", nil)
	if err != nil || created || p.PaneID != "w2:p4" {
		t.Fatalf("pane %+v created=%v err=%v", p, created, err)
	}
}

func TestWorktreePaneCreatesTabInMatchingWorkspace(t *testing.T) {
	cwd := t.TempDir()
	agent := "codex"
	c := scriptedAPI(t, []apiStep{
		{"workspace.list", map[string]any{}, map[string]any{"workspaces": []any{map[string]any{"workspace_id": "w2", "worktree": map[string]any{"checkout_path": cwd}}}}},
		{"pane.list", map[string]any{"workspace_id": "w2"}, map[string]any{"panes": []Pane{{PaneID: "w2:p1", WorkspaceID: "w2", Agent: &agent, Cwd: &cwd}}}},
		{"tab.create", map[string]any{"workspace_id": "w2", "cwd": cwd, "label": "you", "focus": false, "env": map[string]any{"PATH": "bin"}}, map[string]any{"root_pane": Pane{PaneID: "w2:p2", WorkspaceID: "w2"}}},
		{"pane.process_info", map[string]any{"pane_id": "w2:p2"}, map[string]any{"process_info": map[string]any{"shell_pid": 20}}},
	})
	p, created, err := c.WorktreePane(context.Background(), cwd, "you", map[string]string{"PATH": "bin"})
	if err != nil || !created || p.PaneID != "w2:p2" {
		t.Fatalf("pane %+v created=%v err=%v", p, created, err)
	}
}

func TestWorktreePaneOpensWorkspaceAndSkipsRootWidget(t *testing.T) {
	cwd := t.TempDir()
	workspace := map[string]any{"workspace_id": "w2", "worktree": map[string]any{"checkout_path": cwd}}
	c := scriptedAPI(t, []apiStep{
		{"workspace.list", map[string]any{}, map[string]any{"workspaces": []any{}}},
		{"worktree.open", map[string]any{"path": cwd, "cwd": cwd, "focus": false}, map[string]any{"workspace": workspace, "root_pane": Pane{PaneID: "w2:p1", WorkspaceID: "w2"}}},
		{"pane.list", map[string]any{"workspace_id": "w2"}, map[string]any{"panes": []Pane{{PaneID: "w2:p1", WorkspaceID: "w2", Cwd: &cwd}}}},
		{"pane.process_info", map[string]any{"pane_id": "w2:p1"}, map[string]any{"process_info": map[string]any{"shell_pid": nil}}},
		{"tab.create", map[string]any{"workspace_id": "w2", "cwd": cwd, "label": "you", "focus": false}, map[string]any{"root_pane": Pane{PaneID: "w2:p2", WorkspaceID: "w2"}}},
		{"pane.process_info", map[string]any{"pane_id": "w2:p2"}, map[string]any{"process_info": map[string]any{"shell_pid": nil}}},
		{"pane.process_info", map[string]any{"pane_id": "w2:p2"}, map[string]any{"process_info": map[string]any{"shell_pid": 20}}},
	})
	p, created, err := c.WorktreePane(context.Background(), cwd, "you", nil)
	if err != nil || !created || p.PaneID != "w2:p2" {
		t.Fatalf("pane %+v created=%v err=%v", p, created, err)
	}
}

func TestWorktreePaneRejectsMissingOpenedWorkspace(t *testing.T) {
	for _, workspace := range []any{nil, map[string]any{}, map[string]any{"workspace_id": "w2"}} {
		t.Run("missing workspace", func(t *testing.T) {
			cwd := t.TempDir()
			c := scriptedAPI(t, []apiStep{
				{"workspace.list", map[string]any{}, map[string]any{"workspaces": []any{}}},
				{"worktree.open", map[string]any{"path": cwd, "cwd": cwd, "focus": false}, map[string]any{"workspace": workspace}},
			})
			_, _, err := c.WorktreePane(context.Background(), cwd, "you", nil)
			if err == nil || !strings.Contains(err.Error(), "matching workspace") {
				t.Fatalf("want matching workspace error, got %v", err)
			}
		})
	}
}

func TestWorktreePaneRejectsAmbiguousWorkspace(t *testing.T) {
	cwd := t.TempDir()
	c := scriptedAPI(t, []apiStep{{"workspace.list", map[string]any{}, map[string]any{"workspaces": []any{
		map[string]any{"workspace_id": "w1", "worktree": map[string]any{"checkout_path": cwd}},
		map[string]any{"workspace_id": "w2", "worktree": map[string]any{"checkout_path": cwd}},
	}}}})
	_, _, err := c.WorktreePane(context.Background(), cwd, "you", nil)
	if err == nil || !strings.Contains(err.Error(), "multiple workspaces") {
		t.Fatalf("want ambiguity error, got %v", err)
	}
}

func TestAvailableShell(t *testing.T) {
	for _, tt := range []struct {
		name string
		info string
		want bool
	}{
		{"widget", `{"shell_pid":null}`, false},
		{"unknown foreground", `{"shell_pid":10}`, false},
		{"shell process group", `{"shell_pid":10,"foreground_process_group_id":10}`, true},
		{"shell foreground process", `{"shell_pid":10,"foreground_processes":[{"pid":10}]}`, true},
		{"command process group", `{"shell_pid":10,"foreground_process_group_id":11}`, false},
		{"command process", `{"shell_pid":10,"foreground_processes":[{"pid":11}]}`, false},
		{"command beside shell", `{"shell_pid":10,"foreground_processes":[{"pid":10},{"pid":11}]}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var info ProcessInfo
			if err := json.Unmarshal([]byte(tt.info), &info); err != nil {
				t.Fatal(err)
			}
			if got := info.AvailableShell(); got != tt.want {
				t.Fatalf("available=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestSameDirectory(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "myapp")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if !sameDirectory(dir, alias) || !sameDirectory(dir, filepath.Join(dir, "..", filepath.Base(dir))) || sameDirectory(dir, t.TempDir()) || sameDirectory("", dir) {
		t.Fatal("directory equivalence did not preserve checkout identity")
	}
}

func TestWaitForShellRespectsCancellationAndRejectsWidget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := waitForShell(ctx, func(context.Context) (ProcessInfo, error) { return ProcessInfo{}, nil })
	if err == nil || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("widget wait err=%v elapsed=%v", err, time.Since(started))
	}
}

func TestWorktreePaneDoesNotFallBackOnAPIError(t *testing.T) {
	apiErr := &Error{Code: "unavailable", Message: "synthetic API failure"}
	c := scriptedAPI(t, []apiStep{{"workspace.list", map[string]any{}, apiErr}})
	_, created, err := c.WorktreePane(context.Background(), t.TempDir(), "you", nil)
	if err == nil || created || !strings.Contains(err.Error(), "synthetic API failure") {
		t.Fatalf("created=%v err=%v", created, err)
	}
}

func TestWorktreePaneReportsCreatedPaneWhenShellCheckFails(t *testing.T) {
	cwd := t.TempDir()
	c := scriptedAPI(t, []apiStep{
		{"workspace.list", map[string]any{}, map[string]any{"workspaces": []any{map[string]any{"workspace_id": "w2", "worktree": map[string]any{"checkout_path": cwd}}}}},
		{"pane.list", map[string]any{"workspace_id": "w2"}, map[string]any{"panes": []any{}}},
		{"tab.create", map[string]any{"workspace_id": "w2", "cwd": cwd, "label": "you", "focus": false}, map[string]any{"root_pane": Pane{PaneID: "w2:p1", WorkspaceID: "w2"}}},
		{"pane.process_info", map[string]any{"pane_id": "w2:p1"}, &Error{Code: "fixture_refusal", Message: "synthetic widget"}},
	})
	p, created, err := c.WorktreePane(context.Background(), cwd, "you", nil)
	if err == nil || !created || p.PaneID != "w2:p1" {
		t.Fatalf("cannot clean up created non-shell pane: pane=%+v created=%v err=%v", p, created, err)
	}
}

func TestWorktreePaneStopsOnProcessDecodeFailure(t *testing.T) {
	cwd := t.TempDir()
	c := scriptedAPI(t, []apiStep{
		{"workspace.list", map[string]any{}, map[string]any{"workspaces": []any{map[string]any{"workspace_id": "w2", "worktree": map[string]any{"checkout_path": cwd}}}}},
		{"pane.list", map[string]any{"workspace_id": "w2"}, map[string]any{"panes": []Pane{{PaneID: "w2:p1", WorkspaceID: "w2", Cwd: &cwd}}}},
		{"pane.process_info", map[string]any{"pane_id": "w2:p1"}, map[string]any{"process_info": "synthetic malformed response"}},
	})
	_, created, err := c.WorktreePane(context.Background(), cwd, "you", nil)
	if err == nil || created || !strings.Contains(err.Error(), "check worktree shell") {
		t.Fatalf("created=%v err=%v", created, err)
	}
}
