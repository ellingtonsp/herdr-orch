// horch is the herdr-orch command line: agents and humans use it to talk to the daemon.
// Every command takes --json and exits non-zero on refusal.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/client"
	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/rpc"
	"github.com/ellingtonsp/herdr-orch/internal/store"
	"github.com/ellingtonsp/herdr-orch/skills"
)

const usage = `horch — herdr orchestration (mailbox, tasks, dispatch, gates, workers, schedules)

  horch run create --title T [--auto-dispatch] [--max-attempts N]
  horch run use <run> | current | list | show [--run R]
  horch run update [--run R] [--status S] [--coordinator me|PANE] [--auto on|off] [--max-attempts N]
                   [--idle-report 10m] [--flag-idle 30m|0]

  horch send --to coordinator|run|human|PANE|NAME [--subject S] --body B [--kind note|question|escalation]
  horch done [--task T] --body B [--failed]          (report a dispatched task finished)
  horch check [--wait] [--timeout 10m] [--peek]      horch inbox [--all] [--limit N]
  horch ask [--to coordinator] --question Q [--timeout 30m]   (blocks until replied)
  horch reply --id MSG --body B

  horch task create --title T [--spec S | --spec-file F] [--deps a,b]
  horch task list | show --id T | update --id T [--status S] [--deps a,b] [--reset-attempts] ...
  horch dispatch --task T --to PANE|NAME|new:<agent> [--cwd DIR]     horch dispatch --spec S --to ...
  horch dispatch show --task T | --id D              horch dispatch next [--run R]
  horch dispatch nudge --task T [--text S] [--force]  (remind an idle worker to report)
  horch dispatch fail --task T --reason R            (coordinator declares it failed)

  horch worker start --agent claude|pi|codex [--worktree DIR | --pane PANE] [--name N] [-- agent args]
  horch worker register [--pane PANE] [--name N]
  horch worker show|read|stop|release|retain|abandon --worker PANE|NAME [--force] [--lines N] [--off]
  horch worker list [--all]

  horch gate create [--task T] --question Q [--options a,b] [--timeout 1h]
  horch gate resolve --id G --decision D             horch gate list [--all]

  horch schedule add --cron "*/30 * * * *" (--horch "dispatch next" | --prompt-to NAME --prompt TEXT)
  horch schedule list | rm --id S | run --id S | enable --id S [--off]

  horch plan import --day D --file PLAN.md [--ref COMMIT] [--status draft|live] [--replace]
  horch plan show [--day D]                          (default: the project's latest plan)
  horch plan item add --item ID [--position N] [--issues a,b] [--kind K] [--lane L] [--model M]
                      [--state S] [--pr P] [--dispatch-ref R] [--title T] [--where W] [--output O] [--why Y]
  horch plan item update --item ID [field flags as for add] [--if-version N]
  horch plan item move --item ID --to N | hold --item ID --reason R | release --item ID | remove --item ID
  horch plan transition --item ID --state S [--note N] [--pr P] [--dispatch-ref R]   (orchestrator path)
  horch plan events [--since SEQ] [--follow] [--all]   horch plan status --set draft|live|final
  horch plan export [--day D] [--format md|json] [--out FILE] [--finalize]
    plan flags: --project P (default: config default_project), --principal NAME ($HORCH_PRINCIPAL),
    --if-version N (item) / --if-plan-version N (plan): refused if stale.
  horch web [--listen 127.0.0.1:7171]              (embedded fleet UI; Ctrl-C stops HTTP only)
  horch config          show ~/.config/horch/config.toml ($HORCH_CONFIG) and what is missing

  horch status          daemon health          horch daemon stop
  horch guide           print the agent guide (SKILL.md)
  horch version

Global: --json (machine output), --as PANE (act as another identity; default $HERDR_PANE_ID).
`

var (
	jsonOut bool
	cli     *client.Client
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Println(daemon.Version)
		return
	}
	if args[0] == "guide" {
		fmt.Print(skills.Horch)
		return
	}
	var err error
	cli, err = client.New()
	if err != nil {
		fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := dispatch(ctx, args); err != nil {
		fail(err)
	}
}

func fail(err error) {
	var re *rpc.Error
	code, msg := "error", err.Error()
	if errors.As(err, &re) {
		code, msg = re.Code, re.Message
	}
	if jsonOut {
		b, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]string{"code": code, "message": msg}})
		fmt.Println(string(b))
	} else {
		fmt.Fprintf(os.Stderr, "horch: %s: %s\n", code, msg)
	}
	if code == "usage" {
		os.Exit(2)
	}
	os.Exit(1)
}

func usageErr(format string, a ...any) error {
	return &rpc.Error{Code: "usage", Message: fmt.Sprintf(format, a...)}
}

// fs builds a flag set with the global flags.
func fs(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.BoolVar(&jsonOut, "json", false, "")
	f.Func("as", "", func(s string) error { cli.Caller = s; return nil })
	return f
}

// parse parses flags, allowing one leading positional argument (e.g. `run use r1`).
func parse(f *flag.FlagSet, args []string) (string, error) {
	pos := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos, args = args[0], args[1:]
	}
	if err := f.Parse(args); err != nil {
		return "", usageErr("%s: %v", f.Name(), err)
	}
	if pos == "" && f.NArg() > 0 {
		pos = f.Arg(0)
	}
	return pos, nil
}

func call(ctx context.Context, op string, args any) (json.RawMessage, error) {
	var out json.RawMessage
	err := cli.Call(ctx, op, args, &out)
	return out, err
}

func emit(raw json.RawMessage, human func(json.RawMessage)) {
	if jsonOut {
		fmt.Println(string(raw))
		return
	}
	human(raw)
}

func run(ctx context.Context, op string, args any, human func(json.RawMessage)) error {
	raw, err := call(ctx, op, args)
	if err != nil {
		return err
	}
	if human == nil {
		human = printJSON
	}
	emit(raw, human)
	return nil
}

func dispatch(ctx context.Context, args []string) error {
	cmd, rest := args[0], args[1:]
	sub := ""
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		sub = rest[0]
	}
	switch cmd {
	case "run":
		return cmdRun(ctx, sub, tail(rest))
	case "send":
		return cmdSend(ctx, rest)
	case "done":
		return cmdDone(ctx, rest)
	case "check":
		return cmdCheck(ctx, rest)
	case "ask":
		return cmdAsk(ctx, rest)
	case "reply":
		return cmdReply(ctx, rest)
	case "inbox":
		return cmdInbox(ctx, rest)
	case "task":
		return cmdTask(ctx, sub, tail(rest))
	case "dispatch":
		if sub == "show" || sub == "next" || sub == "nudge" || sub == "fail" {
			return cmdDispatchSub(ctx, sub, tail(rest))
		}
		return cmdDispatch(ctx, rest)
	case "worker":
		return cmdWorker(ctx, sub, tail(rest))
	case "gate":
		return cmdGate(ctx, sub, tail(rest))
	case "schedule":
		return cmdSchedule(ctx, sub, tail(rest))
	case "plan":
		return cmdPlan(ctx, sub, tail(rest))
	case "web":
		return cmdWeb(ctx, rest)
	case "config":
		return cmdConfig(rest)
	case "status":
		f := fs("status")
		if _, err := parse(f, rest); err != nil {
			return err
		}
		return run(ctx, "ping", nil, nil)
	case "daemon":
		f := fs("daemon")
		if _, err := parse(f, tail(rest)); err != nil {
			return err
		}
		if sub != "stop" {
			return usageErr("usage: horch daemon stop")
		}
		err := rpc.Call(ctx, cli.Paths.Sock, "shutdown", cli.Caller, nil, nil)
		if errors.Is(err, rpc.ErrUnavailable) {
			fmt.Println("daemon not running")
			return nil
		}
		if err == nil {
			fmt.Println("daemon stopping")
		}
		return err
	}
	return usageErr("unknown command %q (horch --help)", cmd)
}

func tail(a []string) []string {
	if len(a) > 0 && !strings.HasPrefix(a[0], "-") {
		return a[1:]
	}
	return a
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func durMS(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, usageErr("bad duration %q: %v", s, err)
	}
	return d.Milliseconds(), nil
}

func onOff(s string) (*bool, error) {
	switch s {
	case "":
		return nil, nil
	case "on", "true", "yes":
		v := true
		return &v, nil
	case "off", "false", "no":
		v := false
		return &v, nil
	}
	return nil, usageErr("expected on|off, got %q", s)
}

func strPtr(f *flag.FlagSet, name string, v string) *string {
	set := false
	f.Visit(func(fl *flag.Flag) {
		if fl.Name == name {
			set = true
		}
	})
	if !set {
		return nil
	}
	return &v
}

// ---------- run ----------

func cmdRun(ctx context.Context, sub string, args []string) error {
	f := fs("run " + sub)
	runID := f.String("run", "", "")
	title := f.String("title", "", "")
	auto := f.Bool("auto-dispatch", false, "")
	maxA := f.Int("max-attempts", 0, "")
	status := f.String("status", "", "")
	coord := f.String("coordinator", "", "")
	autoS := f.String("auto", "", "")
	idleReport := f.String("idle-report", "", "")
	flagIdle := f.String("flag-idle", "", "")
	pos, err := parse(f, args)
	if err != nil {
		return err
	}
	if pos != "" && *runID == "" {
		*runID = pos
	}
	switch sub {
	case "create":
		if *title == "" {
			*title = pos
		}
		return run(ctx, "run.create", daemon.RunCreateArgs{Title: *title, AutoDispatch: *auto, MaxAttempts: *maxA, Coordinator: *coord}, printRun)
	case "use":
		return run(ctx, "run.use", daemon.RunRef{Run: *runID}, printRun)
	case "current":
		return run(ctx, "run.current", nil, printRun)
	case "list":
		return run(ctx, "run.list", nil, printRuns)
	case "show", "":
		return run(ctx, "run.show", daemon.RunRef{Run: *runID}, printRunShow)
	case "update":
		a := daemon.RunUpdateArgs{Run: *runID, Status: strPtr(f, "status", *status), Coordinator: strPtr(f, "coordinator", *coord), Title: strPtr(f, "title", *title)}
		if a.AutoDispatch, err = onOff(*autoS); err != nil {
			return err
		}
		if a.AutoDispatch == nil && *auto {
			a.AutoDispatch = auto
		}
		if *maxA > 0 {
			a.MaxAttempts = maxA
		}
		if *idleReport != "" {
			ms, err := durMS(*idleReport)
			if err != nil {
				return err
			}
			a.IdleReportMS = &ms
		}
		if *flagIdle != "" {
			ms, err := durMS(*flagIdle)
			if err != nil {
				return err
			}
			a.IdleFlagMS = &ms
		}
		return run(ctx, "run.update", a, printRun)
	}
	return usageErr("unknown run subcommand %q", sub)
}

// ---------- messages ----------

func cmdSend(ctx context.Context, args []string) error {
	f := fs("send")
	a := daemon.SendArgs{}
	f.StringVar(&a.Run, "run", "", "")
	f.StringVar(&a.To, "to", "", "")
	f.StringVar(&a.Subject, "subject", "", "")
	f.StringVar(&a.Body, "body", "", "")
	f.StringVar(&a.Kind, "kind", "note", "")
	f.StringVar(&a.Task, "task", "", "")
	f.BoolVar(&a.Failed, "failed", false, "")
	if _, err := parse(f, args); err != nil {
		return err
	}
	if a.Kind == store.KindDone && a.To == "" {
		a.To = store.Daemon
	}
	if a.Body == "-" {
		b, _ := io.ReadAll(os.Stdin)
		a.Body = string(b)
	}
	return run(ctx, "send", a, printSent)
}

func cmdDone(ctx context.Context, args []string) error {
	f := fs("done")
	a := daemon.SendArgs{Kind: store.KindDone, To: store.Daemon}
	f.StringVar(&a.Task, "task", "", "")
	f.StringVar(&a.Body, "body", "", "")
	f.StringVar(&a.Subject, "subject", "", "")
	f.BoolVar(&a.Failed, "failed", false, "")
	if _, err := parse(f, args); err != nil {
		return err
	}
	if a.Body == "-" {
		b, _ := io.ReadAll(os.Stdin)
		a.Body = string(b)
	}
	return run(ctx, "send", a, func(raw json.RawMessage) {
		var r struct {
			Dispatch store.Dispatch `json:"dispatch"`
		}
		_ = json.Unmarshal(raw, &r)
		fmt.Printf("reported %s for task %s (dispatch %s is %s)\n", map[bool]string{true: "failure", false: "done"}[a.Failed], r.Dispatch.TaskID, r.Dispatch.ID, r.Dispatch.Status)
	})
}

func cmdCheck(ctx context.Context, args []string) error {
	f := fs("check")
	wait := f.Bool("wait", false, "")
	peek := f.Bool("peek", false, "")
	timeout := f.String("timeout", "", "")
	if _, err := parse(f, args); err != nil {
		return err
	}
	ms, err := durMS(*timeout)
	if err != nil {
		return err
	}
	return run(ctx, "check", daemon.CheckArgs{Wait: *wait, TimeoutMS: ms, Peek: *peek}, printMessages)
}

func cmdAsk(ctx context.Context, args []string) error {
	f := fs("ask")
	a := daemon.AskArgs{}
	f.StringVar(&a.Run, "run", "", "")
	f.StringVar(&a.To, "to", "coordinator", "")
	f.StringVar(&a.Question, "question", "", "")
	f.StringVar(&a.Task, "task", "", "")
	timeout := f.String("timeout", "", "")
	if _, err := parse(f, args); err != nil {
		return err
	}
	var err error
	if a.TimeoutMS, err = durMS(*timeout); err != nil {
		return err
	}
	return run(ctx, "ask", a, func(raw json.RawMessage) {
		var r struct {
			Reply store.Message `json:"reply"`
		}
		_ = json.Unmarshal(raw, &r)
		fmt.Println(r.Reply.Body)
	})
}

func cmdReply(ctx context.Context, args []string) error {
	f := fs("reply")
	a := daemon.ReplyArgs{}
	f.StringVar(&a.ID, "id", "", "")
	f.StringVar(&a.Body, "body", "", "")
	pos, err := parse(f, args)
	if err != nil {
		return err
	}
	if a.ID == "" {
		a.ID = pos
	}
	return run(ctx, "reply", a, func(raw json.RawMessage) {
		var m store.Message
		_ = json.Unmarshal(raw, &m)
		fmt.Printf("replied %s → %s\n", m.ID, m.To)
	})
}

func cmdInbox(ctx context.Context, args []string) error {
	f := fs("inbox")
	a := daemon.InboxArgs{}
	f.IntVar(&a.Limit, "limit", 30, "")
	f.BoolVar(&a.All, "all", false, "")
	f.StringVar(&a.Run, "run", "", "")
	if _, err := parse(f, args); err != nil {
		return err
	}
	return run(ctx, "inbox", a, printMessages)
}

// ---------- tasks ----------

func cmdTask(ctx context.Context, sub string, args []string) error {
	f := fs("task " + sub)
	runID := f.String("run", "", "")
	id := f.String("id", "", "")
	title := f.String("title", "", "")
	spec := f.String("spec", "", "")
	specFile := f.String("spec-file", "", "")
	deps := f.String("deps", "", "")
	status := f.String("status", "", "")
	result := f.String("result", "", "")
	reset := f.Bool("reset-attempts", false, "")
	pos, err := parse(f, args)
	if err != nil {
		return err
	}
	if *id == "" {
		*id = pos
	}
	if *specFile != "" {
		b, err := os.ReadFile(*specFile)
		if err != nil {
			return err
		}
		*spec = string(b)
	}
	switch sub {
	case "create":
		return run(ctx, "task.create", daemon.TaskCreateArgs{Run: *runID, Title: *title, Spec: *spec, Deps: splitList(*deps)}, printTask)
	case "list", "":
		return run(ctx, "task.list", daemon.RunRef{Run: *runID}, printTasks)
	case "show":
		return run(ctx, "task.show", daemon.TaskRef{ID: *id}, nil)
	case "update":
		a := daemon.TaskUpdateArgs{ID: *id, Title: strPtr(f, "title", *title), Status: strPtr(f, "status", *status), Result: strPtr(f, "result", *result), ResetAttempts: *reset}
		if s := strPtr(f, "spec", *spec); s != nil || *specFile != "" {
			a.Spec = spec
		}
		if strPtr(f, "deps", *deps) != nil {
			d := splitList(*deps)
			if d == nil {
				d = []string{}
			}
			a.Deps = &d
		}
		return run(ctx, "task.update", a, printTask)
	}
	return usageErr("unknown task subcommand %q", sub)
}

// ---------- dispatch ----------

func cmdDispatch(ctx context.Context, args []string) error {
	f := fs("dispatch")
	a := daemon.DispatchArgs{}
	f.StringVar(&a.Run, "run", "", "")
	f.StringVar(&a.Task, "task", "", "")
	f.StringVar(&a.To, "to", "", "")
	f.StringVar(&a.Title, "title", "", "")
	f.StringVar(&a.Spec, "spec", "", "")
	f.StringVar(&a.Cwd, "cwd", "", "")
	f.StringVar(&a.Cwd, "worktree", "", "")
	f.StringVar(&a.Name, "name", "", "")
	if _, err := parse(f, args); err != nil {
		return err
	}
	return run(ctx, "dispatch", a, printDispatch)
}

func cmdDispatchSub(ctx context.Context, sub string, args []string) error {
	f := fs("dispatch " + sub)
	task := f.String("task", "", "")
	id := f.String("id", "", "")
	runID := f.String("run", "", "")
	reason := f.String("reason", "", "")
	text := f.String("text", "", "")
	force := f.Bool("force", false, "")
	pos, err := parse(f, args)
	if err != nil {
		return err
	}
	if *task == "" {
		*task = pos
	}
	switch sub {
	case "next":
		return run(ctx, "dispatch.next", daemon.RunRef{Run: *runID}, printDispatches)
	case "nudge":
		return run(ctx, "dispatch.nudge", daemon.DispatchActArgs{Task: *task, Text: *text, Force: *force}, func(raw json.RawMessage) {
			var r struct {
				Dispatch store.Dispatch `json:"dispatch"`
				Nudges   int            `json:"nudges"`
			}
			_ = json.Unmarshal(raw, &r)
			fmt.Printf("nudged %s on task %s (nudge %d)\n", r.Dispatch.PaneID, r.Dispatch.TaskID, r.Nudges)
		})
	case "fail":
		return run(ctx, "dispatch.fail", daemon.DispatchActArgs{Task: *task, Reason: *reason}, func(raw json.RawMessage) {
			var st store.Settlement
			_ = json.Unmarshal(raw, &st)
			fmt.Printf("dispatch %s %s; task %s is %s (attempt %d)\n", st.Dispatch.ID, st.Dispatch.Status, st.Task.ID, st.Task.Status, st.Task.Attempts)
		})
	}
	return run(ctx, "dispatch.show", daemon.DispatchShowArgs{Task: *task, ID: *id}, nil)
}

// ---------- workers ----------

func cmdWorker(ctx context.Context, sub string, args []string) error {
	var extra []string
	for i, a := range args {
		if a == "--" {
			extra, args = args[i+1:], args[:i]
			break
		}
	}
	f := fs("worker " + sub)
	runID := f.String("run", "", "")
	worker := f.String("worker", "", "")
	agent := f.String("agent", "", "")
	name := f.String("name", "", "")
	cwd := f.String("worktree", "", "")
	f.StringVar(cwd, "cwd", "", "")
	pane := f.String("pane", "", "")
	force := f.Bool("force", false, "")
	lines := f.Int("lines", 0, "")
	off := f.Bool("off", false, "")
	all := f.Bool("all", false, "")
	pos, err := parse(f, args)
	if err != nil {
		return err
	}
	if *worker == "" {
		*worker = pos
	}
	ref := daemon.WorkerRef{Worker: *worker, Force: *force, Lines: *lines, Off: *off}
	switch sub {
	case "start":
		return run(ctx, "worker.start", daemon.WorkerStartArgs{Run: *runID, Agent: *agent, Name: *name, Cwd: *cwd, Args: extra, Pane: *pane}, printWorker)
	case "register":
		if *pane == "" {
			*pane = pos
		}
		return run(ctx, "worker.register", daemon.WorkerRegisterArgs{Run: *runID, Pane: *pane, Name: *name, Worktree: *cwd}, printWorker)
	case "list", "":
		return run(ctx, "worker.list", daemon.WorkerListArgs{Run: *runID, All: *all}, printWorkers)
	case "read":
		return run(ctx, "worker.read", ref, func(raw json.RawMessage) {
			var r struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(raw, &r)
			fmt.Println(r.Text)
		})
	case "show", "retain", "abandon":
		return run(ctx, "worker."+sub, ref, nil)
	case "stop", "release":
		return run(ctx, "worker."+sub, ref, func(raw json.RawMessage) {
			var r daemon.CloseResult
			_ = json.Unmarshal(raw, &r)
			fmt.Printf("%s %s: pane closed, %d process(es) verified gone", r.Worker.PaneID, r.Worker.State, len(r.PIDs))
			if len(r.Killed) > 0 {
				fmt.Printf(" (signalled %v)", r.Killed)
			}
			if r.ArchivePath != "" {
				fmt.Printf("; transcript %s", r.ArchivePath)
			}
			fmt.Println()
		})
	}
	return usageErr("unknown worker subcommand %q", sub)
}

// ---------- gates ----------

func cmdGate(ctx context.Context, sub string, args []string) error {
	f := fs("gate " + sub)
	runID := f.String("run", "", "")
	task := f.String("task", "", "")
	question := f.String("question", "", "")
	options := f.String("options", "", "")
	timeout := f.String("timeout", "", "")
	id := f.String("id", "", "")
	decision := f.String("decision", "", "")
	all := f.Bool("all", false, "")
	pos, err := parse(f, args)
	if err != nil {
		return err
	}
	if *id == "" {
		*id = pos
	}
	switch sub {
	case "create":
		ms, err := durMS(*timeout)
		if err != nil {
			return err
		}
		return run(ctx, "gate.create", daemon.GateCreateArgs{Run: *runID, Task: *task, Question: *question, Options: splitList(*options), TimeoutMS: ms}, printGate)
	case "resolve":
		return run(ctx, "gate.resolve", daemon.GateResolveArgs{ID: *id, Decision: *decision}, printGate)
	case "list", "":
		return run(ctx, "gate.list", daemon.GateListArgs{Run: *runID, All: *all}, printGates)
	}
	return usageErr("unknown gate subcommand %q", sub)
}

// ---------- schedules ----------

func cmdSchedule(ctx context.Context, sub string, args []string) error {
	f := fs("schedule " + sub)
	cron := f.String("cron", "", "")
	horchArgs := f.String("horch", "", "")
	promptTo := f.String("prompt-to", "", "")
	prompt := f.String("prompt", "", "")
	id := f.String("id", "", "")
	off := f.Bool("off", false, "")
	pos, err := parse(f, args)
	if err != nil {
		return err
	}
	if *id == "" {
		*id = pos
	}
	switch sub {
	case "add":
		a := daemon.ScheduleAddArgs{Cron: *cron}
		if *horchArgs != "" {
			words, err := shellSplit(*horchArgs)
			if err != nil {
				return err
			}
			a.Action = daemon.Action{Type: "horch", Args: words}
		} else {
			a.Action = daemon.Action{Type: "prompt", To: *promptTo, Text: *prompt}
		}
		return run(ctx, "schedule.add", a, nil)
	case "list", "":
		return run(ctx, "schedule.list", nil, nil)
	case "rm":
		return run(ctx, "schedule.rm", daemon.ScheduleRef{ID: *id}, nil)
	case "run":
		return run(ctx, "schedule.run", daemon.ScheduleRef{ID: *id}, nil)
	case "enable":
		return run(ctx, "schedule.enable", daemon.ScheduleRef{ID: *id, Off: *off}, nil)
	}
	return usageErr("unknown schedule subcommand %q", sub)
}

// shellSplit splits a command line on whitespace, honouring single and double quotes.
func shellSplit(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	var quote rune
	inWord := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, usageErr("unbalanced quote in %q", s)
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}
