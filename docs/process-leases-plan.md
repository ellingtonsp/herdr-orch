# Process cleanup and resource admission plan

1. Add macOS/Linux process inventory with cwd containment and identity checks. Release
   discovers detached helpers in a registered worktree, reports termination outcomes,
   uses SIGTERM then SIGKILL, and excludes the orchestrator and processes outside it.
   Add an explicit sweep with dry-run for registered released or missing worktrees.
2. Persist named simulator, port and database leases with optional expiry. Serialize
   competing acquisitions, name the holder in refusals, reserve dispatch requirements
   before delivery, and release reservations when a worker is successfully released.
3. Add optional per-run load and available-memory limits, checked before dispatch on
   macOS/Linux. Existing runs remain unrestricted; refusals explain the sampled limit.
4. Cover safety boundaries, cleanup, expiry/contention, dispatch refusals and CLI parsing
   with tests. Update the README, design notes, embedded guide and changelog.
5. Run Go tests, race tests, vet and isolated builds; inspect the public-content diff.
   Push the task branch, open one draft PR, and mark ready only after CI passes. Never
   merge or replace the installed binaries or daemon.
