#!/usr/bin/env bash
# Integration test against a real, isolated herdr session (never the default one).
# Runs the end-to-end scenarios:
#   Plan:    import a published day plan, live edits from two identities, events --follow,
#            version conflicts, export → import round trip.
#   Mailbox: dispatch to two workers in separate worktrees; both settle with the
#            coordinator only blocking on `horch check --wait` (no polling loop);
#            survives a daemon restart mid-run; a worker that goes idle without
#            reporting is escalated (never auto-failed), nudged, then failed by the coordinator.
#   DAG:     A → B, A → C with a human gate in front of C, auto-dispatch on.
#   Workers: start 3 real agents (claude, pi, codex), release all 3, zero processes left.
# REAL_AGENTS="claude codex" picks the agents (missing CLIs are skipped); SKIP_REAL_AGENTS=1
# skips the step.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN=${BIN:-$ROOT/bin}
SESSION=${HORCH_IT_SESSION:-horch-it}
SOCK=$HOME/.config/herdr/sessions/$SESSION/herdr.sock
WORK=$(mktemp -d "${TMPDIR:-/tmp}/horch-it.XXXXXX")
STATE=$HOME/.local/state/herdr/plugins/herdr-orch/sessions/$SESSION

unset HERDR_PANE_ID HERDR_TAB_ID HERDR_WORKSPACE_ID HERDR_SESSION HERDR_PLUGIN_STATE_DIR HORCH_AS
export HERDR_SOCKET_PATH=$SOCK
export PATH=$BIN:$PATH
export HORCH_IDLE_REPORT_AFTER=4s
export HORCH_UNOBSERVED_AFTER=60s
# The daemon reads the per-user config; never the real ~/.config/horch here.
export HORCH_CONFIG=$WORK/horch.toml
cat >"$HORCH_CONFIG" <<'TOML'
default_project = "it"
[owner]
principal = "it-owner"
[projects.it]
repo = "example/it"
TOML

pass=0
ok()   { pass=$((pass + 1)); printf '  \033[32m✓\033[0m %s\n' "$*"; }
die()  { printf '  \033[31m✗ %s\033[0m\n' "$*"; exit 1; }
step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
j()    { jq -r "$@"; }

cleanup() {
  set +e
  horch daemon stop >/dev/null 2>&1
  herdr session stop "$SESSION" >/dev/null 2>&1; herdr session delete "$SESSION" >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

step "setup: isolated herdr session '$SESSION'"
horch daemon stop >/dev/null 2>&1 || true
herdr session stop "$SESSION" >/dev/null 2>&1 || true
herdr session delete "$SESSION" >/dev/null 2>&1 || true
rm -rf "$STATE"
env -u HERDR_ENV -u HERDR_SOCKET_PATH herdr --session "$SESSION" server >"$WORK/server.log" 2>&1 &
for _ in $(seq 50); do [ -S "$SOCK" ] && herdr status server >/dev/null 2>&1 && break; sleep 0.2; done
herdr status server >/dev/null || die "herdr test server did not start"
ok "herdr server up"

# A throwaway repo with two linked worktrees.
git -C "$WORK" init -q repo
git -C "$WORK/repo" -c user.email=t@t -c user.name=t commit -q --allow-empty -m init
git -C "$WORK/repo" worktree add -q "$WORK/wt-a" -b a
git -C "$WORK/repo" worktree add -q "$WORK/wt-b" -b b
git -C "$WORK/repo" worktree add -q "$WORK/wt-c" -b c

WS=$(herdr workspace create --cwd "$WORK" --label horch-it | j .result.workspace.workspace_id)
COORD=$(herdr pane list --workspace "$WS" | j '.result.panes[0].pane_id')

fake() { # fake <cwd> <mode> → pane id
  local p
  p=$(herdr pane split "$COORD" --direction right --cwd "$1" --env "PATH=$PATH" --no-focus | j .result.pane.pane_id)
  sleep 0.5
  herdr pane run "$p" "FAKE_MODE=$2 $ROOT/scripts/fake-agent.sh" >/dev/null
  for _ in $(seq 50); do
    [ "$(herdr pane get "$p" | j .result.pane.agent_status)" = idle ] && { echo "$p"; return; }
    sleep 0.2
  done
  die "fake agent in $p never reported idle"
}

# collect_done N: block on `horch check --wait` until N `done` messages arrived.
collect_done() {
  local want=$1 got=0 out
  while [ "$got" -lt "$want" ]; do
    out=$(horch check --wait --timeout 60s --json)
    [ "$(echo "$out" | j 'length')" = 0 ] && die "timed out waiting for done messages ($got/$want)"
    got=$((got + $(echo "$out" | j '[.[] | select(.kind=="done")] | length')))
    echo "$out" | j '.[] | "    · \(.kind): \(.subject)"'
  done
}

step "mailbox + dispatch-and-wait"
A=$(fake "$WORK/wt-a" ok)
B=$(fake "$WORK/wt-b" ok)
ok "fake workers $A ($WORK/wt-a) and $B ($WORK/wt-b) idle"
R1=$(horch run create --title "mailbox" --json | j .id)
horch worker register --pane "$A" --worktree "$WORK/wt-a" --json >/dev/null
horch worker register --pane "$B" --worktree "$WORK/wt-b" --json >/dev/null
T1=$(horch task create --title "write a" --spec "Pretend to write a." --json | j .id)
T2=$(horch task create --title "write b" --spec "Pretend to write b." --json | j .id)
horch dispatch --task "$T1" --to "$A" --json | j '"    dispatched \(.task_id) → \(.pane_id) [\(.status)] \(.outcome)"'
horch dispatch --task "$T2" --to "$B" --json | j '"    dispatched \(.task_id) → \(.pane_id) [\(.status)] \(.outcome)"'
ok "dispatched $T1 → $A, $T2 → $B in run $R1"

OLD=$(horch status --json | j .pid)
horch daemon stop >/dev/null
sleep 0.5
NEW=$(horch status --json | j .pid)
[ "$OLD" != "$NEW" ] || die "daemon was not restarted"
ok "daemon restarted mid-run ($OLD → $NEW) by the CLI"

collect_done 2
for t in "$T1" "$T2"; do
  s=$(horch task show --id "$t" --json | j .task.status)
  [ "$s" = completed ] || die "$t is $s"
done
ok "both dispatches settled completed; coordinator only blocked on check --wait"

# Runs right after the CLI restarted the daemon, so this build (not the plugin-started
# one) serves the plan ops.
step "day plan: import, live edits, follow, export round trip"
FIX=$ROOT/internal/planmd/testdata/example.plan.md
out=$(HORCH_AS="$COORD" horch plan import --day 2026-03-14 --file "$FIX" --ref fixture --json)
[ "$(echo "$out" | j .changed)" = true ] && [ "$(echo "$out" | j '.view.items | length')" = 12 ] || die "import: $out"
[ "$(HORCH_AS="$COORD" horch plan import --day 2026-03-14 --file "$FIX" --json | j .changed)" = false ] || die "re-import changed the plan"
ok "imported the example plan (12 items); re-import is a no-op"
horch plan events --follow --json >"$WORK/follow.ndjson" &
FOLLOW=$!
sleep 0.5
V=$(horch plan show --json | j '.items[] | select(.id=="B1") | .version')
HORCH_AS="$COORD" horch plan transition --item B1 --state settled --pr '#1' --if-version "$V" --json >/dev/null
out=$(horch plan item update --item B1 --title stale --if-version "$V" --json 2>/dev/null || true)
[ "$(echo "$out" | j .error.code)" = version_conflict ] || die "stale write not refused: $out"
ok "orchestrator transition applied; a stale --if-version write is refused"
horch plan item hold --item I1 --reason "sim farm down" --json >/dev/null
horch plan item add --item B2 --issues ACME-1 --title "slice b" --position 2 --json >/dev/null
horch plan item move --item W1 --to 1 --json >/dev/null
last=$(horch plan events --json | j '.[-1]')
[ "$(echo "$last" | j .principal)" = it-owner ] && [ "$(echo "$last" | j .approval)" = true ] || die "owner edit not an approval: $last"
[ "$(horch inbox --as "$COORD" --json | j '[.[] | select(.subject | startswith("plan replanned by it-owner"))] | length')" = 3 ] || die "coordinator not told about owner edits"
ok "owner edits recorded as approvals; coordinator told 3 times"
for _ in $(seq 50); do [ "$(wc -l <"$WORK/follow.ndjson")" -ge 5 ] && break; sleep 0.1; done
kill "$FOLLOW" 2>/dev/null; wait "$FOLLOW" 2>/dev/null || true
[ "$(j -s 'map(.op) | join(",")' "$WORK/follow.ndjson")" = "import,transition,item.hold,item.add,item.move" ] || die "follow got: $(cat "$WORK/follow.ndjson")"
ok "events --follow received the backlog and every live event"
horch plan export --out "$WORK/export.md" --json >/dev/null
grep -q '^## Event log' "$WORK/export.md" || die "export lacks the event log"
horch plan import --day 2026-10-03 --file "$WORK/export.md" --json >/dev/null
a=$(horch plan show --day 2026-03-14 --json | j '[.items[] | [.id,.state,.held,.title,.pr]]')
b=$(horch plan show --day 2026-10-03 --json | j '[.items[] | [.id,.state,.held,.title,.pr]]')
[ "$a" = "$b" ] || die "export → import changed the plan"
ok "export → import round trip preserves every item"

S=$(fake "$WORK/wt-c" silent)
horch worker register --pane "$S" --json >/dev/null
T3=$(horch task create --title "stay quiet" --json | j .id)
horch dispatch --task "$T3" --to "$S" --json >/dev/null
out=$(horch check --wait --timeout 60s --json)
echo "$out" | j '.[] | "    · \(.kind): \(.subject)"'
[ "$(echo "$out" | j '.[0].kind')" = escalation ] || die "no escalation for idle worker"
[ "$(horch dispatch show --task "$T3" --json | j '.[0].status')" = dispatched ] || die "idle worker's dispatch must stay open"
ok "worker idle without reporting → coordinator told (screen tail), dispatch stays open"
horch dispatch nudge --task "$T3" --json >/dev/null
st=$(horch dispatch fail --task "$T3" --reason "nudged, still silent" --json)
[ "$(echo "$st" | j .dispatch.status)" = failed ] && [ "$(echo "$st" | j .task.status)" = ready ] || die "dispatch fail: $st"
horch check --json >/dev/null
ok "coordinator nudged, then failed it explicitly → task ready for retry"

step "tasks, dependencies, gates"
R2=$(horch run create --title "dag" --auto-dispatch --json | j .id)
for p in "$A" "$B"; do horch worker register --pane "$p" --run "$R2" --json >/dev/null; done
TA=$(horch task create --title A --json | j .id)
TB=$(horch task create --title B --deps "$TA" --json | j .id)
TC=$(horch task create --title C --deps "$TA" --json | j .id)
G=$(horch gate create --task "$TC" --question "Ship C?" --options yes,no --json | j .id)
ok "A=$TA  B=$TB←A  C=$TC←A  gate $G on C"
collect_done 2
[ "$(horch task show --id "$TC" --json | j .task.status)" = blocked ] || die "C should be blocked on the gate"
ok "A then B completed via auto-dispatch; C blocked on $G"
horch gate resolve --id "$G" --decision yes --json >/dev/null
collect_done 1
for t in "$TA" "$TB" "$TC"; do
  [ "$(horch task show --id "$t" --json | j .task.status)" = completed ] || die "$t not completed"
done
ok "gate resolved → C auto-dispatched and completed"

step "worker lifecycle"
PIDS=()
for p in "$A" "$B" "$S"; do
  res=$(horch worker release --worker "$p" --force --json)
  [ "$(echo "$res" | j .worker.state)" = released ] || die "release $p: $res"
  PIDS+=($(echo "$res" | j '.pids[]'))
done
ok "released 3 fake workers; transcripts archived"

if [ "${SKIP_REAL_AGENTS:-}" != 1 ]; then
  R3=$(horch run create --title "workers" --json | j .id)
  W=()
  for a in ${REAL_AGENTS:-claude pi codex}; do
    command -v "$a" >/dev/null || { echo "    · $a not installed, skipping"; continue; }
    out=$(HORCH_AS="$COORD" horch worker start --agent "$a" --worktree "$WORK/wt-a" --name "it-$a" --json) || die "start $a: $out"
    W+=("$(echo "$out" | j .pane_id)")
  done
  [ ${#W[@]} -gt 0 ] || die "no real agent CLI installed (REAL_AGENTS=${REAL_AGENTS:-claude pi codex}); set SKIP_REAL_AGENTS=1"
  ok "started real workers ${W[*]}"
  sleep 2
  for p in "${W[@]}"; do
    pids=$(herdr pane process-info --pane "$p" | j '[.result.process_info.shell_pid] + [.result.process_info.foreground_processes[].pid] | .[]')
    PIDS+=($pids)
    res=$(horch worker release --worker "$p" --json)
    [ "$(echo "$res" | j .worker.state)" = released ] || die "release $p: $res"
  done
  ok "released all 3 real workers"
fi

alive=()
for pid in "${PIDS[@]}"; do kill -0 "$pid" 2>/dev/null && alive+=("$pid $(ps -o comm= -p "$pid")"); done
[ ${#alive[@]} -eq 0 ] || die "processes still alive: ${alive[*]}"
ok "process sweep: 0 of ${#PIDS[@]} recorded processes alive"

printf '\n\033[32m%d checks passed\033[0m\n' "$pass"
