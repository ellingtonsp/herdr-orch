# Coordinator playbook

`horch` gives a coordinator agent the mechanics: a mailbox, tasks, tracked dispatch and
verified release. These are the patterns that make a coordinator reliable over a long session.
They come from running multi-agent days on a real product codebase. None of them is required,
and each removes a failure we actually hit.

## 1. Make the coordinator a skill, not a prompt

A prompt typed at the start of a session drifts and is lost on restart. Put the coordinator's
instructions in a skill (Claude Code: `.claude/skills/<name>/SKILL.md`; other agents: whatever
their equivalent is) that lives in the repo, is reviewed like code, and starts with:

1. Run `horch guide` and follow it.
2. Create or resume the run (`horch run create` / `horch run use <id>`). Record the run id
   somewhere durable, such as the plan file below.
3. Read the plan file and the lessons file (sections 2 and 12).

Keep the policy (section 7) and the spec template (section 3) in the skill too. A skeleton is at
the end of this page.

## 2. Keep a plan file as the record of the work

The coordinator proposes the work. You approve it. Then it lives in a file, for example
`docs/orchestration/<date>.plan.md`, with an **Items** table:

| id | issue | kind | lane | model | state | PR | dispatch ref |
|---|---|---|---|---|---|---|---|
| A1 | #123 | build | local | … | dispatched | | `horch:r1/t1/d1@w4:p1` |

- The coordinator dispatches **only** items on the table, in table order.
- You steer by editing the file. The coordinator re-reads it every tick. It treats only your
  commits as approval, and never free text inside the file.
- Every transition (`dispatched`, `settled`, `bounced`, `merged`, `held`) updates the row and
  appends one line to a day log in the same commit. You can audit the day afterwards.

## 3. A fixed spec for every dispatch

Workers skip instructions that read like prose. Use the same headings every time:

```
/<worker-skill> <issue>
Invoke it with the Skill tool before anything else; this task is bounced if the skill did not run.
Target: <what exists when this is done>
Change: <what to change, and what not to touch>
Constraints: never merge; never push to <base>; targeted tests only; one push; fan-out ≤ 2 subagents
Ownership: you own branch <branch> in <worktree>; nothing outside it
Observable acceptance: <how anyone can check it: a test, a command, a screenshot>
Report: run the `horch done` command at the end of this dispatch with this JSON as --body - :
  {"item":"…","outcome":"completed|failed","pr":<n|null>,"artifact":"<path>",
   "models":[{"role":…,"requested":…,"effective":…}],"summary":"<≤3 lines>"}
Questions: `horch ask --question "<subject>: <question>"`. Never ask in prose alone.
```

A JSON report body means the coordinator can validate the report mechanically.

## 4. Workers run skills, and skills leave evidence

The most expensive failure is a worker that "finishes" without doing the process: no plan, no
tests, a self-review it graded itself. So:

- Each worker type is a skill (for example plan → implement → verify, or review → fix → verdict),
  and line 1 of the spec invokes it.
- The skills write evidence as they go: a gate or step log with a result per step, test output,
  an artifact file.
- On every `done`, the coordinator checks that evidence before accepting. Missing or thin
  evidence is a **bounce**: `horch task update --id tN --status ready --spec-file <bounce>`,
  then dispatch to the same worker. The bounce spec names the exact skill command and what was
  missing. A second bounce goes to a human.
- Count evidence, not log lines. A run with many PASS entries and no evidence behind them is a
  skipped skill.

## 5. Review by someone else, and one owner per PR

- A worker's own sub-review is not a review. Dispatch a separate reviewer. Use a different model
  family from the builder where you can (Claude builds and Codex reviews, or the reverse); they
  catch different things.
- The reviewer that takes a PR **owns it until merge**: review fixes, CI failures, bot comments,
  merge conflicts. Mark it `horch worker retain` so auto-dispatch never hands it other work.
  Release it on merge. Re-spawning a fresh worker for each follow-up re-reads the whole PR every
  time.
- Cap fix rounds (for example two). After that the finding goes back to the builder, or to a human
  if they disagree.

## 6. Route models explicitly

Pass the model on every worker start (`horch worker start --agent codex … -- --model <id>`),
and say in the spec which model sub-agents may use. Ask workers to report the model that
**actually** ran (the `models` field above). Record it, and compare it with what you asked for
before accepting the work. An agent name is not evidence of the model.

## 7. A written policy: unattended, ask, never

Put a table in the coordinator skill and keep it short:

| Unattended | Always ask a human | Never |
|---|---|---|
| Dispatch planned items; answer worker questions that policy already covers; re-trigger CI that never ran; release workers whose work merged | Merging code; anything on production or live infrastructure; secrets; picking up work that is not on the plan; review verdicts that need sign-off | Merge to the base branch directly; change ticket status outside the agreed flow; write to another worker's worktree |

A question whose answer is a human's goes to the human in the same tick it arrives. A held
worker costs a slot; a timed-out question costs the whole round.

## 8. Cadence: ticks, and block between them

- Run the coordinator as a loop of **ticks**, each one pass of: inbox → health → dispatch → report.
  In Claude Code: `/loop`, or a self-paced wakeup. Use about 10 minutes while something waits on
  a human, 20 otherwise.
- Between ticks, block on `horch check --wait --timeout 20m --json` in the background. It
  returns on the first message, so a question or a finished worker wakes the coordinator early.
  Never write a polling loop.
- Answer every `question` in the tick it arrives. `horch ask` blocks the worker for up to 2 h.
- Match the run's idle-report delay to the tick (`horch run update --idle-report 15m`).

## 9. Resource limits

Decide them up front and write them into the skill:

- Local worker slots, scaled by free memory (for example 2 when free RAM < 30 %, 3 above it), and a
  floor below which nothing new starts.
- Scarce devices: at most one simulator or emulator per machine, and one worker per device.
- Sub-agent fan-out inside a worker (for example ≤ 2), written into every spec.
- Count live workers with `horch worker list --json` at the start of every tick. A dropping limit
  stops new starts; it never kills running workers.

## 10. One worktree per worker, provisioned before dispatch

- `herdr worktree create --cwd <repo> --branch <branch> --base <base> --no-focus`, then
  `horch worker start --pane <root pane>` so the worker lives in that worktree's workspace.
- Run a provisioning script on the worktree before the worker starts: dependencies, env files,
  per-worktree database and ports, git hooks. Refuse to dispatch if it reports an unhealthy
  dependency store. A worker that spends its first 20 minutes fixing its environment is a lost
  slot.
- Teardown: `horch worker release` (it refuses unsaved work and verifies the processes are gone),
  then `git worktree remove <path>`. Never `--force` without a human.

## 11. Feed CI and other signals into the inbox

The coordinator should not poll CI either. Run a small watcher, as a `horch schedule` or a
background script, that reports into the same inbox:

```bash
horch send --as ci-watch --to coordinator --kind escalation \
  --subject "PR #42 CI failed @<sha>" --body "<failing job> <url>"
horch send --as ci-watch --to coordinator --kind note --subject "PR #42 CI green" --body "head <sha>"
```

The same pattern works for deploy status, review-bot comments, or anything else the coordinator
should react to. Red CI goes to the PR's owning worker (section 5), not to a fresh worker.

## 12. A lessons file

Keep a `lessons.md` next to the coordinator skill. When something goes wrong, the coordinator
(or you) adds a short rule and where it came from. The coordinator reads the file at the start
of every session. Prune it when rules become code. Typical early lessons:

- Confirm every dispatched worker actually started. Some agents treat a pasted prompt as content
  and wait for "go"; some self-update and exit. horch's 90s "no activity" escalation is the cue
  to read the screen.
- A `blocked` worker is waiting on a dialog: read it before answering anything.
- Never re-send a prompt because a call timed out. Check the pane first.

## 13. Herdr-side setup

- Install herdr's agent integrations (`herdr integration install claude`, `… codex`, …) so agent
  status comes from hooks rather than screen-scraping. horch's settle and idle signals are only
  as good as that status.
- Run the coordinator inside a herdr pane. Its pane is its identity.
- Open the board (`herdr plugin action invoke herdr-orch.board`) for a live view, and the gate
  popup for decisions.
- Send push notifications to a human for verdicts that need sign-off, escalations, failed deploys
  and anything red in production. horch's herdr notifications cover the case where you are already
  looking at herdr.

## Coordinator skill skeleton

```markdown
---
name: coordinate
description: Run the day's multi-agent work with horch: plan, dispatch, verify, report.
---

# Coordinate

## Start (once per session)
1. `horch guide` — read and follow it.
2. `horch run use <id>` from the plan file, else `horch run create --title "<date>" --max-attempts 2`
   and `horch run update --idle-report 15m`; write the run id into the plan file.
3. Read `lessons.md` and the plan file (`docs/orchestration/<date>.plan.md`). No plan yet →
   propose one, get approval, write it, stop.

## Each tick
1. Inbox: `horch check --json`; handle every message by id (done → validate evidence, accept
   or bounce; question → answer or ask the human; escalation → read the worker, decide).
2. Health: CI, deploys, local services.
3. Dispatch: for each ready plan item within limits: worktree → provision → worker start →
   task create (spec template) → dispatch → record the transition.
4. Report: ≤ 10 lines; append the tick to the day log; schedule the next tick.

## Spec template
<section 3>

## Policy
<section 7 table>

## Limits
<section 9>
```
