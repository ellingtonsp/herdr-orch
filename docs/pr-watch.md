# Watching pull requests

```sh
horch pr watch 12 --repo you/myapp
horch pr watch 12 --repo you/myapp --until merged --run r1
```

`watch` is a foreground process; keep it in a shell or supervise it as a background job.
It uses authenticated `gh` reads and the existing daemon's coordinator inbox. It does not
start a daemon, change PR status, rerun checks, or merge. Omit `--repo` to resolve the
current repository through `gh repo view`. The caller's current run is pinned at startup;
`--run` selects another run explicitly. `--json` prints one JSON event per delivered message.
Ctrl-C stops the watcher. Restarting begins a new observation session and can repeat a
notification; no persistent watch registration is created.

## CI selection

Each poll reads the PR head and base, effective branch rules, workflow runs, check runs,
workflow jobs and legacy commit statuses. Pagination is enabled. An incomplete workflow
run listing is an error. The head/base are checked again before reporting results; changes
restart observation. Only that head counts. For each workflow, the newest run ID wins,
with the newest attempt winning within a run. Only jobs from that attempt count; old
cancelled/failed jobs cannot override their replacement. Check names are deduplicated within
a workflow by newest job ID. Third-party check runs and legacy statuses use their newest
IDs per app/name and context, respectively.

Required contexts come from the union of classic branch protection and active branch
rulesets, including app bindings. A missing required context remains pending. Success,
neutral and skipped check conclusions pass; optional failures do not fail required CI.
Duplicate required names emitted by separate workflows must all pass. A selected workflow
that has not materialized any jobs remains pending; a workflow emitting required jobs must
finish before green. Two identical polls are required before reporting a terminal CI result.
`ci green` describes required CI for the PR head, not reviews, deployments, merge-group CI,
or permission to merge. With an explicitly known empty requirement set, observed check
evidence is sufficient; no checks at all remains pending.

If either policy API is unavailable, configure a repository-specific fallback list:

```toml
[projects.myapp]
repo = "you/myapp"
required_checks = ["test", "lint"]
```

The fallback augments any requirements successfully discovered and generates a warning.
An omitted list is different from `required_checks = []` (explicitly no fallback gates).
A 404 alone is not proof of no protection; an independently verified unprotected branch
is required. Required-workflow rules also require this context-name fallback. Legacy status
contexts do not expose an app binding through the REST status list, so they cannot satisfy
an app-pinned requirement. Use a check run from the required app in that case.

## Notifications and exit behavior

The coordinator receives `ci green`, `required check failed: <name> <url>`,
`left merge queue: <reason>`, and `merged`. The latest queue timeline transition supplies
removal reasons; a later re-entry suppresses an older removal. Missing reasons are explicit.
Repeated observations are deduplicated for the life of the process.

`--until green` (default) returns success after stable required CI or an already merged PR;
a stable required failure is posted and exits nonzero. `--until merged` continues through
CI failures and queue removals until GitHub reports merged. API/delivery errors and a PR
closed without merging exit nonzero, never masquerading as required-check failures.

`--interval 15s` controls polling. `--race-window 5s` warns if ready-for-review occurs within
that window after the head's push timestamp. When GitHub omits `pushedDate`, the watcher
uses push-workflow creation time or two nearby runs of the same PR workflow as evidence
of a possible push/ready race, explicitly labelled as an inference. The most recent 100
ready/queue timeline events are inspected. No warning means no observed race evidence,
not proof that no race occurred. A zero race window checks same-timestamp events only.

API references: [effective branch rules](https://docs.github.com/en/rest/repos/rules#get-rules-for-a-branch),
[branch protection](https://docs.github.com/en/rest/branches/branch-protection#get-branch-protection),
[PR timeline events](https://docs.github.com/en/graphql/reference/pulls).
All checked-in API recordings under `internal/prwatch/testdata` are synthetic.
