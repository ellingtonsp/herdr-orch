package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/config"
	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/planmd"
	"github.com/ellingtonsp/herdr-orch/internal/rpc"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

// planFlags adds the flags every plan command takes.
func planFlags(f *flag.FlagSet) *daemon.PlanRef {
	r := &daemon.PlanRef{Principal: os.Getenv("HORCH_PRINCIPAL")}
	f.StringVar(&r.Project, "project", "", "")
	f.StringVar(&r.Day, "day", "", "")
	f.StringVar(&r.Principal, "principal", r.Principal, "")
	return r
}

func cmdPlan(ctx context.Context, sub string, args []string) error {
	switch sub {
	case "import":
		return cmdPlanImport(ctx, args)
	case "show", "":
		f := fs("plan show")
		ref := planFlags(f)
		if _, err := parse(f, args); err != nil {
			return err
		}
		return run(ctx, "plan.show", ref, printPlan)
	case "item":
		isub := ""
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			isub = args[0]
		}
		return cmdPlanItem(ctx, isub, tail(args))
	case "transition":
		return cmdPlanItem(ctx, "transition", args)
	case "status":
		f := fs("plan status")
		ref := planFlags(f)
		a := daemon.PlanStatusArgs{}
		f.StringVar(&a.Status, "set", "", "")
		f.Int64Var(&a.IfPlanVersion, "if-plan-version", 0, "")
		if _, err := parse(f, args); err != nil {
			return err
		}
		if a.Status == "" {
			return usageErr("usage: horch plan status --set draft|live|final")
		}
		a.PlanRef = *ref
		return run(ctx, "plan.status", a, printPlanWrite)
	case "events":
		return cmdPlanEvents(ctx, args)
	case "export":
		return cmdPlanExport(ctx, args)
	}
	return usageErr("unknown plan subcommand %q", sub)
}

func cmdPlanImport(ctx context.Context, args []string) error {
	f := fs("plan import")
	ref := planFlags(f)
	a := daemon.PlanImportArgs{}
	file := f.String("file", "", "")
	f.StringVar(&a.Ref, "ref", "", "")
	f.StringVar(&a.Status, "status", "", "")
	f.BoolVar(&a.Replace, "replace", false, "")
	if _, err := parse(f, args); err != nil {
		return err
	}
	if *file == "" || ref.Day == "" {
		return usageErr("usage: horch plan import --day YYYY-MM-DD --file PLAN.md [--ref COMMIT] [--replace]")
	}
	b, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	a.PlanRef, a.Markdown = *ref, string(b)
	if a.Ref == "" {
		a.Ref = *file
	}
	return run(ctx, "plan.import", a, func(raw json.RawMessage) {
		var r daemon.PlanImportResult
		_ = json.Unmarshal(raw, &r)
		if r.Changed {
			fmt.Printf("imported %s (%d items, v%d, %s)\n", r.View.Plan.ID, len(r.View.Items), r.View.Plan.Version, r.View.Plan.Status)
		} else {
			fmt.Printf("%s already imported from this file; unchanged (v%d)\n", r.View.Plan.ID, r.View.Plan.Version)
		}
		for _, w := range r.Warnings {
			fmt.Println("warning:", w)
		}
	})
}

func cmdPlanItem(ctx context.Context, sub string, args []string) error {
	ops := map[string]string{"add": "plan.item.add", "update": "plan.item.update", "move": "plan.item.move", "hold": "plan.item.hold", "release": "plan.item.release", "remove": "plan.item.remove", "transition": "plan.transition"}
	op, ok := ops[sub]
	if !ok {
		return usageErr("usage: horch plan item add|update|move|hold|release|remove --item ID ...")
	}
	f := fs("plan " + sub)
	ref := planFlags(f)
	a := daemon.PlanItemArgs{}
	f.StringVar(&a.Item, "item", "", "")
	f.StringVar(&a.Item, "id", "", "")
	f.Int64Var(&a.IfVersion, "if-version", 0, "")
	f.Int64Var(&a.IfPlanVersion, "if-plan-version", 0, "")
	f.IntVar(&a.Position, "position", 0, "")
	f.IntVar(&a.Position, "to", 0, "")
	f.StringVar(&a.Reason, "reason", "", "")
	f.StringVar(&a.Note, "note", "", "")
	fields := []string{"kind", "lane", "model", "state", "pr", "dispatch-ref", "title", "where", "output", "why"}
	vals := map[string]*string{}
	for _, n := range fields {
		vals[n] = f.String(n, "", "")
	}
	issues := f.String("issues", "", "")
	pos, err := parse(f, args)
	if err != nil {
		return err
	}
	if a.Item == "" {
		a.Item = pos
	}
	p := &a.Patch
	for n, dst := range map[string]**string{"kind": &p.Kind, "lane": &p.Lane, "model": &p.Model, "state": &p.State, "pr": &p.PR, "dispatch-ref": &p.DispatchRef, "title": &p.Title, "where": &p.Where, "output": &p.Output, "why": &p.Why} {
		*dst = strPtr(f, n, *vals[n])
	}
	if strPtr(f, "issues", *issues) != nil {
		l := splitList(*issues)
		if l == nil {
			l = []string{}
		}
		p.Issues = &l
	}
	a.PlanRef = *ref
	return run(ctx, op, a, printPlanWrite)
}

func cmdPlanEvents(ctx context.Context, args []string) error {
	f := fs("plan events")
	ref := planFlags(f)
	a := daemon.PlanEventsArgs{}
	f.Int64Var(&a.Since, "since", 0, "")
	f.IntVar(&a.Limit, "limit", 0, "")
	f.BoolVar(&a.All, "all", false, "")
	follow := f.Bool("follow", false, "")
	if _, err := parse(f, args); err != nil {
		return err
	}
	a.PlanRef = *ref
	if !*follow {
		return run(ctx, "plan.events", a, printPlanEvents)
	}
	// Stream until interrupted, resuming after the last seen event if the daemon restarts.
	for {
		err := cli.Stream(ctx, "plan.subscribe", a, func(raw json.RawMessage) error {
			var e store.PlanEvent
			if err := json.Unmarshal(raw, &e); err != nil {
				return err
			}
			a.Since = e.Seq
			if jsonOut {
				fmt.Println(string(raw))
			} else {
				printEventLine(e)
			}
			return nil
		})
		if ctx.Err() != nil {
			return nil
		}
		var re *rpc.Error
		switch {
		case errors.As(err, &re):
			if re.Code != "lagging" {
				return err
			}
		case err != nil && !errors.Is(err, rpc.ErrLost) && !errors.Is(err, rpc.ErrUnavailable):
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func cmdPlanExport(ctx context.Context, args []string) error {
	f := fs("plan export")
	ref := planFlags(f)
	a := daemon.PlanExportArgs{}
	f.StringVar(&a.Format, "format", "md", "")
	f.BoolVar(&a.Finalize, "finalize", false, "")
	out := f.String("out", "", "")
	if _, err := parse(f, args); err != nil {
		return err
	}
	a.PlanRef = *ref
	raw, err := call(ctx, "plan.export", a)
	if err != nil {
		return err
	}
	var x daemon.PlanExport
	if err := json.Unmarshal(raw, &x); err != nil {
		return err
	}
	body := x.Markdown
	if a.Format == "json" {
		b, _ := json.MarshalIndent(x, "", "  ")
		body = string(b) + "\n"
	}
	if *out == "" {
		if jsonOut && a.Format == "md" {
			fmt.Println(string(raw))
			return nil
		}
		fmt.Print(body)
		return nil
	}
	if err := os.WriteFile(*out, []byte(body), 0o644); err != nil {
		return err
	}
	if jsonOut {
		b, _ := json.Marshal(map[string]any{"ok": true, "out": *out, "plan": x.Plan, "events": len(x.Events)})
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("wrote %s (%s, %d events, %s)\n", *out, x.Plan.ID, len(x.Events), x.Plan.Status)
	return nil
}

// cmdConfig shows the per-user config (never secrets) and what is still missing.
func cmdConfig(args []string) error {
	f := fs("config")
	if _, err := parse(f, args); err != nil {
		return err
	}
	c, err := config.Load()
	if err != nil {
		return err
	}
	missing := c.Missing()
	if jsonOut {
		b, _ := json.Marshal(map[string]any{"config": c, "missing": missing})
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("config: %s (found=%v)\n", c.File, c.Found)
	fmt.Printf("owner: %s  default project: %s\n", or(c.Owner.Principal, "-"), or(c.DefaultProject, "-"))
	for _, m := range missing {
		fmt.Println("missing:", m)
	}
	return nil
}

// ---------- printing ----------

func printPlan(raw json.RawMessage) {
	var v store.PlanView
	_ = json.Unmarshal(raw, &v)
	p := v.Plan
	fmt.Printf("%s  [%s]  v%d  ref=%s  coordinator=%s\n\n", p.ID, p.Status, p.Version, or(p.PublishedRef, "-"), or(p.Coordinator, "-"))
	w := tw()
	fmt.Fprintln(w, "#\tID\tSTATE\tKIND\tLANE\tMODEL\tPR\tVER\tTITLE")
	for _, it := range v.Items {
		state := it.State
		if it.Held {
			state = "held: " + it.HeldReason
			if len(state) > 40 {
				state = state[:40] + "…"
			}
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", it.Position, it.ID, state, or(it.Kind, "-"), or(it.Lane, "-"), or(it.Model, "-"), or(it.PR, "-"), it.Version, it.Title)
	}
	w.Flush()
	if len(v.Decisions) > 0 {
		fmt.Println("\nDecisions:")
		for _, d := range v.Decisions {
			fmt.Printf("  - %s — %s\n", d.Title, d.Body)
		}
	}
}

func printPlanWrite(raw json.RawMessage) {
	var w store.PlanWrite
	_ = json.Unmarshal(raw, &w)
	if len(w.Events) == 0 {
		fmt.Printf("%s unchanged (v%d)\n", w.View.Plan.ID, w.View.Plan.Version)
		return
	}
	for _, e := range w.Events {
		printEventLine(e)
	}
}

func printPlanEvents(raw json.RawMessage) {
	var es []store.PlanEvent
	_ = json.Unmarshal(raw, &es)
	for _, e := range es {
		printEventLine(e)
	}
}

func printEventLine(e store.PlanEvent) {
	who := e.Actor
	if e.Principal != "" {
		who = e.Principal + "@" + e.Actor
	}
	appr := ""
	if e.Approval {
		appr = " ✓approval"
	}
	fmt.Printf("#%d %s %s %s(%s)%s %s %s  %s\n", e.Seq, time.UnixMilli(e.TS).Format("15:04:05"), e.PlanID, who, e.ActorKind, appr, e.Op, e.Item, planmd.Change(e))
}
