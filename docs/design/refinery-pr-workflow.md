# Refinery PR Workflow

> Design for end-to-end PR-based merge flow: polecat pushes, refinery creates
> and shepherds a GitHub PR through an automated AI review loop, and a human
> approval gates the final merge to a protected branch.

## Status

Draft — implementation not started. Supersedes the ad-hoc overlay that lives
in `jira_claude_channel`.

## Motivation

Gastown ships with `merge_strategy=pr` plumbing in the Go layer
(`internal/refinery/engineer.go` — `doMergePR()`, the `PRProvider` abstraction,
rig-level `MergeQueueConfig.MergeStrategy`), but the agent-facing layer
(guards, templates, formula, `gt sling`, `gt done`) assumes direct-to-main.
Multiple users running against GitHub branch-protection rulesets that require
PRs have filed issues describing the same failure mode: refineries run but do
nothing; polecats bypass `gt done` and create PRs by hand; mayors end up doing
manual git. See #3601, #3588, #3602, #3198, #3320, #3406, #3604, #3249, #3363.

This design closes the gap in one coherent change: when a rig sets
`merge_strategy=pr`, the refinery owns PR lifecycle, dispatches polecats to
address review comments, and blocks on a human approval before the final
squash merge.

## Goals

1. A rig opts in with `merge_queue.merge_strategy=pr` and gets a working
   end-to-end flow with no manual overlay, no manual hooks-override, no
   per-rig templates.
2. Refinery, not polecats, owns PR creation. `gh pr create` is blocked for
   polecats regardless of strategy (a polecat creating its own PR defeats the
   consolidation/batching refinery can do).
3. Automated AI review runs as a bounded loop against the PR; actionable
   comments trigger a review-fix polecat dispatch; the loop terminates when
   no unresolved threads remain or on escalation.
4. Human approval is the merge gate. The refinery never squash-merges a PR
   that lacks an approving review from the configured reviewer.
5. Direct-merge (`merge_strategy=direct`, the default) continues to work
   unchanged.

## Non-Goals

- Cross-fork PRs (#3249). Called out as a follow-up; out of scope here.
- Per-PR configurable review policies beyond reviewer + required-approvals
  count. #3406's richer `pr_requirements` map is deferred.
- GitHub auto-merge / merge-queue delegation. The refinery does the squash
  merge itself via `gh pr merge` once human approval + CI green are observed.
  Auto-merge is an optional follow-up after the basic flow is stable.
- Branch-consolidation PRs (#3604). This design assumes one PR per source
  issue, matching current refinery behavior. Consolidation is orthogonal.

## Actors

| Actor | Responsibility under `merge_strategy=pr` |
|-------|-------------------------------------------|
| Polecat | Implement work on a feature branch. Push branch. Run `gt done` with no PR creation. On review-fix dispatch, check out the existing PR branch, commit fixes, reply to review threads. |
| Witness | Verify polecat work, send `MERGE_READY` to refinery. Unchanged from direct-merge flow. |
| Refinery | Rebase branch, push, create PR, wait for CI, request AI review, detect unresolved threads, dispatch review-fix polecat, poll for human approval, squash-merge, clean up. |
| Mayor / Human | Review the AI's work, approve the PR on GitHub. Close the escalation that the refinery opens when it parks waiting for human merge. |

## End-to-End Flow

```
Polecat                Witness            Refinery                      GitHub
  │                       │                   │                             │
  ├─ commit + push ──────▶│                   │                             │
  │                       ├─ verify ─────────▶│                             │
  │                       │    MERGE_READY    │                             │
  │                       │                   ├─ rebase temp on main        │
  │                       │                   ├─ push branch ──────────────▶│
  │                       │                   ├─ gh pr create ─────────────▶│ PR#N
  │                       │                   ├─ wait CI ──────────────────▶│
  │                       │                   │         ◀── CI green ───────│
  │                       │                   ├─ gh /request-review ───────▶│ augment
  │                       │                   ├─ poll unresolved threads ──▶│
  │                       │                   │         ◀── threads ────────│
  │                       │                   │                             │
  │  ◀─ sling review-fix ─┤                   │ (if unresolved)             │
  ├─ checkout PR branch ──┤                   │                             │
  ├─ fix + commit + push ─┼──────────────────▶│──────────────────────────┬─▶│
  ├─ reply threads ───────┼───────────────────┼──────────────────────────┼─▶│
  ├─ gt done (no-merge) ──▶                   │                             │
  │                       ├─ signal refinery ▶│                             │
  │                       │                   ├─ re-poll threads ──────────▶│
  │                       │                   │   (loop bounded by N)       │
  │                       │                   │                             │
  │                       │                   ├─ escalate "ready for human" │
  │                       │                   │   (park; watcher on PR)     │
  │                       │                   │                             │
  │                                     human approves on GitHub ──────────▶│
  │                       │                   │         ◀── APPROVED ───────│
  │                       │                   ├─ gh pr merge --squash ─────▶│
  │                       │                   ├─ mq post-merge              │
  │                       │                   ├─ mail mayor/ (persistent)   │
```

## Design

### Configuration surface

Rig settings (`<rig>/settings/config.json`):

```json
{
  "merge_queue": {
    "enabled": true,
    "merge_strategy": "pr",
    "vcs_provider": "github",
    "pr_reviewer": "augment",
    "pr_approver": "kevinpjones",
    "pr_required_approvals": 1,
    "pr_review_loop_max": 3,
    "pr_merge_method": "squash"
  }
}
```

New fields on `config.MergeQueueConfig` (`internal/config/types.go`):

- `PRReviewer string` — GitHub user/bot to request a review from. Defaults
  to empty (no automated review requested; loop skipped, flow jumps
  straight to the human-approval gate).
- `PRApprover string` — GitHub user whose approving review gates the
  merge. Required when `merge_strategy=pr`. Hard-fail on refinery start if
  unset.
- `PRRequiredApprovals int` — approvals required. Defaults to 1.
- `PRReviewLoopMax int` — max polecat dispatch cycles per PR. Defaults to 3.
- `PRMergeMethod string` — passed to `gh pr merge`. Defaults to `squash`.

Existing `RequireReview *bool` becomes an alias for `PRRequiredApprovals>0`
for backward compatibility; remove after one release.

### Flow ownership boundary: who creates the PR

The refinery creates the PR. Polecats never call `gh pr create`. Enforcement:

- `gt tap guard pr-workflow` blocks `gh pr create` for polecat contexts
  regardless of `merge_strategy` — this is the existing behavior and stays.
- The guard is made `merge_strategy`-aware only for the refinery context,
  and only to *allow* `gh pr create`. Polecats remain blocked. (Fixes #3601
  root cause without opening the floodgates.)

### Formula + template rewrites

The refinery formula (`mol-refinery-patrol.formula.toml`) already has a
branch on `merge_strategy=pr` inside the `merge-push` step, but it's
incomplete (doesn't handle the review loop, doesn't escalate on approval
wait, and doesn't reconcile with the Go-side `doMergePR`). It will be
rewritten as a set of dedicated steps:

```
queue-scan → process-branch → rebase → run-tests → handle-failures
                                                        │
                                                        ▼
                              ┌──── (direct) ────┐  ┌── (pr) ──────────────────┐
                              │                  │  │                          │
                           merge-push          pr-push                         │
                              │                  │                             │
                              │               pr-create                        │
                              │                  │                             │
                              │               pr-wait-ci                       │
                              │                  │                             │
                              │               pr-request-review                │
                              │                  │                             │
                              │               pr-review-loop ─┐                │
                              │                  │            │                │
                              │                  ▼            │                │
                              │               pr-wait-human ◀─┘                │
                              │                  │                             │
                              │               pr-merge                         │
                              └──────────┬───────┘                             │
                                         ▼                                     │
                                  post-merge (common)                          │
```

`pr-review-loop` is a formula step with an explicit counter bounded by
`PRReviewLoopMax`. Each iteration: poll unresolved review threads; if none,
advance; if some, sling a review-fix polecat with `no_merge=true` and wait
for its completion signal; on any loop error, escalate and park.

The template (`internal/templates/roles/refinery.md.tmpl`) removes its
hardcoded `merge-push` summary (lines 231-239 today) and instead renders
a conditional block driven by the `MergeStrategy` template var. The two
branches reference the formula steps by ID — the template no longer
duplicates instructions. (This is the root-cause fix for the
"template contradicts overlay" issue documented in
`docs/notes/refinery-template-contradicts-overlay.md`.)

### Go-side changes

`internal/refinery/pr_provider.go` grows three methods on the `PRProvider`
interface:

```go
type PRProvider interface {
    FindPRNumber(branch string) (int, error)
    IsPRApproved(prNumber int) (bool, error)
    MergePR(prNumber int, method string) (string, error)

    // New:
    CreatePR(opts CreatePROptions) (prNumber int, url string, err error)
    RequestReview(prNumber int, reviewer string) error
    UnresolvedThreads(prNumber int) ([]ReviewThread, error)
}
```

Rationale: the agent-facing formula can still drive this, but the Go layer
owns the mechanics. The formula becomes a thin orchestrator over the
`gt refinery pr …` subcommands that expose these primitives. This removes
the chicken-and-egg problem where `doMergePR` finds PRs but never creates
them (documented gap in today's code).

A new `gt refinery pr` command tree, used by the formula:

| Command | Purpose |
|---------|---------|
| `gt refinery pr push <branch>` | Force-push rebased branch safely (`--force-with-lease`) |
| `gt refinery pr create <branch>` | Create PR with title/body from source issue; idempotent (returns existing PR if one exists) |
| `gt refinery pr wait-ci <pr>` | Block until required checks pass; exit non-zero on fail |
| `gt refinery pr request-review <pr> <reviewer>` | `gh api … /requested_reviewers` |
| `gt refinery pr threads <pr>` | JSON: unresolved threads with body + file/line + author |
| `gt refinery pr wait-approval <pr> --approver=<user> --escalate` | Poll for approval; on first call, open an escalation; return when approved |
| `gt refinery pr merge <pr> --method=squash` | Delegates to `prProvider.MergePR` |

These keep Go as the implementation but let the formula express the
orchestration, which is where conditional branching and loops are natural.

### `gt sling` stamps `merge_strategy` on the work bead

Fixes #3320 and unblocks `gt done` from knowing how to route. One-line
change in `internal/cmd/sling.go` where the convoy is created: the convoy's
`merge_strategy` is already stamped on the convoy bead today; it must also
be stamped in the work bead's `AttachmentFields.MergeStrategy` (the field
already exists in `internal/beads/fields.go:26`; the write just doesn't
happen).

### `gt sling --pr <number>` for review-fix dispatch

Fixes #3602. When the refinery dispatches a review-fix polecat, the polecat
needs to check out the existing PR branch, not create a new one. Add
`--pr <number>` and `--branch <name>` flags to `gt sling`; sling then:

- Skips the normal `polecat/<worker>/<slug>` branch computation.
- Writes the target branch into the bead's attachment fields so
  `gt prime` / polecat formula can check it out on startup.
- Stamps `no_merge=true` (polecat must not trigger its own merge path;
  the refinery re-polls threads after the polecat signals done).

### `gt done` completion path for review-fix polecats

Fixes #3363 as a prerequisite. When a polecat with `no_merge=true` has
commits and calls `gt done`, the work bead must close (currently it
silently stays open — see `internal/cmd/done.go:684-714`). For the PR
workflow specifically, the polecat additionally sends a `REVIEW_FIX_DONE`
mail to the refinery so the review loop can re-poll without waiting for
a timer.

### Refinery parks on human approval with an escalation

The `pr-wait-approval` step opens an escalation the first time it runs
for a given PR:

```
gt escalate -s MEDIUM \
  "PR #$PR ready for human approval" \
  --mail mayor/ \
  --context "PR: $PR_URL\nIssue: $ISSUE_ID\nReviewer: $PR_REVIEWER\nApprover: $PR_APPROVER"
```

The mayor receives a persistent mail (not an ephemeral nudge — this fixes
the problem documented in `docs/notes/refinery-merge-notification-to-mayor.md`
by routing through mail rather than nudge). The human approves on GitHub;
the refinery's poll sees APPROVED; it merges and closes the escalation.

### Hooks: what changes, what doesn't

`internal/cmd/tap_guard.go:runTapGuardPRWorkflow`:

- Keep the `isGasTownAgentContext()` check.
- Add: if the caller is the refinery (`GT_REFINERY` set) AND the current
  rig has `merge_queue.merge_strategy=pr`, allow `gh pr create` and friends.
- Everyone else stays blocked.

No changes needed to the default hooks templates
(`internal/hooks/templates/claude/settings-autonomous.json` etc.) — they
keep the matcher, the guard just becomes smarter about when to exit 0.

## Gap Analysis — What Exists vs. What's Needed

| # | Area | Today | After this change |
|---|------|-------|-------------------|
| 1 | `MergeQueueConfig.MergeStrategy` | Exists, threaded through formula vars | Unchanged; grows `PRApprover`, `PRReviewer`, `PRReviewLoopMax`, `PRMergeMethod` |
| 2 | `doMergePR()` | Assumes PR exists; only finds + merges | Keeps merge responsibility; PR creation moved to `PRProvider.CreatePR` |
| 3 | `PRProvider` interface | `FindPRNumber`, `IsPRApproved`, `MergePR` | Adds `CreatePR`, `RequestReview`, `UnresolvedThreads` |
| 4 | `gt tap guard pr-workflow` | Unconditionally blocks `gh pr create` in agent contexts | Allows for refinery when `merge_strategy=pr`; polecats still blocked |
| 5 | `refinery.md.tmpl` quick-reference | Hardcoded direct-merge summary | Conditional on `MergeStrategy`; references formula step IDs |
| 6 | `mol-refinery-patrol.formula.toml` | Has partial `pr` branch inside `merge-push` | Dedicated `pr-*` steps for push/create/wait-ci/review/loop/wait-human/merge |
| 7 | `gt sling` merge_strategy stamp | Not stamped on work bead (#3320) | Stamps `AttachmentFields.MergeStrategy` |
| 8 | `gt sling --pr/--branch` | Absent (#3602) | New flags for review-fix dispatch |
| 9 | `gt done` no-merge close path | Leaves bead open when polecat has commits (#3363) | Closes bead on no-merge + commits |
| 10 | Mayor notification of merge | `gt nudge` — ephemeral | `gt mail send mayor/` — persistent; nudge side effect retained |
| 11 | `gt refinery pr …` subtree | Absent | New commands for push/create/wait-ci/request-review/threads/wait-approval/merge |

## Code Touchpoints

Primary:
- `internal/config/types.go` — add PR fields to `MergeQueueConfig`
- `internal/config/loader.go` — validate `PRApprover` is set when `merge_strategy=pr`
- `internal/refinery/pr_provider.go` — extend interface
- `internal/refinery/pr_provider_github.go` — implement new methods via `gh`
- `internal/refinery/pr_provider_bitbucket.go` — implement new methods (or return `ErrUnsupported` for Bitbucket in phase 1)
- `internal/git/git.go` — wrappers for `gh pr create`, `gh api /requested_reviewers`, `gh api /pulls/{n}/comments`
- `internal/refinery/engineer.go` — `doMergePR` delegates creation to provider
- `internal/cmd/refinery_pr.go` — new file; `gt refinery pr …` subcommand tree
- `internal/cmd/tap_guard.go:runTapGuardPRWorkflow` — refinery-aware allow
- `internal/cmd/sling.go` + `sling_dispatch.go` — `--pr`/`--branch` flags + `MergeStrategy` stamp
- `internal/cmd/done.go` lines 684-714 — close bead on no-merge + commits
- `internal/formula/formulas/mol-refinery-patrol.formula.toml` — new `pr-*` steps
- `internal/templates/roles/refinery.md.tmpl` lines 231-239 — conditional render

Secondary / tests:
- `internal/refinery/engineer_pr_merge_test.go` — extend for new interface methods
- `internal/cmd/sling_test.go` (+ dispatch tests) — `--pr` flag behavior
- `internal/cmd/done_test.go` — no-merge close path

## Backward Compatibility

- `merge_strategy=direct` (default) is unaffected. The formula keeps its
  `merge-push` step intact; the template's direct-merge branch is what
  rigs without `merge_strategy=pr` set see.
- `RequireReview *bool` is honored as `PRRequiredApprovals>0` for one
  release, then removed.
- Rigs already running with the `jira_claude_channel` overlay can delete
  the overlay and per-rig hooks-override once this lands. The overlay
  becomes a no-op (it references the same step IDs); delete is a chore,
  not a migration.

## Open Questions

1. Should `pr-wait-approval` use GitHub webhooks instead of polling?
   Polling keeps the refinery dependency-free; webhooks would require a
   local listener. Starting with polling; webhooks are a later optimization.
2. How does the review loop handle a reviewer that leaves a comment *and*
   an approving review? Proposal: if `APPROVED` is present AND no
   unresolved threads exist, merge. If `APPROVED` is present but threads
   remain unresolved, treat threads as higher-priority and continue the
   loop — the human can always re-review after fixes.
3. What's the budget for `pr-wait-approval`? Hard-cap at 24h with
   re-escalation at 4h stale? Defer to the escalation subsystem's stale
   detection — don't re-invent it here.

## Related Issues

Gaps this design closes:

- #3601 — merge_via: github-pr mode
- #3588 — polecats/refinery unable to create/merge PRs
- #3602 — polecats can't work on existing PR branches
- #3320 — `gt sling` does not stamp MergeStrategy
- #3198 — refinery closes upstream PRs / deletes branches
- #3363 — `gt done` no-merge leaves bead open
- #3406 — (partial) `pr_requirements` — this design covers reviewer +
  required approvals; richer requirements remain open

Out of scope, filed as follow-ups:

- #3249 — `fork_remote` for cross-fork PRs
- #3604 — refinery stalls during branch consolidation

## Implementation Plan

Phase 1 — plumbing (no agent-visible change yet):
1. `MergeQueueConfig` new fields + validation
2. `PRProvider` interface extension + GitHub impl
3. `gt refinery pr` command tree

Phase 2 — agent flow:
4. Formula: new `pr-*` steps
5. Template: conditional quick-reference
6. Guard: refinery-aware allow

Phase 3 — fixes that unblock the loop:
7. `gt sling --pr/--branch` + `MergeStrategy` stamp (#3320, #3602)
8. `gt done` no-merge close path (#3363)
9. Mayor mail-send on merge (replaces nudge)

Phase 4 — delete the overlay:
10. Remove `jira_claude_channel/formula-overlays/mol-refinery-patrol.toml`
    and `~/.gt/hooks-overrides/jira_claude_channel__refinery.json` as a
    chore; end-to-end test passes without them.

Each phase is a standalone PR. Phase 1+2 get the flow working against a
test rig; phase 3 makes it pleasant; phase 4 is the acceptance test.
