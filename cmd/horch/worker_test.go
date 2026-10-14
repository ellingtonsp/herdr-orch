package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ellingtonsp/herdr-orch/internal/client"
	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/paths"
	"github.com/ellingtonsp/herdr-orch/internal/rpc"
)

func workerCLIRequest(t *testing.T, args ...string) rpc.Request {
	t.Helper()
	dir, err := os.MkdirTemp("", "worker-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	requests := make(chan rpc.Request, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req rpc.Request
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		requests <- req
		_ = json.NewEncoder(conn).Encode(rpc.Response{Error: &rpc.Error{Code: "fixture", Message: "request recorded"}})
	}()
	oldCLI, oldJSON := cli, jsonOut
	t.Cleanup(func() { cli, jsonOut = oldCLI, oldJSON })
	cli = &client.Client{Paths: paths.Paths{Sock: socket}, Caller: "w1:p1"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := dispatch(ctx, args); err == nil || !strings.Contains(err.Error(), "fixture") {
		t.Fatalf("expected isolated fixture response, got %v", err)
	}
	return <-requests
}

func TestWorkerCLIWorktreeAndLegacyCwd(t *testing.T) {
	for _, flag := range []string{"--worktree", "--cwd"} {
		t.Run(flag, func(t *testing.T) {
			dir := t.TempDir()
			req := workerCLIRequest(t, "worker", "start", "--agent", "codex", "--name", "you", flag, dir, "--", "--example")
			var args daemon.WorkerStartArgs
			if err := json.Unmarshal(req.Args, &args); err != nil {
				t.Fatal(err)
			}
			if req.Op != "worker.start" || args.Agent != "codex" || args.Name != "you" || len(args.Args) != 1 || args.Args[0] != "--example" {
				t.Fatalf("request=%+v args=%+v", req, args)
			}
			if flag == "--worktree" && (args.Worktree != dir || args.Cwd != "") {
				t.Fatalf("worktree flag lost workspace routing: %+v", args)
			}
			if flag == "--cwd" && (args.Cwd != dir || args.Worktree != "") {
				t.Fatalf("legacy cwd changed workspace routing: %+v", args)
			}
		})
	}
}

func TestWorkerRegisterPreservesCwdAlias(t *testing.T) {
	for _, flag := range []string{"--worktree", "--cwd"} {
		t.Run(flag, func(t *testing.T) {
			dir := t.TempDir()
			req := workerCLIRequest(t, "worker", "register", "--pane", "w2:p1", flag, dir)
			var args daemon.WorkerRegisterArgs
			if err := json.Unmarshal(req.Args, &args); err != nil {
				t.Fatal(err)
			}
			if req.Op != "worker.register" || args.Pane != "w2:p1" || args.Worktree != dir {
				t.Fatalf("registration alias changed: request=%+v args=%+v", req, args)
			}
		})
	}
}
