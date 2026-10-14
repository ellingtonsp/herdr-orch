package main

import (
	"encoding/json"
	"net"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ellingtonsp/herdr-orch/internal/client"
	"github.com/ellingtonsp/herdr-orch/internal/paths"
	"github.com/ellingtonsp/herdr-orch/internal/rpc"
)

// Check the actual CLI-to-RPC path against a private one-request socket. No
// installed daemon can be reached or spawned by this test.
func TestLifecycleCLIWire(t *testing.T) {
	cases := []struct {
		args []string
		op   string
		want map[string]any
	}{
		{[]string{"dispatch", "hold", "--task", "t1", "--until", "event:review", "--reason", "waiting"}, "dispatch.hold", map[string]any{"task": "t1", "until": "event:review", "reason": "waiting"}},
		{[]string{"dispatch", "resume", "--task", "t1"}, "dispatch.resume", map[string]any{"task": "t1"}},
		{[]string{"dispatch", "fail", "--task", "t1", "--reason", "usage cap", "--infra"}, "dispatch.fail", map[string]any{"task": "t1", "reason": "usage cap", "infra": true}},
		{[]string{"dispatch", "fail", "--task", "t1", "--reason", "bug"}, "dispatch.fail", map[string]any{"task": "t1", "reason": "bug"}},
		{[]string{"worker", "restart", "--worker", "helper", "--env", "PROFILE=alternate", "--env", "MODE=test", "--unset", "API_KEY", "--unset", "OLD_PROFILE"}, "worker.restart", map[string]any{"worker": "helper", "env": []any{"PROFILE=alternate", "MODE=test"}, "unset": []any{"API_KEY", "OLD_PROFILE"}}},
		{[]string{"worker", "release", "--worker", "helper", "--hand-off-to", "owner"}, "worker.release", map[string]any{"worker": "helper", "hand_off_to": "owner"}},
		{[]string{"worker", "release", "--worker", "helper"}, "worker.release", map[string]any{"worker": "helper"}},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			sock := filepath.Join(t.TempDir(), "s")
			ln, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			old := cli
			cli = &client.Client{Paths: paths.Paths{Sock: sock}}
			defer func() { cli = old }()
			requests := make(chan rpc.Request, 1)
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				var req rpc.Request
				if json.NewDecoder(c).Decode(&req) != nil {
					return
				}
				requests <- req
				_ = json.NewEncoder(c).Encode(rpc.Response{OK: true, Result: json.RawMessage(`{}`)})
			}()
			args := append(append([]string{}, tc.args...), "--json")
			if _, err := horch(t, "human", args...); err != nil {
				t.Fatal(err)
			}
			req := <-requests
			var got map[string]any
			if err := json.Unmarshal(req.Args, &got); err != nil {
				t.Fatal(err)
			}
			if req.Op != tc.op || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %s %v; want %s %v", req.Op, got, tc.op, tc.want)
			}
		})
	}
}
