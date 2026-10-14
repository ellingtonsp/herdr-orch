# PR CI watch and worktree shell selection

Implement a foreground `horch pr watch` command using authenticated `gh` reads and the
existing coordinator inbox. Select the current PR head and newest workflow attempts,
combine classic branch protection with effective ruleset requirements, and require a
repository-specific fallback when requirement discovery is unavailable. Track merge,
queue removal and push/ready timing independently of CI. Use synthetic API recordings
and isolated daemon tests; never mutate GitHub from the watcher.

Fix worktree worker startup to resolve the owning workspace and use only a verified
shell pane, creating one when necessary. Reject missing workspace identity explicitly.
Keep the existing worker flags and non-worktree paths compatible.

Validation: focused regression tests first, then Go vet, full tests with the race detector,
isolated builds, public-content diff review, and the PR's CI including private-terms.
Update the README, coordinator guide, API notes, config example and changelog. Open one
draft PR and mark ready only after its checks pass. Do not merge.
