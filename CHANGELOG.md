# Changelog

## Unreleased

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
