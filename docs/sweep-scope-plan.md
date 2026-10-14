# Sweep scope safety fix

- Reproduce unsafe broad recorded roots with synthetic system-agent, home-shell and unrelated-server inventories.
- Default deny cleanup unless an explicitly recorded released or missing worker worktree contains the process cwd; exclude stopped-but-existing roots, protected directories and other users.
- Apply the same validated scope to release, hand-off, rescans and each signal; preserve literal directory names and resolve symlink escapes conservatively.
- Verify isolated regression tests, full vet/race and platform CI; never run live cleanup or replace installed components.
