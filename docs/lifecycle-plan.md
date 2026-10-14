# Dispatch lifecycle implementation plan

- Persist dispatch holds with a deadline or named event; expose hold/resume and reset the
  idle grace on release. Holds suppress idle, blocked and unobserved reminders while normal
  completion remains possible. Named events are released explicitly with dispatch resume.
- Recognize usage-cap notices from inactive panes, persist a deduplication key, and hold
  until a parsed reset. Unknown reset times require explicit resume rather than guessing.
- Reuse infrastructure fencing for dispatch fail --infra. Restart workers only after agent
  exit and shell environment verification; preserve launch arguments and task ownership.
- Allow release hand-off only to a verified live worker in the same worktree, and record
  the responsibility transfer before closing the helper.
- Add migration, store/engine/CLI tests, update the guide and public documentation, run
  vet/tests/race and isolated builds, then open one draft PR and wait for CI before ready.

All fixtures are synthetic. Validation never uses an installed daemon or agent pane.
