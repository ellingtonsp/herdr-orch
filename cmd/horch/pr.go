package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/config"
	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/prwatch"
	"github.com/ellingtonsp/herdr-orch/internal/rpc"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

var prCommand prwatch.Command = prwatch.ExecGH

func firstNonempty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
func cmdPR(ctx context.Context, sub string, args []string) error {
	if sub != "watch" {
		return usageErr("pr: use watch <n>")
	}
	f := fs("pr watch")
	repo := f.String("repo", "", "")
	until := f.String("until", "green", "")
	interval := f.Duration("interval", 15*time.Second, "")
	raceWindow := f.Duration("race-window", 5*time.Second, "")
	runID := f.String("run", "", "")
	pos, err := parse(f, args)
	if err != nil {
		return err
	}
	number, err := strconv.Atoi(pos)
	if err != nil || number <= 0 || f.NArg() > 1 || (len(args) > 0 && args[0] == pos && f.NArg() != 0) {
		return usageErr("pr watch requires one positive PR number")
	}
	if *until != "green" && *until != "merged" {
		return usageErr("--until must be green or merged")
	}
	if *interval <= 0 || *raceWindow < 0 {
		return usageErr("--interval must be positive and --race-window non-negative")
	}
	if *repo == "" {
		b, err := prCommand(ctx, "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
		if err != nil {
			return err
		}
		*repo = strings.TrimSpace(string(b))
	}
	parts := strings.Split(*repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(*repo, "?%# \\\t\n") {
		return usageErr("--repo must be owner/name")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var fallback []string
	for _, p := range cfg.Projects {
		if strings.EqualFold(p.Repo, *repo) && p.RequiredChecks != nil {
			if fallback != nil {
				return errors.New("multiple required_checks fallbacks configured for repository")
			}
			fallback = p.RequiredChecks
		}
	}
	// Pin the run at startup. Use direct RPC so a watcher never starts/replaces a daemon.
	var active daemon.RunShow
	if err := rpc.Call(ctx, cli.Paths.Sock, "run.show", cli.Caller, daemon.RunRef{Run: *runID}, &active); err != nil {
		return err
	}
	github := prwatch.GitHub{Repo: *repo, Number: number, Fallback: fallback, Command: prCommand}
	fetch := func(ctx context.Context) (prwatch.Snapshot, error) { return github.Snapshot(ctx) }
	post := func(ctx context.Context, event prwatch.Event) error {
		kind := store.KindNote
		if event.Escalation {
			kind = store.KindEscalation
		}
		a := daemon.SendArgs{Run: active.Run.ID, To: "coordinator", Kind: kind, Subject: fmt.Sprintf("%s PR #%d: %s", *repo, number, event.Message), Body: fmt.Sprintf("%s\nhead %s\n%s", event.Message, event.Head, event.URL)}
		if err := rpc.Call(ctx, cli.Paths.Sock, "send", cli.Caller, a, nil); err != nil {
			return err
		}
		if jsonOut {
			b, _ := json.Marshal(event)
			fmt.Println(string(b))
		} else {
			fmt.Println(a.Subject)
		}
		return nil
	}
	return prwatch.Watch(ctx, prwatch.Options{Until: *until, Interval: *interval, RaceWindow: *raceWindow}, fetch, post)
}
