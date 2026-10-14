package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/prwatch"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

func TestPRWatchCLIInbox(t *testing.T) {
	startDaemon(t)
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HORCH_CONFIG", cfg)
	old := prCommand
	t.Cleanup(func() { prCommand = old })
	calls := 0
	prCommand = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "repo" {
			return []byte("you/myapp\n"), nil
		}
		if args[0] == "api" && args[1] == "graphql" {
			calls++
			return []byte(`{"data":{"repository":{"pullRequest":{"headRefOid":"abc","baseRefName":"main","state":"MERGED","url":"https://example.com/pr/12"}}}}`), nil
		}
		t.Fatalf("unexpected gh call: %v", args)
		return nil, nil
	}
	var r store.Run
	if err := json.Unmarshal([]byte(mustHorch(t, "w1:p1", "run", "create", "--title", "acme", "--json")), &r); err != nil {
		t.Fatal(err)
	}
	mustHorch(t, "w1:p2", "run", "create", "--title", "another run")
	out := mustHorch(t, "w1:p2", "pr", "watch", "12", "--until", "merged", "--run", r.ID, "--json")
	var e prwatch.Event
	if err := json.Unmarshal([]byte(out), &e); err != nil || e.Message != "merged" {
		t.Fatalf("output: %s %v", out, err)
	}
	var msgs []store.Message
	if err := cli.Call(context.Background(), "inbox", daemon.InboxArgs{All: true}, &msgs); err != nil {
		t.Fatal(err)
	}
	// Inspect as the intended coordinator, not the watcher's current run coordinator.
	cli.Caller = "w1:p1"
	if err := cli.Call(context.Background(), "inbox", daemon.InboxArgs{All: true}, &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].RunID != r.ID || !strings.Contains(msgs[0].Body, "merged\nhead abc") {
		t.Fatalf("mail: %+v", msgs)
	}
	if calls != 1 {
		t.Fatalf("merged queried CI: %d", calls)
	}
}
func TestPRWatchCLIValidation(t *testing.T) {
	old := prCommand
	t.Cleanup(func() { prCommand = old })
	prCommand = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("unexpected gh invocation") }
	for _, args := range [][]string{{"pr"}, {"pr", "watch"}, {"pr", "watch", "0"}, {"pr", "watch", "12", "extra"}, {"pr", "watch", "12", "--until", "bad"}, {"pr", "watch", "12", "--interval", "0s"}, {"pr", "watch", "12", "--race-window", "-1s"}, {"pr", "watch", "12", "--repo", "bad"}} {
		if err := dispatch(context.Background(), args); err == nil || strings.Contains(err.Error(), "unexpected") {
			t.Fatalf("%v: %v", args, err)
		}
	}
}
