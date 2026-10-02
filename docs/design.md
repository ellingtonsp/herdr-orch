# herdr-orch design

## Goal

Give herdr the coordination layer that multi-agent work needs — a mailbox, tasks with
dependencies, tracked dispatch, decision gates, supervised workers and schedules — as one
plugin, built on what herdr already does well: it knows each agent's real lifecycle state
(`idle | working | blocked | done | unknown`) and streams it as events.

## Principles

1. **One writer.** Only the daemon writes to SQLite. The `horch` CLI, the board and the gate
   popup go through the daemon's unix socket. No multi-writer races to guard against, and no
   state that lives only in memory: the daemon can be restarted at any time.
2. **Settle on evidence, not silence.** A dispatch completes when the worker reports
   (`horch done`) *and* herdr shows it is no longer working. A worker that goes quiet is reported
   to the coordinator, never failed or re-dispatched automatically. Long loops, CI waits and
   background jobs are indistinguishable from "forgot to report", so the decision belongs to
   the coordinator (`dispatch nudge`, `dispatch fail`).
3. **Fail only on positive evidence:** the worker's own `--failed`, its pane closing or exiting,
   a stop, or the coordinator's `dispatch fail`. Each counts toward a per-task circuit breaker.
4. **Never claim a release that left a process alive.** Release records the pane's process ids,
   closes the pane, waits, escalates to SIGTERM then SIGKILL, and reports `release_failed` with
   the survivors otherwise. It refuses when the worktree has uncommitted or unpushed work.
5. **Escalate, don't guess.** Blocked prompts, startup dialogs, prompts that never landed and
   worktrees removed under a live worker all go to the coordinator (and, where urgent, a herdr
   notification). horch never answers an agent's dialog or re-sends a prompt on its own.

## Architecture

```
herdr server
  ├─ [[startup]]  herdr-orch daemon     events.subscribe, SQLite, timers, orch.sock
  ├─ [[panes]]    herdr-orch board      runs → tasks → workers → gates → inbox
  ├─ [[panes]]    herdr-orch gate       popup: resolve a pending decision gate
  └─ [[actions]]  board / resolve gate / dispatch next / install CLI
agents ──► horch ──► $STATE/sessions/<session>/orch.sock ──► daemon ──► herdr socket API
```

- **State:** `$HERDR_PLUGIN_STATE_DIR/sessions/<session>/orch.db` (WAL). herdr's plugin state
  directory is shared across herdr sessions, so everything is namespaced by session.
- **Session identity:** `HERDR_SOCKET_PATH` decides the session (agent helpers can carry a stale
  `HERDR_SESSION`).
- **Singleton:** an exclusive `flock` on `orch.lock`. A new daemon asks the old one to exit and
  takes over (the newest copy is attached to the live server after a handoff). The daemon exits
  when its lock or socket file is replaced, or herdr has been unreachable for 30s, because herdr
  never reaps plugin startup processes.
- **Events:** one `events.subscribe` connection holds global pane/worktree events plus a
  per-pane `pane.agent_status_changed` subscription for every watched pane. It is rebuilt when
  the watched set changes, and statuses are re-synced after every (re)connect.
- **Delivery:** `agent.prompt` for agents herdr manages (retried while a freshly started agent
  becomes interactive); typed text + Enter for panes herdr does not manage as named agents.

## Data model

| Table | Holds |
|---|---|
| `runs` | title, coordinator pane, status, auto-dispatch, max attempts, idle-report and idle-flag delays |
| `messages` | from, to, kind (`note, question, reply, done, escalation`), task, reply-to, read state |
| `tasks` | spec, deps, status, assignee, attempts, result |
| `dispatches` | task, pane, status, observed activity, done report, idle/unobserved escalation flags, nudges, outcome |
| `gates` | task, question, options, decision, timeout |
| `workers` | pane, run, agent, worktree, state (`live, released, stopped, abandoned, exited, release_failed`), retained |
| `schedules`, `schedule_runs` | cron spec, action, history |

Ids are short and typeable (`r1`, `t7`, `d9`, `m42`, `g1`, `s1`).

## Day plans

The day plan has three phases. Only the middle one lives in horch.

1. **Publish (the orchestrate skill, unchanged).** The skill writes the proposal, the owner
   approves it, and `<date>.plan.md` is committed on the day-log branch.
2. **Live (horch).** On approval, `horch plan import --day D --file <date>.plan.md --ref <commit>`
   seeds the plan store. From then on every change goes through the store: orchestrator
   transitions (`horch plan transition`) and the owner's re-order, hold, release, add and remove
   (`horch plan item …`). Each write appends to an event log that subscribers receive as it happens.
3. **Finalize (the skill, new mechanism).** At EOD,
   `horch plan export --day D --format md --finalize --out <date>.plan.md` renders the plan plus
   its event log back into the file, and the skill commits it. `final` freezes the plan.

| Table | Holds |
|---|---|
| `plans` | id `<project>/<day>`, published ref + source hash, status (`draft, live, final`), coordinator pane, the file's other sections (verbatim), version |
| `plan_items` | stable id, position, issues, kind, lane, model, state, PR, dispatch ref, title, What row (issues/where/model/output), why, held + reason (+ state held from), listed, version |
| `plan_decisions` | the "Decisions that come back" bullets |
| `plan_events` | seq, time, actor (pane or `human`), actor kind (`human, orchestrator, worker`), principal, approval, op, item, note, before/after JSON, plan version. **Append-only**: triggers refuse UPDATE and DELETE |

Item states: `planned dispatched settled bounced ratified merged held replanned dropped`.

- **Markdown.** `internal/planmd` parses the Items, What, Why, "Held on purpose" and Decisions
  sections into rows and keeps every other section verbatim. Exporting a freshly imported
  plan reproduces the published file byte for byte, and an export (with its event log)
  imports back to the same plan. Held rows that are not Items rows become unlisted held
  items: they appear only under "Held on purpose" until released.
- **Import is idempotent.** Re-importing the same file is a no-op and keeps live edits. A
  different file for the same day is refused unless `--replace`, which reseeds the items and
  keeps the event log.
- **Identity and approval.** Every write records the caller (`--as` / `$HORCH_AS` / the pane /
  `human`). The daemon decides the actor kind and approval; the caller never does. A named
  principal (`--principal` / `$HORCH_PRINCIPAL`), or a terminal outside herdr (which acts for the
  configured owner), is `human`. A registered worker pane, or a pane with an active dispatch,
  is `worker`. Any other pane is `orchestrator`. Only writes by `owner.principal` from
  `~/.config/horch/config.toml` carry `approval: true`. That flag is what the orchestrate skill
  counts as the owner's approval of a steer, not commit authorship.
- **Optimistic concurrency.** Items and the plan carry versions. Writes with
  `--if-version` (item) or `--if-plan-version` (plan) are refused with `version_conflict` when
  stale, so a stale UI or CLI never overwrites a newer change. Every write bumps the item's
  version and the plan's.
- **Guards.** A held item cannot be transitioned (except to `dropped`/`replanned`) until released.
  Holding goes through `item hold`, never `--state held`. A `final` plan refuses all writes.
- **Fan-out.** Writes publish their events to an in-daemon hub. `plan.subscribe` is a streaming
  op on orch.sock: the backlog after `--since`, then one line per event as it is committed.
  Pushes, not polls. `horch plan events --follow` resumes from the last seq if the daemon
  restarts. A subscriber more than 256 events behind is dropped and reconnects without loss.
  `plan.events --wait` is the long-poll equivalent.
- **Notifications.** A human edit (not import) messages the plan's coordinator (the pane that
  imported it, else the newest running run's coordinator): `plan replanned by <principal>: …`,
  saying whether it counts as the owner's approval.

### Per-user config

`~/.config/horch/config.toml` (`$HORCH_CONFIG` overrides; `config.example.toml` documents it):
owner principal, default project, default agent kind/model/Codex effort per role, slot limits,
per-project plan-file path and day-log branch patterns (`<date>` placeholder), and integrations
(Linear team). Secrets are never in the file. The loader refuses secret-looking keys without
echoing their values; `LinearAPIKey` reads `$LINEAR_API_KEY` (or the configured env var) or
the macOS Keychain, and nothing prints it. The daemon re-reads the file on each plan write, so
an owner change applies without a restart. `horch config` shows the file and what is missing
(the web UI's first-run screen uses the same list).

## Out of scope

- Multi-machine coordination: every agent runs against one herdr server.
- Browser, simulator and desktop automation.
- Owning git worktrees: herdr (`herdr worktree create`) or git does that. horch only refuses to
  release work that is unsaved, and reports a worktree removed under a live worker.

## Risks

| What breaks | Effect | Mitigation |
|---|---|---|
| herdr misreports an agent's state | A dispatch could settle early or be reported idle | Settlement also requires the worker's `done`; idleness only escalates; `dispatch show` flags mismatches |
| The daemon dies | Messages stop flowing | The CLI respawns it; SQLite keeps everything; statuses are re-synced on subscribe |
| A herdr API change | Orchestration stops | `min_herdr_version`, and a protocol check that refuses to start (with a notification) on an unknown protocol |
| Release with unsaved work | Lost changes | Refuse on uncommitted or unpushed work; `--force` is explicit |
| A stale plan editor | Overwrites a newer change | `--if-version` / `--if-plan-version` refuse stale writes |
| A pane claims to be the owner | A false approval | Local trust only (orch.sock is 0600, same user); the web UI will set the principal from the viewer's Tailscale identity |
