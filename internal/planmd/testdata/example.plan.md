# Day plan 2026-03-14

approved: owner, 2026-03-14 ~09:10Z ("Reviews first, then the build"; ACME-44 → backlog).

## Summary
- Ship today: reviews on two open PRs — #210 (ACME-41, login fix), #211 (ACME-42, docs search) — then merge asks.
- Build today: ACME-50 (CSV export, slice 1/2 of ACME-49), dispatched after the reviews are under way.
- Batched small work: none.
- Capacity: 3 local slots (52% free RAM); mobile slot used by I1.
- Back to the owner: verdict ratifications and merge asks per PR; #212 docs chore evidence review.

## What
| # | work | issues | where | model | output |
|---|---|---|---|---|---|
| R1 | review #210 (login fix) | ACME-41 | local slot | claude-opus-5-5 (cross-vendor) | verdict + fixes on #210 |
| R2 | review #211, then UI review | ACME-42 | local slot | codex | verdict, UI verdict |
| C1 | #212 retrigger CI | — | coordinator | — | checks run; evidence review |
| B1 | build ACME-50 | ACME-50 (parent ACME-49) | local slot (after reviews) | claude-sonnet-5-5 build; codex review | PR |
| I1 | build ACME-60 (settings screen) | ACME-60 | mobile slot | claude-sonnet-5-5 build; codex review | PR |
| W1 | internal tool: plan board backend | — | local slot (next free) | claude-opus-5-5 | tool PR |

## Why
- **R1** — login sessions expire early for SSO users; the fix needs an independent review before it ships.
- **R2** — docs search is the most-used help path and returns stale pages.
- **C1** — a docs-only chore with zero check runs stalls silently.
- **B1** — every later export slice depends on this one.

## How
- Lanes: R1/R2 start now in two local slots; B1 takes the third when a review frees it.
- Review routing cross-vendor: Claude-built → Codex, Codex-built → Claude.

| capacity | now | plan uses |
|---|---|---|
| local workers | 0 / 3 | 3 |
| mobile workers | 0 / 1 | 1 |
| free RAM | 52% | ≥ 30% |

## Steers (owner, 2026-03-14 ~11:00Z)
- **Mobile slot for I1** — What: I1 runs in parallel on the mobile slot. Why: the settings screen is next on the roadmap.
- **Plan board (W1)** — What: a small internal tool to view the plan; backend first. Why: owner request.

## Decisions that come back to the owner
- **Verdicts R1–R2** — What: ratify each review verdict. Why: close-out verdicts are always the owner's.
- **Merges** — What: code-PR merge asks with evidence. Why: policy.
- **#212** — What: evidence review of the docs chore. Why: only lessons-only chores auto-merge.

## Held on purpose
| item | what | why held |
|---|---|---|
| #205 | ACME-30 legacy importer | Superseded by ACME-49; close after ACME-50 merges |
| ACME-44 | onboarding revamp | Blocked by 6 open issues → backlog (owner) |
| ACME-51 / 52 | ACME-49 slices b, c | Sequenced after ACME-50 |
| ACME-12, 17, misc epics | backlog | Not aligned this week |

## Items
| id | issues | kind | lane | model | state | PR | dispatch ref |
|---|---|---|---|---|---|---|---|
| R1 | ACME-41 | review | local | claude-opus-5-5 | merged | #210 | horch:r1/t1/d1@w2:p1 |
| R2 | ACME-42 | review | local | codex | merged | #211 | horch:r1/t2/d2@w3:p1 |
| U2 | ACME-42 | ui-review | local | claude-opus-5-5 | settled | #211 | horch:r1/t3/d3@w3:p2 |
| C1 | — | chore-ci | coordinator | — | merged | #212 | retrigger 1a2b3c4 |
| B1 | ACME-50 | build | local | claude-sonnet-5-5 | dispatched | | horch:r1/t5/d5@w4:p1 |
| I1 | ACME-60 | build | mobile | claude-sonnet-5-5 | settled | #213 | horch:r1/t6/d6@w5:p1 |
| I1r | ACME-60 | review | mobile | codex | dispatched | #213 | horch:r1/t7/d7@w6:p1 |
| W1 | — | build (internal tool) | local | claude-opus-5-5 | dispatched | | horch:r1/t8/d8@w7:p1 |
