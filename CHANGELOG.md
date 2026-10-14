# Changelog

## Unreleased

- **Cleanup scope safety.** Sweep defaults to no candidates outside recorded released
  linked Git worktrees. Exact Git root and administration-backlink proof is required
  before each signal. Main checkouts, ordinary directories and missing roots are skipped
  with `not a disposable worktree`, reported separately as `skipped_worktrees`.
  Shared cleanup rejects home/system scopes, symlink escapes and unknown/other-user
  ownership. Existing stopped roots no longer qualify; unsafe or non-disposable release
  roots refuse before pane closure even with `--force`. Reports identify matched
  worktrees and literal deleted-suffix directory names retain their boundaries.

- **Process hygiene.** Worker release discovers detached helpers by worktree cwd on
  macOS/Linux, rechecks identity and scope before TERM/KILL, protects orchestration
  processes and reports outcomes. `horch sweep [--dry-run]` handles recorded released
  linked worktrees while excluding reused live scopes; unproven or removed roots are report-only.
- **Resource leases.** `horch lease acquire|release|list`, optional `--ttl`, and repeatable
  dispatch `--lease` reserve named simulator, port and database resources across sessions.
  Successful worker release frees leases; hand-off transfers them with unchanged expiry,
  restart retains them, and conflicts name the holder.
- **Admission control.** `horch run update --max-load N --min-free-mem PCT` adds optional
  macOS/Linux capacity checks before manual or automatic dispatch, with explicit refusals.
- **Message delivery receipts.** Sends and replies wait for an interactive idle agent,
  track submission separately from pickup, recover collapsed pastes, retry once and
  escalate unconfirmed delivery. `send --wait-pickup [--timeout D]` waits for evidence;
  `inbox` shows receipts. Queued jobs survive restarts; uncertain writes are escalated.
- **Mailbox ownership.** A single-consumer lease refuses competing destructive checks
  with the holder's identity. `check --peek` is non-destructive; new `ack --id` acknowledges
  one recipient-owned message. Leases release on timeout, cancellation and disconnect.
- **Completion reports.** Late `done --task` reports append notes to the latest closed
  dispatch without changing its result. Report history is included in dispatch show.
  Worker launch account/profile labels and requested model are persisted, snapshotted
  per dispatch, preserved across restart replacements, and surfaced in completion notifications.
  Messaging migrations retain existing dispatch holds and worker lifecycle records.

- **Lifecycle integration.** Worktree-selected workers retain saved launch arguments for
  restart; PR watching and dispatch holds remain available together.
- **Dispatch lifecycle:** `dispatch hold --until <RFC3339|duration|event:name> --reason ...`
  and `dispatch resume` persist intentional waits. Usage-cap screens raise one escalation
  and auto-hold until reset (explicit resume when unknown). `dispatch fail --infra` refunds
  the interrupted attempt.
- **Worker recovery:** `worker restart --worker W [--env KEY=VAL] [--unset KEY]` verifies clean
  agent exit and shell environment before relaunching with saved arguments and re-dispatching
  the same task without another attempt. `worker release --hand-off-to W` records a transfer
  to a live owner in the same worktree, allowing unsaved helper work without `--force`.
- **Native PR watch.** `horch pr watch <n> [--until green|merged] [--repo owner/name]`
  reports required CI, queue removals and merges to the coordinator inbox, selects latest
  same-head workflow attempts, warns about push/ready races, and supports per-project
  `required_checks` fallbacks when branch policy discovery is unavailable.
- **Worktree shells.** `worker start --worktree` finds or creates an available shell in
  the worktree workspace, skipping widgets and busy panes. Missing pane workspace IDs
  now fail clearly. `--cwd` preserves caller-workspace tab creation.

- **Day-plan store.** `horch plan import|show|item add|update|move|hold|release|remove|transition|events|export|status`.
  The published `<date>.plan.md` is imported into SQLite (migration 3), and every mid-day change
  goes through the store. Writes take `--if-version` (optimistic concurrency) and append to an
  append-only event log that records actor, actor kind, principal and approval. `plan events
  --follow` streams events as they are written (new streaming RPC, `plan.subscribe`). `plan
  export --finalize` writes the plan and its event log back as markdown that re-imports to the
  same plan. Owner edits notify the coordinator.
- **Per-user config.** `~/.config/horch/config.toml` (`$HORCH_CONFIG`), documented in
  `config.example.toml`: owner principal, agents per role, slots, plan/day-log patterns, and
  Linear. Secrets are refused in the file and come only from env or Keychain. `horch config`
  shows what is missing.
- Existing commands and wire formats are unchanged.

## 0.1.2 — 2026-10-02

- **Clearer releases:** one archive per platform with readable names
  (`herdr-orch-v<version>-macos-arm64.tar.gz`, …), each containing both binaries, plus a single
  `checksums.txt`. Release notes say up front that `herdr plugin install` needs no manual download,
  and map each machine to its archive. The installer downloads and verifies the archive.

## 0.1.1 — 2026-10-02

- **No blind re-sends across a daemon restart.** When the connection drops after a request was
  sent, horch re-sends it only if it is safe to repeat (`check`, reads). `ask`, `send`,
  `dispatch`, `done` and the other mutations return `outcome_unknown` with what to check, so a
  restart can no longer duplicate a question, message or dispatch.
- A daemon that is shutting down drops in-flight long polls without an answer, so `check --wait`
  reconnects to the new daemon instead of failing with `context canceled`. Updating the plugin
  mid-run is safe for coordinators waiting in `check --wait`.
- **Docs:** a sample coordinator prompt in the README ("Teach your coordinator"), and
  `docs/coordinator-playbook.md`: plan files, spec templates, worker skills with evidence,
  separate reviewers, model routing, policy, cadence, limits, worktree provisioning, CI
  watchers, lessons, and a coordinator skill skeleton.

## 0.1.0 — 2026-10-02

First public release.

- **Daemon** (`[[startup]]`): one per herdr session, a SQLite store, herdr event subscription,
  singleton with takeover, exits when herdr goes away.
- **`horch` CLI:** runs, mailbox (`send`, `check --wait`, blocking `ask`/`reply`, `inbox`), tasks
  with dependencies, tracked `dispatch` with `nudge`/`fail`, decision gates, workers
  (`start` in a new tab or an existing pane, `register`, `read`, `release`, `stop`, `retain`,
  `abandon`), cron schedules, `--json` everywhere.
- **Settlement:** a dispatch completes on the worker's `horch done` plus herdr agent state.
  Silence escalates to the coordinator, never fails. Blocked prompts escalate after 20s.
- **Release** archives the transcript, closes the pane, verifies every process is gone, and
  refuses unsaved worktrees.
- **Board** pane, **gate** popup, and actions for dispatch-next and installing the CLI.
- Prebuilt binaries for macOS and Linux (amd64, arm64) with SHA-256 verification at install.
