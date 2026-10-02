---
name: horch
description: >-
  Coordinate supervised agent workers inside herdr with `horch`: threaded messages,
  blocking ask/reply, tracked task dispatch that settles on real agent state plus the
  worker's own report, task DAGs, human decision gates, worker start/release with
  verified process cleanup, and cron schedules. Use when asked to orchestrate,
  supervise, fan out work to other agents, or coordinate a DAG inside herdr.
  Requires HERDR_ENV=1.
---

# horch — herdr orchestration

`horch` talks to the herdr-orch daemon (one per herdr session). The daemon owns all state
(SQLite), follows herdr's real agent status (`idle | working | blocked | done | unknown`)
and settles dispatches for you. You never poll: block on `horch check --wait`.

Every command takes `--json` and exits non-zero on refusal (`{"ok":false,"error":{code,message}}`).
Your identity is your pane (`$HERDR_PANE_ID`).

## Coordinator loop

```bash
horch run create --title "Refactor auth"            # you become the coordinator
horch task create --title "Extract token store" --spec-file /tmp/spec-a.md
horch task create --title "Migrate callers" --deps t1 --spec "..."
horch gate create --task t2 --question "Merge t1 first?" --options yes,no   # optional human gate

horch worker start --agent claude --worktree ~/.herdr/worktrees/auth-a --name auth-a
horch worker start --agent codex  --worktree ~/.herdr/worktrees/auth-b --name auth-b
horch dispatch --task t1 --to auth-a                 # or --to new:codex --worktree DIR

horch check --wait --timeout 30m                     # blocks until something happens
```

Messages you receive as coordinator:

| kind | meaning | what to do |
|---|---|---|
| `done` | a dispatch settled `completed`; body has the worker's summary and newly ready tasks | review, dispatch the next task |
| `escalation` | worker idle without reporting (dispatch still open), dispatch failed (`pane_exited`, worker-reported, `dispatch fail`), circuit broken, worker blocked at a prompt, gate timed out, unsaved work blocked a release, worktree removed under a worker | read it, `horch worker read --worker X`, decide |
| `question` | a worker is blocked in `horch ask` | `horch reply --id mN --body "..."` |
| `note` | gate resolved, dispatch fenced, idle worker flagged | informational |

With `horch run update --auto on`, ready tasks go to idle, unretained workers of the run
automatically (oldest task first). `horch dispatch next` does one round by hand. Retain a
worker (`horch worker retain`) to reserve it, e.g. a PR owner waiting on its merge.
`horch run update --flag-idle 30m` opts in to a note about unretained workers left without
work.

## Settlement rules (what "done" means)

A dispatch settles only when **both** hold:
1. the worker ran `horch done --task tN ...`, and
2. herdr reports the agent is no longer working/blocked.

- **Silence never fails a dispatch.** A worker that went `working → idle` without reporting
  for the run's idle-report delay (default 3 min; `horch run update --idle-report 20m`)
  produces one `escalation` to the coordinator per idle episode, with the last lines of its
  screen. The dispatch stays open and nothing is re-dispatched — long loops, CI waits and
  background jobs look exactly like this. Decide on your next tick:
  `horch dispatch nudge --task tN` (asks it to report or say what it is waiting on; refused
  while it is working) or `horch dispatch fail --task tN --reason "..."` (counts the attempt).
  A late `horch done` always settles normally.
- No activity at all for 90s → one escalation ("prompt may not have landed"). horch never
  re-sends a prompt on its own; check the pane first.
- Pane closed or exited → `failed: pane_closed|pane_exited`.
- A failed dispatch returns the task to `ready`; after `max_attempts` (default 3) the last
  dispatch is `circuit_broken` and the task `failed`. Retry with
  `horch task update --id tN --reset-attempts --status ready`.
- `horch dispatch show --task tN` shows live status and flags state/report mismatches.

## Worker side

The dispatch prompt tells the worker exactly what to run. As a worker:

```bash
horch done --task t3 --body "Extracted TokenStore; branch auth-a, commit abc123, PR #42"
horch done --task t3 --failed --body "tests need a DB fixture that doesn't exist"
horch ask --question "Keep the legacy cookie path?"   # blocks until replied (default 2h)
horch send --to coordinator --kind escalation --body "found a security issue in ..."
```

## Workers

```bash
horch worker start --agent claude|pi|codex --worktree DIR [--name N] [-- agent args]   # new tab
horch worker start --agent codex --pane w4:p1 --name n1-1 -- --model ID  # existing shell pane,
                                              # e.g. the root pane from `herdr worktree create`
horch worker register --pane w1:p4            # adopt an agent pane you already have
horch worker list | show | read --worker N [--lines 200]
horch worker release --worker N               # archive transcript, close pane, verify every process is gone
horch worker stop --worker N                  # fence its dispatch (attempt not counted) + close
horch worker abandon --worker N               # fence and stop supervising; pane left open
horch worker retain --worker N [--off]        # keep an idle worker without being nagged
```

`release`/`stop` **refuse** when the worktree has uncommitted changes or commits not on any
remote (`unsaved_work`) and escalate instead; `--force` overrides. Release never reports
success while a process from the pane is alive (it escalates to SIGTERM/SIGKILL and reports
`release_failed` otherwise). A worker stuck at a startup prompt (folder trust) is escalated;
horch never answers such prompts.

## Gates and schedules

```bash
horch gate create --task t4 --question "Deploy to prod?" --options approve,reject --timeout 2h
horch gate list ;  horch gate resolve --id g1 --decision approve
horch schedule add --cron "*/30 * * * *" --horch "dispatch next --run r1"
horch schedule add --cron "@hourly" --prompt-to reviewer --prompt "Re-check open PRs"
horch schedule list | run --id s1 | enable --id s1 --off | rm --id s1
```

A gate blocks its task until resolved (from the CLI or the herdr "resolve gate" popup);
a timed-out gate escalates and keeps the task blocked.

## Day plan

The published `<date>.plan.md` is the start and end of the day. In between, the plan lives in
horch, and every change goes through it so subscribers see it live.

```bash
horch plan import --day 2026-03-14 --file docs/orchestration/2026-03-14.plan.md --ref <commit>
horch plan show                                   # latest plan of the default project
horch plan transition --item B1 --state dispatched --dispatch-ref horch:r2/t15/d15@w1J:p1
horch plan transition --item B1 --state settled --pr '#214' --note "PR up"
horch plan item add --item B2 --issues ACME-51 --kind build --lane local --title "slice b" --position 3
horch plan item hold --item I1 --reason "sim farm down"     # release --item I1 to resume
horch plan item move --item W1 --to 1 ;  horch plan item remove --item U3 --note "folded into R3"
horch plan events --follow --json                 # NDJSON stream of every change
horch plan export --day 2026-03-14 --format md --finalize --out docs/orchestration/2026-03-14.plan.md
```

- States: `planned dispatched settled bounced ratified merged held replanned dropped`.
- Import on approval. Re-importing the same file is a no-op. A different file needs `--replace`.
- The orchestrator records progress with `plan transition`, never by editing the file mid-day.
  A held item refuses transitions until released.
- Every event carries `actor`, `actor_kind` (`human|orchestrator|worker`), `principal` and
  `approval`. Only edits by the configured owner (`owner.principal` in
  `~/.config/horch/config.toml`) have `approval: true`. Treat those as the owner's approval
  of that change. Orchestrator writes never are.
- Read-modify-write with the version you read: `--if-version N` on item commands and
  `--if-plan-version N` on `item add` / `plan status`. `version_conflict` means reload and
  re-decide; do not retry blindly.
- Owner edits arrive in the coordinator's inbox as `plan replanned by <owner>: …`.
- Every plan command takes `--project P` (default: `default_project`) and `--day D`
  (default: latest), and the owner can act from a pane with `--principal NAME`.

## Coming from Orca orchestration

| Orca | horch |
|---|---|
| `orchestration run create/current` | `horch run create/current/use` |
| send / check / ask / reply | same names |
| `worker_done` message | `horch done` (settles the dispatch) |
| task create/list/update, deps | same, `--deps a,b` |
| dispatch + wait | `horch dispatch` + `horch check --wait` |
| decision gates | `horch gate create/resolve/list` |
| worker-release | `horch worker release` (closes the pane, verifies its processes exited) |

Status values match Orca's, so Orca-based skills port with renames: runs `idle|running|completed|failed`, tasks
`pending|ready|dispatched|completed|failed|blocked`, dispatches
`pending|dispatched|completed|failed|circuit_broken`, gates `pending|resolved|timeout`.

horch does not create or delete worktrees: use `herdr worktree create` (or git) and pass the path
or the new workspace's pane to `horch worker start`.
