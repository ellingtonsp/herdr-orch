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
