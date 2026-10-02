# herdr API notes (herdr 0.9.1–0.9.3, protocol 22)

What the herdr socket API and plugin runtime actually do, measured on 2026-10-01 while building
herdr-orch. Useful if you are writing a herdr plugin of your own.

All experiments ran in an isolated session (`herdr --session orchtest server`), never the
default one.

## Wire protocol

- Unix socket at `$HERDR_SOCKET_PATH`, newline-delimited JSON.
- Request `{"id","method","params"}` → `{"id","result"}` or `{"id","error":{"code","message"}}`.
- **One request per connection.** A second request on the same connection gets a broken pipe.
- `events.subscribe` holds its connection open: first line is
  `{"id":"sub","result":{"type":"subscription_started"}}`, then one event per line.
- The subscription set is fixed for the life of the connection. Changing it = reconnect.

## events.subscribe

Global (no filter): `workspace.*`, `worktree.created|opened|removed`, `tab.*`,
`pane.created|closed|updated|focused|moved|exited|agent_detected`, `layout.updated`.

Per pane (`pane_id` **required**): `pane.agent_status_changed` (optional `agent_status`
filter), `pane.output_matched` (needs `source` + `match`), `pane.scroll_changed`.

So the daemon cannot watch "all agents" with one filter: it keeps one subscription whose
set includes every watched pane and reconnects when that set changes. `pane.updated` (global)
also carries `pane.agent_status`, but it can lag the per-pane stream (seen flipping a pane
back to `idle` 100ms after `working`), so herdr-orch does not use it for status. Original note:
also carries `pane.agent_status`, but it fires for many unrelated reasons.

Event names are inconsistent on the wire — match both spellings:

```json
{"event":"pane_agent_detected","data":{"agent":"fake","pane_id":"w1:p1","type":"pane_agent_detected","workspace_id":"w1"}}
{"event":"pane.agent_status_changed","data":{"agent":"fake","agent_status":"working","pane_id":"w1:p1","workspace_id":"w1"}}
{"event":"pane_created","data":{"type":"pane_created","pane":{"pane_id":"w1:p2","agent_status":"unknown","cwd":"/private/tmp","tab_id":"w1:t1","workspace_id":"w1", "...":"..."}}}
{"event":"pane_closed","data":{"pane_id":"w1:p1","type":"pane_closed","workspace_id":"w1"}}
{"event":"pane_exited","data":{"pane_id":"w1:p2","type":"pane_exited","workspace_id":"w1"}}
{"event":"pane_updated","data":{"pane":{"agent":"pi","agent_status":"idle","agent_session":{"...":"..."}, "...":"..."}}}
```

- `pane_exited` has **no exit code**. `blocked` status changes do not carry the message passed
  to `report-agent`.

### Real agents (claude, pi, codex)

| Agent | After `agent start` | Prompt | Transitions seen |
|---|---|---|---|
| pi | idle | ok | idle → working → idle |
| codex | idle | ok | idle → working → idle |
| claude | `agent_not_ready` (blocked on folder-trust dialog) | first prompt after the dialog was **swallowed** (`agent_prompt_stalled`); retry ok | blocked → idle; working → idle |

- No agent emitted `done` over the API; a finished turn shows as `working → idle`.
  Settlement therefore keys on `working → idle|done` **plus** the worker's `done` message.
- `agent_prompt_stalled` does not prove the prompt was lost or delivered. Dispatch records
  it and does not blindly resend.

## agent.prompt vs fake agents

`agent.prompt` / `agent.wait` only accept agents herdr started or detected itself.
Agents created with `pane report-agent` (even after `agent rename`) are refused with
`agent_not_ready: ... is not an active named agent`. Dispatch falls back to
`pane.send_text` + `pane.send_keys enter` in that case; the integration test's fake agent
relies on that path.

## Plugin lifecycle

| Trigger | Effect on `[[startup]]` process |
|---|---|
| `plugin link` | not started |
| `server reload-config` | not started, not restarted (existing one keeps running) |
| server start | started |
| `server.live_handoff` | **new server starts a second copy; the old one keeps running** |
| `session stop` | **not killed** |

Consequences for the daemon:
- **Singleton lock with takeover**: a new daemon asks the old one to exit over `orch.sock`,
  then takes the lock (`flock`). The newest instance always wins, since it is the one
  attached to the live server.
- **Self-exit** when the herdr socket stays unreachable (server stopped).
- The CLI respawns the daemon if `orch.sock` is dead.

Startup environment:

```
HERDR_BIN_PATH=~/.local/bin/herdr
HERDR_PLUGIN_CONFIG_DIR=~/.config/herdr/plugins/config/<id>
HERDR_PLUGIN_CONTEXT_JSON={"invocation_source":"startup","correlation_id":"plugin.startup"}
HERDR_PLUGIN_EVENT=startup
HERDR_PLUGIN_ID=<id>
HERDR_PLUGIN_ROOT=<plugin dir>          (also the cwd)
HERDR_PLUGIN_STATE_DIR=~/.local/state/herdr/plugins/<id>
HERDR_SESSION=orchtest
HERDR_SOCKET_PATH=~/.config/herdr/sessions/orchtest/herdr.sock
```

- **`HERDR_PLUGIN_STATE_DIR` is not per session**, and the plugin registry
  (`~/.config/herdr/plugins.json`) is global: a plugin linked once runs in every session.
  State therefore lives in `$HERDR_PLUGIN_STATE_DIR/sessions/<HERDR_SESSION>/` (`orch.db`,
  `orch.sock`, `orch.lock`).
- Agent panes do not get `HERDR_SESSION`, only `HERDR_SOCKET_PATH`. `horch` derives the
  session from the socket path (`.../sessions/<name>/herdr.sock` → `<name>`, else `default`).

## Process cleanup

`pane.process_info` returns `shell_pid` and `foreground_processes[].pid`. After
`pane.close` on claude, pi and codex panes, all 9 recorded pids were gone within 2s.
Release verifies this the same way: record pids, close, poll `kill(pid, 0)`.

## Plugin questions answered

1. **Plugin CLI subcommands?** No. The manifest supports `[[startup]]`, `[[panes]]`,
   `[[actions]]`, `[[events]]`, `[[build]]` only, and `herdr plugin` has no command hook.
   `horch` stays a standalone binary (symlinked onto `PATH` by `make install`).
2. **Does startup survive `live_handoff`?** The old process survives *and* a new one is
   started. Handled by the takeover lock above.
