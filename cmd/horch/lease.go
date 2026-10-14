package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

func cmdLease(ctx context.Context, sub string, args []string) error {
	f := fs("lease " + sub)
	a := daemon.LeaseArgs{}
	f.StringVar(&a.Resource, "resource", "", "")
	f.StringVar(&a.Worker, "worker", "", "")
	ttl := f.String("ttl", "", "")
	if _, err := parse(f, args); err != nil {
		return err
	}
	switch sub {
	case "acquire", "release":
		if a.Resource == "" || a.Worker == "" {
			return usageErr("lease %s requires --resource and --worker", sub)
		}
		if _, err := store.NormalizeResource(a.Resource); err != nil {
			return usageErr("%v", err)
		}
		if *ttl != "" {
			if sub != "acquire" {
				return usageErr("--ttl is only valid for lease acquire")
			}
			d, err := time.ParseDuration(*ttl)
			if err != nil || d < time.Millisecond {
				return usageErr("--ttl must be a positive duration of at least 1ms")
			}
			ms := d.Milliseconds()
			a.TTLMS = &ms
		}
		return run(ctx, "lease."+sub, a, printLeases)
	case "list", "":
		return run(ctx, "lease.list", nil, printLeases)
	default:
		return usageErr("unknown lease subcommand %q", sub)
	}
}
func printLeases(raw json.RawMessage) {
	var ls []store.Lease
	if err := json.Unmarshal(raw, &ls); err != nil {
		var l store.Lease
		if err := json.Unmarshal(raw, &l); err == nil && l.Resource != "" {
			ls = []store.Lease{l}
		} else {
			fmt.Println(string(raw))
			return
		}
	}
	if len(ls) == 0 {
		fmt.Println("no active leases")
		return
	}
	for _, l := range ls {
		name := l.HolderName
		if name == "" {
			name = l.Worker
		}
		fmt.Printf("%s  %s (%s, session %s)  expires %s\n", l.Resource, name, l.Worker, l.Scope, time.UnixMilli(l.ExpiresAt).Format(time.RFC3339))
	}
}
func cmdSweep(ctx context.Context, args []string) error {
	f := fs("sweep")
	a := daemon.SweepArgs{}
	f.BoolVar(&a.DryRun, "dry-run", false, "")
	if _, err := parse(f, args); err != nil {
		return err
	}
	return run(ctx, "sweep", a, printSweepResult)
}

func printProcessOutcomes(ps []daemon.ProcessOutcome) {
	for _, p := range ps {
		fmt.Printf("  %s pid=%d command=%q cwd=%q", p.Action, p.PID, p.Command, p.Cwd)
		if p.Error != "" {
			fmt.Printf(" error=%q", p.Error)
		}
		fmt.Println()
	}
}
func printCloseResult(raw json.RawMessage) {
	var r daemon.CloseResult
	_ = json.Unmarshal(raw, &r)
	fmt.Printf("%s %s: pane closed; %d process(es) observed", r.Worker.PaneID, r.Worker.State, len(r.PIDs))
	if r.ArchivePath != "" {
		fmt.Printf("; transcript %s", r.ArchivePath)
	}
	fmt.Println()
	printProcessOutcomes(r.Processes)
}
func printSweepResult(raw json.RawMessage) {
	var r struct {
		DryRun    bool                    `json:"dry_run"`
		Processes []daemon.ProcessOutcome `json:"processes"`
	}
	_ = json.Unmarshal(raw, &r)
	if r.DryRun {
		fmt.Printf("sweep dry run: %d process(es) considered\n", len(r.Processes))
	} else {
		fmt.Printf("sweep: %d process(es) considered\n", len(r.Processes))
	}
	printProcessOutcomes(r.Processes)
}
