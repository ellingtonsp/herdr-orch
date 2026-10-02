#!/bin/sh
# Fake agent for integration tests. It reports its state to herdr like an agent
# integration would, reads dispatched prompts typed into the pane (horch falls back to
# pane.send_text for agents herdr did not start), and reports back through horch.
#   FAKE_MODE=ok (default) | fail | silent      FAKE_WORK_SECS=2
P="$HERDR_PANE_ID"
rep() { herdr pane report-agent "$P" --source horch-fake --agent fake --state "$1" >/dev/null; }
rep idle
echo "fake agent ready in $P (mode ${FAKE_MODE:-ok})"
while IFS= read -r line; do
  task=$(printf '%s\n' "$line" | sed -n 's/.*task \(t[0-9][0-9]*\):.*/\1/p' | head -1)
  [ -z "$task" ] && continue
  echo "got $task"
  rep working
  sleep "${FAKE_WORK_SECS:-2}"
  case "${FAKE_MODE:-ok}" in
    ok) horch done --task "$task" --body "fake agent $P did $task" ;;
    fail) horch done --task "$task" --failed --body "fake agent $P could not do $task" ;;
    silent) echo "staying silent" ;;
  esac
  rep idle
done
