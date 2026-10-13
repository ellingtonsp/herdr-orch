# Message delivery and acknowledgement

`horch send` stores the message before attempting terminal delivery. Pane recipients get a
background delivery job, serialized per pane. A busy worker can finish its current turn;
startup hooks, unknown status and blocked dialogs are not input-ready states. The job waits
up to two minutes per attempt for two consecutive idle/done readiness observations, 500 ms
apart. Where herdr exposes `interactive_ready`, it must not be false.

```sh
horch send --to worker-a --body "Please inspect the new test failure." --wait-pickup
horch send --to worker-a --body - --wait-pickup --timeout 5m < message.txt
horch inbox --all --json
```

Delivery and pickup are separate evidence:

| Receipt | Meaning |
|---|---|
| `queued` | Stored durably; waiting for the recipient to read it or become ready. |
| `submitting` | A terminal submission attempt began; its outcome is not yet confirmed. |
| `delivered` | The terminal API accepted the submission; this alone does not prove a new agent turn. |
| `picked_up` | Recipient read/acknowledgement (`pickup_via=read`) or observed working state after submission (`pickup_via=working`). |
| `undelivered` | Pickup could not be confirmed after the retry, or a restart left the previous write uncertain. |

`inbox` shows the receipt status. JSON also includes `delivery_attempts`, `delivered_at`,
`picked_up_at`, `pickup_via`, `delivery_error` and the independent mailbox `read_at`.
A working-state pickup is evidence of activity, not proof the agent understood the message;
it leaves the mailbox unread. A later `check` or `ack` still consumes it.

After submission, pickup is observed for ten seconds. The job makes at most two terminal
submission attempts. On retry, Claude/Codex collapsed-paste indicators or the current
message marker cause Enter to be sent to the existing composer, without pasting a duplicate.
Otherwise the same message ID and body are submitted once more after readiness. If pickup
remains unconfirmed, the coordinator receives an `undelivered` escalation. Inspect the pane
before manually resending: unconfirmed delivery can still have reached the agent.

The normal transport is herdr's `agent.prompt`, which preserves multiline content and handles
live bracketed-paste mode. Legacy report-agent integrations retain single-line text input,
with a delay between text and Enter. A restart resumes jobs with no submission attempt;
it escalates uncertain in-flight submissions rather than replaying them.

`--wait-pickup` waits for all recipients and exits nonzero on `undelivered` or
`pickup_timeout`. Its default wait is 4m25s; `--timeout` changes the caller's wait, not the
background job. Disconnecting or timing out does not discard the message. Check `inbox`
before sending it again. Human recipients remain mailbox-only and require a read/ack.

Replies use the same receipts. A reply consumed by a still-running `horch ask` is marked
read and does not need terminal input. If the original ask has ended, the reply remains in
the mailbox and is delivered when the agent becomes ready.

## One consumer per mailbox

A destructive `check --wait` holds a daemon lease for its recipient mailbox until the RPC
returns, times out or disconnects. A second destructive check or ack is refused with
`mailbox_busy`, naming the holder's command and hostname/PID. A daemon restart releases
these connection-scoped leases; reconnecting clients must acquire them again.

```sh
horch check --wait --timeout 30m       # one consuming coordinator loop
horch check --peek                    # inspect without consuming
horch check --peek --wait             # wait without claiming the mailbox
horch ack --id m12                    # acknowledge a specific message addressed to you
```

Ack is idempotent and recipient-scoped. `check --peek` and `inbox` never mark messages read.
Plain `check` retains its existing consuming behavior, but cannot steal messages while a
waiter holds the lease. This lease prevents concurrent consumers; it does not promise
exactly-once delivery if a client disconnects after the database read but before receiving
its response. For explicit processing acknowledgement, use peek followed by ack.

## Late completion reports and launch metadata

`horch done --task t12 --body -` accepts a report after that task's latest dispatch has
closed. It appends a late note associated with that dispatch and notifies the coordinator.
The existing task result, attempt count and dispatch settlement do not change, including
when the late report uses `--failed`. Only that dispatch's assignee can report. Without
`--task`, done still resolves the caller's active dispatch; specify the task for a late note.
`horch dispatch show --task t12 --json` includes the complete associated `reports` history.

```sh
horch worker start --agent codex --name worker-a --worktree ../myapp-a \
  --account account-a --profile profile-a --model model-id
```

`--account` and `--profile` are recorded labels, not authentication or account-switching
commands. Put agent-specific selection flags after `--`. Account, profile and model values
are also captured from `--account`, `--profile`, and `--model`/`-m` launch arguments.
`--model` before the separator adds the agent's `--model` option unless one is already
present; a model supplied in the agent arguments takes precedence. Values are supplied
launch metadata, not verified account identity or proof of the effective runtime model.
They are saved on the worker and snapshotted on each dispatch, then included in completion
and late-report notifications. Do not put credentials in these labels.
