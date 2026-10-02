package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

func printJSON(raw json.RawMessage) {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		fmt.Println(string(raw))
		return
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func ago(ms int64) string {
	if ms == 0 {
		return "-"
	}
	d := time.Since(time.UnixMilli(ms)).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func tw() *tabwriter.Writer { return tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0) }

func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func printRun(raw json.RawMessage) {
	var r store.Run
	_ = json.Unmarshal(raw, &r)
	fmt.Printf("%s  %s  [%s]  coordinator=%s  auto-dispatch=%v  max-attempts=%d\n", r.ID, r.Title, r.Status, or(r.CoordinatorPaneID, "human"), r.AutoDispatch, r.MaxAttempts)
}

func printRuns(raw json.RawMessage) {
	var rs []store.Run
	_ = json.Unmarshal(raw, &rs)
	w := tw()
	fmt.Fprintln(w, "RUN\tSTATUS\tCOORDINATOR\tAGE\tTITLE")
	for _, r := range rs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Status, or(r.CoordinatorPaneID, "human"), ago(r.CreatedAt), r.Title)
	}
	w.Flush()
}

func printRunShow(raw json.RawMessage) {
	var s daemon.RunShow
	_ = json.Unmarshal(raw, &s)
	printRun(mustJSON(s.Run))
	fmt.Println()
	printTasks(mustJSON(s.Tasks))
	if len(s.Gates) > 0 {
		fmt.Println()
		printGates(mustJSON(s.Gates))
	}
	if len(s.Workers) > 0 {
		fmt.Println()
		printWorkers(mustJSON(s.Workers))
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func printTask(raw json.RawMessage) {
	var t store.Task
	_ = json.Unmarshal(raw, &t)
	fmt.Printf("%s  %s  [%s]", t.ID, t.Title, t.Status)
	if len(t.Deps) > 0 {
		fmt.Printf("  deps=%s", strings.Join(t.Deps, ","))
	}
	fmt.Println()
}

func printTasks(raw json.RawMessage) {
	var ts []store.Task
	_ = json.Unmarshal(raw, &ts)
	w := tw()
	fmt.Fprintln(w, "TASK\tSTATUS\tASSIGNEE\tTRIES\tDEPS\tTITLE")
	for _, t := range ts {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", t.ID, t.Status, or(t.AssigneePaneID, "-"), t.Attempts, or(strings.Join(t.Deps, ","), "-"), t.Title)
	}
	w.Flush()
}

func printDispatch(raw json.RawMessage) {
	var d store.Dispatch
	_ = json.Unmarshal(raw, &d)
	fmt.Printf("dispatched task %s to %s as %s [%s] %s\n", d.TaskID, d.PaneID, d.ID, d.Status, d.Outcome)
}

func printDispatches(raw json.RawMessage) {
	var ds []store.Dispatch
	_ = json.Unmarshal(raw, &ds)
	if len(ds) == 0 {
		fmt.Println("nothing dispatched (no ready task or no idle worker)")
	}
	for _, d := range ds {
		printDispatch(mustJSON(d))
	}
}

func printMessages(raw json.RawMessage) {
	var ms []store.Message
	_ = json.Unmarshal(raw, &ms)
	if len(ms) == 0 {
		fmt.Println("no messages")
		return
	}
	for _, m := range ms {
		head := fmt.Sprintf("── %s %s from %s to %s", m.ID, m.Kind, m.From, m.To)
		if m.TaskID != "" {
			head += " · task " + m.TaskID
		}
		if m.ReplyTo != "" {
			head += " · re " + m.ReplyTo
		}
		head += " · " + ago(m.CreatedAt) + " ago"
		fmt.Println(head)
		if m.Subject != "" {
			fmt.Println(m.Subject)
		}
		if m.Body != "" {
			fmt.Println(m.Body)
		}
		if m.Kind == store.KindQuestion {
			fmt.Printf("(answer with: horch reply --id %s --body \"...\")\n", m.ID)
		}
	}
}

func printSent(raw json.RawMessage) {
	var ms []store.Message
	if json.Unmarshal(raw, &ms) != nil {
		printJSON(raw)
		return
	}
	for _, m := range ms {
		fmt.Printf("sent %s (%s) → %s\n", m.ID, m.Kind, m.To)
	}
}

func printWorker(raw json.RawMessage) {
	var w store.Worker
	_ = json.Unmarshal(raw, &w)
	fmt.Printf("%s  %s  %s  [%s]  %s\n", w.PaneID, or(w.Name, "-"), or(w.Agent, "-"), w.State, w.Worktree)
}

func printWorkers(raw json.RawMessage) {
	var ws []store.Worker
	_ = json.Unmarshal(raw, &ws)
	w := tw()
	fmt.Fprintln(w, "PANE\tNAME\tAGENT\tSTATE\tRUN\tAGE\tWORKTREE")
	for _, x := range ws {
		state := x.State
		if x.Retained {
			state += "+retained"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", x.PaneID, or(x.Name, "-"), or(x.Agent, "-"), state, or(x.RunID, "-"), ago(x.StartedAt), or(x.Worktree, "-"))
	}
	w.Flush()
}

func printGate(raw json.RawMessage) {
	var g store.Gate
	_ = json.Unmarshal(raw, &g)
	fmt.Printf("%s  [%s]  task=%s  %q", g.ID, g.Status, or(g.TaskID, "-"), g.Question)
	if len(g.Options) > 0 {
		fmt.Printf("  options=%s", strings.Join(g.Options, "/"))
	}
	if g.Decision != "" {
		fmt.Printf("  decision=%s by %s", g.Decision, g.DecidedBy)
	}
	fmt.Println()
}

func printGates(raw json.RawMessage) {
	var gs []store.Gate
	_ = json.Unmarshal(raw, &gs)
	if len(gs) == 0 {
		fmt.Println("no gates")
	}
	for _, g := range gs {
		printGate(mustJSON(g))
	}
}
