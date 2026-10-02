#!/usr/bin/env bash
# Live check with real agents: claude and codex workers in separate git
# worktrees each get a dispatched task, do it, report with `horch done`, and settle while
# the coordinator only blocks on `horch check --wait`. Then the release guards are checked
# against the real (dirty) worktrees and every worker is released.
#
# Uses an isolated herdr session and spends one short turn per agent. The repo is a temp
# dir this script creates, so the script answers the agents' folder-trust prompts for it.
#   AGENTS="claude codex" (default; pi works too if its model backend is up)   KEEP=1
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
SESSION=${HORCH_IT_SESSION:-horch-live}
SOCK=$HOME/.config/herdr/sessions/$SESSION/herdr.sock
WORK=$(mktemp -d "${TMPDIR:-/tmp}/horch-live.XXXXXX")
AGENTS=${AGENTS:-claude codex}
unset HERDR_PANE_ID HERDR_TAB_ID HERDR_WORKSPACE_ID HERDR_SESSION HERDR_PLUGIN_STATE_DIR HORCH_AS
BIN=${BIN:-$ROOT/bin}
export HERDR_SOCKET_PATH=$SOCK PATH=$BIN:$PATH
j() { jq -r "$@"; }
die() { printf '\033[31m✗ %s\033[0m\n' "$*"; exit 1; }
cleanup() {
  [ "${KEEP:-}" = 1 ] && { echo "kept session $SESSION, work dir $WORK"; return; }
  set +e; horch daemon stop >/dev/null 2>&1; herdr session stop "$SESSION" >/dev/null 2>&1; herdr session delete "$SESSION" >/dev/null 2>&1; rm -rf "$WORK"
}
trap cleanup EXIT
horch daemon stop >/dev/null 2>&1 || true
herdr session stop "$SESSION" >/dev/null 2>&1 || true
herdr session delete "$SESSION" >/dev/null 2>&1 || true
rm -rf "$HOME/.local/state/herdr/plugins/herdr-orch/sessions/$SESSION"
env -u HERDR_ENV -u HERDR_SOCKET_PATH herdr --session "$SESSION" server >"$WORK/server.log" 2>&1 &
for _ in $(seq 50); do herdr status server >/dev/null 2>&1 && break; sleep 0.2; done

git -C "$WORK" init -q repo
git -C "$WORK/repo" -c user.email=t@t -c user.name=t commit -q --allow-empty -m init
WS=$(herdr workspace create --cwd "$WORK" --label live | j .result.workspace.workspace_id)
COORD=$(herdr pane list --workspace "$WS" | j '.result.panes[0].pane_id')
export HORCH_AS=$COORD
horch run create --title live --json | j '"run \(.id), coordinator \(.coordinator_pane_id)"'

for a in $AGENTS; do
  git -C "$WORK/repo" worktree add -q "$WORK/wt-$a" -b "live-$a"
  horch worker start --agent "$a" --worktree "$WORK/wt-$a" --name "live-$a" --json | j '"worker \(.pane_id) \(.agent) [\(.state)]"'
  # Answer a folder-trust dialog for the temp repo if one shows up; nothing else.
  for _ in $(seq 20); do
    screen=$(herdr agent read "live-$a" --source visible 2>/dev/null || true)
    if grep -q "Yes, I trust this folder" <<<"$screen"; then
      herdr agent send-keys "live-$a" down enter >/dev/null; echo "  answered claude's folder-trust prompt"
    elif grep -q "Trust and continue" <<<"$screen"; then
      herdr agent send-keys "live-$a" enter >/dev/null; echo "  answered codex's folder-trust prompt"
    elif [ "$(herdr agent get "live-$a" | j .result.agent.agent_status)" = idle ]; then
      break
    fi
    sleep 1.5
  done
done
sleep 2
horch check --json >/dev/null  # drop the startup escalations

n=0
for a in $AGENTS; do
  t=$(horch task create --title "note from $a" --spec "Create a file NOTE.txt in the current directory containing exactly: hello from $a. Do not commit it." --json | j .id)
  herdr agent wait "live-$a" --until idle --until done --timeout 60000 >/dev/null || die "$a never went idle"
  out=$(horch dispatch --task "$t" --to "live-$a" --json) || die "dispatch to $a: $out"
  printf '%s\n' "$out" | j '"dispatched \(.task_id) → \(.pane_id) via \(.outcome)"'
  n=$((n + 1))
done

start=$(date +%s); got=0
while [ $got -lt $n ]; do
  out=$(horch check --wait --timeout 300s --json)
  [ "$(printf '%s\n' "$out" | j length)" = 0 ] && { horch run show; die "timed out"; }
  printf '%s\n' "$out" | j '.[] | "[\(.kind)] \(.subject)\n    \(.body | gsub("\n"; " ") | .[0:240])"'
  got=$((got + $(printf '%s\n' "$out" | j '[.[]|select(.kind=="done")]|length')))
done
echo "all $n settled in $(( $(date +%s) - start ))s; coordinator only blocked on check --wait"
for a in $AGENTS; do
  [ "$(cat "$WORK/wt-$a/NOTE.txt")" = "hello from $a" ] || die "$a NOTE.txt wrong"
done
echo "NOTE.txt correct in every worktree"

a=${AGENTS%% *}
if horch worker release --worker "live-$a" 2>/dev/null; then die "release of dirty worktree was not refused"; fi
echo "release refused for the dirty worktree of live-$a (unsaved_work)"
for a in $AGENTS; do horch worker release --worker "live-$a" --force; done
