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
`PRReviewLoopMax`. Crucially, the loop is **patrol-resumable** — on each
refinery patrol cycle the step takes a single action and returns control
to the patrol, it does not busy-wait inside a `while true` for the polecat.
This keeps the merge queue moving: the refinery can process other MRs
while a polecat is addressing review threads on one PR.

State per MR (stored on the MR bead's description as attachment fields):

- `review_loop_iter` int — number of review-fix dispatches already made.
- `review_fix_polecat` string — name of the currently-dispatched polecat,
  or empty when no polecat is in flight.

Step logic on each patrol cycle, for an MR bead that has reached
`pr-review-loop`:

1. If `review_fix_polecat` is set and that polecat is still alive
   (`gt polecat status` reports non-terminal): the previous dispatch is
   still working. Record the MR as "awaiting-review-fix" and skip to
   `loop-check` so the refinery can process other MRs. On the next
   patrol cycle, re-enter at step 1.
2. If `review_fix_polecat` is set and the polecat has terminated (either
   normally — `gt done` sent `REVIEW_FIX_DONE` mail — or abnormally —
   nuked by witness, exited with error): clear the field; proceed.
3. Poll `gt refinery pr threads $PR --unresolved`.
4. If zero unresolved threads: advance to `pr-wait-human`.
5. Else if `review_loop_iter >= PRReviewLoopMax`: open an escalation,
   park the MR; proceed to `loop-check`.
6. Else: sling a review-fix polecat (`gt sling review-fix/<issue> <rig>
   --pr $PR --branch <polecat-branch>`); record
   `review_fix_polecat=<polecat-name>` and
   `review_loop_iter=<iter+1>` on the MR bead; proceed to `loop-check`.

`REVIEW_FIX_DONE` mail from the polecat is an optimization, not a
correctness requirement: it surfaces the refinery's next patrol sooner
(via `gt mail check --inject`), but even without it the patrol's normal
cadence would re-enter step 1 and observe the polecat's terminal status
within one cycle.

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
    IsPRApprovedBy(prNumber int, user string) (bool, error)
    MergePR(prNumber int, method string) (string, error)

    // New in Phase 1:
    CreatePR(opts CreatePROptions) (prNumber int, url string, err error)
    RequestReview(prNumber int, reviewers []string) error
    UnresolvedThreads(prNumber int) ([]ReviewThread, error)
    AllThreads(prNumber int) ([]ReviewThread, error)
    CountApprovals(prNumber int) (int, error)
    ChecksRollup(prNumber int) (state string, done bool, err error)
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
| `gt refinery pr create --branch=<b> --base=<t> --title=<s> --body=<s>` | Idempotent PR create; returns existing PR if one is already open for the branch |
| `gt refinery pr wait-ci <pr>` | Poll until checks rollup is terminal; exit non-zero on failure or timeout |
| `gt refinery pr request-review <pr> --user=<u>` | `gh pr edit --add-reviewer` (repeatable) |
| `gt refinery pr threads <pr> [--unresolved]` | JSON threads with body + file/line + author; `--unresolved` (default) filters |
| `gt refinery pr wait-approval <pr> --approver=<u> --min-approvals=<n> --escalate` | Two gates: specific-user approval + distinct-reviewer count; poll until both met |
| `gt refinery pr merge <pr> --method=squash` | Delegates to `PRProvider.MergePR` |

Branch push is not a subcommand — the formula runs `git push` directly (with
`--force-with-lease` on rebased branches). Wrapping it in Go would add no value.

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

### Resolved

- **Should the review-fix loop block the refinery?** No. Phase 5 (the
  top of this stack) converted Step PR.5 from a `while true` bash loop
  inside the formula to a patrol-resumable state machine: each patrol
  cycle takes one action (check polecat status / poll threads / dispatch /
  advance) and returns to `loop-check`, letting the refinery continue
  processing other MRs. State (iteration counter + in-flight polecat
  name) lives on the MR bead. See the "Formula + template rewrites"
  section above.

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

Phase 4 — acceptance + cleanup:
10. Add an integration test in this repo that parses the embedded
    `mol-refinery-patrol.formula.toml`, locates the `merge-push` step,
    and asserts under the `If merge_strategy = "pr":` gate marker that
    the step's description drives the flow through `gt refinery pr …`
    subcommands (not raw `gh pr create --base/--head`, `gh pr merge
    <branch>`, or `gh pr checks <branch>`). The test is a substring
    guard on the parsed step text — it does not instantiate a wisp or
    substitute vars; instantiation-time validation is orthogonal and
    remains the domain of `TestParseRealFormulas` +
    `variable_validation_test`. This cheap static check is enough to
    protect the Phase 2 refactor from silent regression.
11. **Out of this repo** — in the user's `~/gt` workspace, remove
    `jira_claude_channel/formula-overlays/mol-refinery-patrol.toml` and
    `~/.gt/hooks-overrides/jira_claude_channel__refinery.json` as a
    chore. Once Phases 1-3 land, the overlay becomes a no-op (references
    the same step IDs). Delete is a chore, not a migration. This item
    lives in the user's workspace because per-rig overlays and
    hooks-overrides are runtime state, not source code; there is no
    gastown PR for this step.

Each phase is a standalone PR. Phase 1+2 get the flow working against a
test rig; phase 3 makes it pleasant; phase 4 is the acceptance test.

## Dogfood observations (Telegraph v1, 2026-04-19)

First real run of `merge_strategy = "pr"` on the `gastown` rig. Epic
`gt-6if` (Telegraph v1) filed with 1 design-doc bead + 6 impl/infra
children.

**Headline finding: the design doc landed on `main` with no PR, with
no review, and with the refinery acting as both reviewer and merger —
the entire PR workflow was bypassed on the first bead.** The two
downstream items (G2 witness reporting, G3 mayor unblocking on close)
and G4 (prefix mismatch) are smaller, related effects.

### G1 — Refinery improvises a fast-forward merge when the MR bead is missing, bypassing `merge_strategy = "pr"`

**Trace of what actually happened** for `gt-vd8` (design doc):

1. Polecat `furiosa` committed `docs/design/telegraph.md` on
   `polecat/furiosa-mo636ayu` and ran `gt done`.
2. `gt done` pushed the branch to origin (verified: `✓ Branch pushed
   to origin` in the polecat's session transcript).
3. `gt done` then tried to create the MR bead and **failed** with:
   ```
   bd create … --rig=gastown …: Error: unknown flag: --rig
   ```
   `bd create` does not have a `--rig` flag — it has `--repo`. `gt
   done` fell through to `notifyWitness` with `mrFailed=true`, the
   polecat session exited IDLE with "Work needs recovery (push or MR
   failed)".
4. The refinery's patrol cycle ran `bd list --label=merge-queue
   --status=open --json` → empty. It then *improvised*: it ran `git
   fetch origin polecat/furiosa-mo636ayu`, read the design doc,
   self-approved ("All required sections present. Clean fast-forward
   merge — proceeding."), and executed
   `git push origin FETCH_HEAD:refs/heads/main` followed by
   `bd update gt-vd8 --status=closed` and `git push origin --delete
   polecat/furiosa-mo636ayu`.

Two compounding bugs in our Phase 1-3 work:

- **(a) `beads.CreateOptions.Rig` passed a nonexistent bd flag (pre-fix).**
  Before this stack landed, `internal/beads/beads.go` appended
  `--rig=<name>` to the `bd create` invocation, but `bd create` only
  accepts `--repo`. Every MR bead create under a rig errored out with
  "unknown flag: --rig". This was the triggering cause of the incident
  — with MR creation working, the refinery would have taken the
  formula's `pr-create → wait-ci → request-review → wait-approval →
  merge` path. The first PR in this stack fixes the mapping (`--rig`
  → `--repo`) and adds a regression test.
- **(b) The refinery LLM treats "branch on origin, no MR" as
  permission to improvise.** The `mol-refinery-patrol` formula does
  not cover the "polecat branch exists but no MR bead" state, so the
  refinery session resolves it by hand — and its default resolution
  is to merge by fast-forward push to main, acting as self-reviewer.
  This directly contradicts the whole purpose of
  `merge_strategy = pr`.

Fix direction (three layers, in order):

1. **Rename `CreateOptions.Rig` → `CreateOptions.Repo`** or translate
   it to `--repo` at the call site. Single-line fix; unblocks the
   entire pipeline. Add an integration test that slings a bead in a
   rig and asserts MR bead creation succeeds.
2. **Broaden the tap-guard beyond `gh pr create`.** Today's matcher
   is `Bash(gh pr create*)`. Under `merge_strategy = pr`, *no agent
   except the refinery PR-merge step* should be allowed to push to
   `main`. Add matchers for `Bash(git push * main*)`,
   `Bash(git push *:refs/heads/main*)`, `Bash(git push origin HEAD:main*)`.
   The refinery's `merge` step runs `gh pr merge`, not `git push
   origin :main`, so this is safe.
3. **Make the formula explicit about "no MR, branch exists" state.**
   Add an error branch that escalates rather than improvising: if the
   refinery patrol finds a polecat branch on origin that's not
   covered by any open MR, it should open an escalation bead, not
   merge.

### G2 — Witness reports `"status": "merged"` for work that took the bypassed path

`~/gt/<rig>/witness/state.json` recorded furiosa's completion as
`status: merged`. That is technically accurate *after* the refinery's
improvised fast-forward, but it collapses three very different
outcomes onto the same field: "bead closed, no merge", "merged via
PR", "merged by refinery improvisation / direct push". The status
should carry the *how* (`pr-merged`, `direct-pushed`,
`closed-no-merge`) so dashboards and humans can tell when the PR
workflow was or wasn't actually used. Pre-existing semantics; becomes
load-bearing under `merge_strategy = pr`.

### G3 — Mayor unblocks `blocks:` dependents purely on bead `closed`

When `gt-vd8` closed, the mayor slung L1/L2/L3 immediately, regardless
of the fact that the improvised "merge" had just happened without any
review. The bead's own acceptance clause *"Reviewed by Overseer before
impl unblocks"* was never honored. This is independent of G1 — even
with G1 fixed (merge via PR, approval required), the mayor currently
treats `closed` as the unblock signal, but `closed` can fire on
escalation / DEFERRED / no-merge close paths too.

Options:

- **Hold** — don't auto-unblock on close; require an explicit
  `gate:unblock <bead>` signal (new primitive).
- **Tie** — require the bead's MR to have *landed on `main`* (not
  just bead-closed) before the mayor treats `blocks` as cleared.

Tie is cleaner because with G1 fixed, "landed on main" *is* the
review-passed signal, and no new primitive is needed.

### G4 — `gt rig add` leaves rig `.beads/config.yaml` prefix out of sync with `rigs.json`

Orthogonal to the PR workflow but hit during kickoff. Town
`routes.jsonl` declared `gastown.beads.prefix = "gts"`; the rig DB's
`.beads/config.yaml` used `prefix: gt` (default). First sling from the
mayor failed with `no route found for prefix "gt-"`. Workaround: added
a `gt-` route to `~/gt/.beads/routes.jsonl`. Real fix: `gt rig add`
should either propagate the declared prefix into the new rig's beads
config at provisioning time, or assert agreement between the two
sources and fail loudly. File as a separate gastown bug.

### G5 — `gt deacon start` / `restart` leaves the Claude session parked in INSERT mode

After the fix-stack landed and I restarted services for round two of
the Telegraph v1 dogfood, `gt deacon restart` reported "✓ Deacon
session started" and `gt deacon status` showed the session running —
but the deacon's heartbeat stayed **45+ minutes stale** and no work
moved. Peeking at the tmux pane revealed the cause: the Claude Code
process inside the deacon's tmux session was sitting at an empty
prompt in `-- INSERT --` mode, waiting for human input. No patrol
loop ever started, which is why the earlier witness escalation
(`hq-kgm` at 13:02) read *"Deacon stuck in editor mode - preventing
polecat spawning"* — same failure mode, hit twice in one session.

Workaround: `tmux send-keys -t hq-deacon "gt prime --hook" Enter`
manually, which kicks Claude into the priming sequence and the patrol
starts. The same failure likely applies to mayor/witness restarts,
but my mayor session carried over from the pre-fix run so I didn't
observe it there.

Fix direction:

- `gt deacon start` / `restart` should pipe the initial patrol
  invocation (`gt prime --hook` or equivalent) to the new Claude
  process automatically, not rely on a human attaching to type it.
- Or: the SessionStart hook already runs `gt prime --hook`, but it's
  executing in the wrong context (doesn't feed the prompt to Claude).
  Verify and wire it up properly.

Not a blocker for merge_strategy=pr itself — but every polecat dispatch
flows through the deacon, so a stuck deacon is load-bearing on the
whole dispatch pipeline. Worth filing as a separate gastown bug
(alongside the rig-provisioning fix from G4) and covering with an
integration test that starts a deacon and asserts a patrol cycle
advances within N seconds.

### G6 — witness false-positives `-- INSERT --` status as "stuck in INSERT mode"

After the fix-stack landed and polecats were re-dispatched, the
witness fired two HIGH escalations in rapid succession:

- `hq-14a` (14:04) — "Sling succeeded (created 4 convoys) but work
  never reached hooks. All polecats completed empty and hung in
  INSERT mode. **Polecats nuked**. Investigate sling→polecat work
  routing."
- `hq-ihh` (14:13) — "Systemic INSERT mode failure: Polecats
  stalling on interactive prompts during work dispatch. furiosa
  stuck in INSERT mode on gt-ipc task."

But peeking directly at each polecat's tmux pane at the time of the
second escalation showed them ACTIVELY working:

    ✽ Orchestrating… (4m 5s · ↓ 6.1k tokens · thinking)
    …
    -- INSERT -- ⏵⏵ bypass permissions on

Claude Code's status line shows `-- INSERT --` during the normal
between-turn state — that's how Claude Code displays idle input mode
whenever it's not actively emitting a tool call. A polecat thinking
for 4 minutes shows the same banner as one that's genuinely stuck;
the witness can't distinguish from that alone.

The 14:04 escalation's first batch was probably a real failure (the
routing issue it names), but the 14:13 alert was a false positive
that triggered a destructive action: the witness nuked 4 actively
working polecats based on the same signal Claude shows during normal
thinking cycles.

Fix direction:

1. **Witness stuck-detection must not rely on tmux screen scraping
   for `-- INSERT --`.** That string is visible during normal
   operation. Use a real liveness signal instead — polecat heartbeat
   bead updates (the same mechanism the deacon/refinery use), token
   rate, or an explicit "last-activity" timestamp surfaced by Claude
   Code's session state.
2. **Don't nuke on a single alarm.** The witness should require
   multiple stale heartbeats across >N cycles before nuking, and
   polecats with active git activity (commits landing on their
   branch after the heartbeat check started) should be exempt.

This is adjacent to the PR workflow but becomes visible under it:
the PR path extends polecat work time (design/implementation is
longer than quick fixes), which gives the witness more opportunities
to false-alarm on what looks like "stuck" sessions.

### Tangent observation: sling→hook routing (first-batch failure, self-corrected)

The `hq-14a` escalation's "Sling succeeded but work never reached
hooks" description is a separate real issue from G6 — the first
post-restart batch genuinely didn't get its work (polecats spawned
with empty hooks). The mayor re-dispatched a second batch at ~14:13
and that second batch did receive work correctly. Since I can't
reproduce the routing failure from the existing evidence (the failed
first-batch polecats were nuked before I could inspect their hook
beads), I'm noting this here as something to watch for on the next
cycle rather than a definitive G-item. If it recurs, promote to a
numbered observation.

### G7 — abbreviated-patrol rules bypass the new `orphan-branch-check` step

The B2 fix added `orphan-branch-check` as an explicit step between
queue-scan and process-branch, with rule language in queue-scan's
description that reads *"Do NOT skip past orphan-branch-check on an
empty queue"*. During Telegraph v1 round two I confirmed the refinery
is seeing the new step (it lists it as a known step in its patrol
report), but **it's marking the step as SKIP on empty-queue cycles**:

    Steps: inbox-check OK | queue-scan OK | orphan-branch-check SKIP
           | process-branch SKIP | run-tests SKIP | ...

Root cause: the molecule's top-level "Abbreviated Patrol Mode" block
(lines ~96-108 of `mol-refinery-patrol.formula.toml`) has rules
predating my fix that say:

    - queue-scan: Quick `gt mq list` check. If queue is empty, skip
      directly to check-integration-branches.
    - process-branch through merge-push: Only run if queue-scan
      found work. Otherwise SKIP all.

The refinery is following the abbreviated rules' "skip directly to
check-integration-branches" instruction and treating orphan-branch-check
as part of the "everything between queue-scan and merge-push that
SKIPs on empty" block. My queue-scan rule update conflicts with
these top-level abbreviated rules, and the top-level rules win.

Consequence: 10+ orphan polecat branches have accumulated on origin
(e.g. `polecat/rictus-gt-8vr` carrying real work), and none have
been escalated. The formula guard that was supposed to catch this
state isn't firing in the cycle where it matters (empty-queue idle
loop).

Fix direction (tight scope):

1. Update the "Abbreviated Patrol Mode" section to list
   orphan-branch-check as a REQUIRED step on every cycle,
   independent of queue state. Rewrite the bullet as:

       - queue-scan: Quick `gt mq list` check.
       - orphan-branch-check: ALWAYS run — orphan polecat branches
         on origin are a failure signal regardless of queue state,
         and detecting them in the idle cycle is exactly when the
         check matters.
       - process-branch through merge-push: Only run if queue-scan
         found work. Otherwise SKIP all.

2. Extend TestRefineryPatrolHasOrphanBranchCheck to assert the
   abbreviated-patrol rules mention orphan-branch-check (or at
   minimum, don't list it in a SKIP-all group). Without this, any
   future rewrite of the abbreviated rules can re-introduce the
   same conflict silently.

3. Consider lifting orphan-branch-check out of the "if queue empty"
   branch entirely and running it unconditionally after queue-scan
   in both abbreviated and full modes. The step is token-cheap
   (one `git branch -r` + one `gt mq list --json` + a `comm`).

This is the second round of "my own fix had a silent gap that only
real-world dogfood exposes". The first was the no-merge close path
(G1); this is the abbreviated-patrol skip. Both share a pattern:
the formula is long enough that top-level flow-control rules and
per-step rules can disagree, and the refinery LLM resolves the
conflict by following whichever it sees first. File as a follow-up
PR on top of this stack — not blocking, but wanted before the next
dogfood.

### G8 — mail subsystem has no test-mode isolation; `go test ./...` pollutes production mail

While monitoring Telegraph v1 round two, the mayor's inbox filled
with ~36 `MERGED:` notifications from `gastown/nux` and
`gastown/rictus` in under five minutes. Each one had placeholder
values (`mr-a` / `feature-b` / `test-rig`) that matched no real
work — and origin `main` did not advance during the window.

My initial hypothesis was "LLM is executing documentation examples
verbatim" — **wrong**. The mayor's own diagnosis (mail
`hq-wisp-603p`, 14:27) identified the real cause:

> Polecats run `go test ./...` which exercises the L3 mail envelope
> code paths against the real Dolt-backed mail backend. There is no
> test-mode/mock backend.
> Volume was tens of mails per minute, growing — every mail is a
> Dolt commit.

So `gt-eef` (the Telegraph L3 bead — "Mail envelope + rate-limited
Mayor nudge") has unit tests that call the real `mail.Router.Send`
path, and those tests ship real mail into the mayor's inbox. Every
test run = a wave of `MERGED:` messages to the mayor. The `mr-a` /
`feature-a` / `test-rig` values are just the test fixtures that
`mail_test.go` uses.

This is a real architectural gap: the mail subsystem has no
`test-mode` / mock backend, so any code under test that touches the
mail API writes to the shared Dolt-backed queue.

Consequences under merge_strategy=pr specifically:

- **Mayor inbox DoS.** MERGED mail is how the mayor learns that
  work has landed. Tests flooding the inbox with fake MERGED mail
  makes real completion signals indistinguishable from noise. The
  mayor is forced into a "validate everything against git state"
  posture that erases the whole point of the mail-based signal.
- **Dolt write amplification.** Every test-mail is a Dolt commit.
  Combined with G9's lock contention, the test runs actively make
  the rest of the engine slower — the more tests you run, the more
  the engine blocks on Dolt.
- **Polecat self-cancellation.** The mayor correctly paused all 4
  Telegraph polecats on 2026-04-19 14:27 to stop the flood. That's
  the right call, but it means no Telegraph work can land until a
  test-isolation policy is in place — the PR workflow is paused on
  an infrastructure issue unrelated to the workflow itself.

Fix direction (mayor's proposal, reproduced with endorsement):

1. **L3 (`gt-eef`) must ship a test-mode mail backend** (in-memory
   sink or explicit dev-null) and its own tests must use it. Land
   before any other Telegraph bead so the mail-test pollution
   doesn't keep recurring.
2. **Town-level safeguard**: `mail.Router.Send` should refuse
   (or redirect to the in-memory sink) when `GT_TEST_MODE=1` (or
   `go test` is detected via `testing.Short()` / build tag) is
   active. Fail-loud so a test trying to send real mail gets an
   explicit error instead of silently polluting production.
3. **Sender validation in mayor**: under merge_strategy=pr, only
   the refinery is a legitimate sender of `MERGED:` mail. The mayor
   should reject MERGED mail from other senders (or quarantine +
   warn) rather than processing it as a completion signal.

Not blocking the B1/B2/B3 fixes that already shipped — those were
about the refinery's merge path. This is about the test surface of
the work that FLOWS through the merge path, and it's a
precondition for Telegraph v1 (or any feature whose tests touch
mail) to complete cleanly under the PR workflow. Mayor is holding
Telegraph work pending this policy call.

### G9 — embedded Dolt lock contention blocks MR bead creation under concurrent gt done

The refinery, polecats, and witness all write to
`~/gt/<rig>/mayor/rig/.beads/embeddeddolt`. Observed in the rictus
polecat pane after it tried to commit work:

    Warning: couldn't write checkpoint pushed on
    gts-gastown-polecat-rictus: bd update …: Error: failed to open
    database: embeddeddolt: waiting for lock on
    /Users/agent/gt/gastown/mayor/rig/.beads/embeddeddolt:
    context canceled

Embedded Dolt serializes writers via a file lock with a bounded
timeout. When four polecats call `gt done` around the same time
(plus the refinery's patrol cycle writes, plus the witness's
heartbeat writes), lock acquisition can time out — which fails
silently for the MR-bead-creation step. The branch gets pushed,
the polecat sends the witness notification (also mail-via-Dolt, so
also potentially locked), and exits as "failed" — which the witness
sees as a stuck polecat and nukes.

Consequence: rictus's real `gt-8vr` work is on origin as
`polecat/rictus-gt-8vr` with a clean commit, no MR bead was created
(lock timeout), no escalation fires (G7 — orphan-branch-check was
skipped), so the work sits stranded. Without the witness nuking
polecats on the timeout, a retry loop inside `gt done` would likely
handle it; but the nuke prevents the retry.

Fix direction:

1. **`gt done` should retry MR-bead creation with exponential
   backoff on lock-contention errors** (distinguish
   `context canceled` / lock-timeout from other `bd create` errors
   — retry the former, fail fast on the latter).
2. **Queue the Dolt writes outside the polecat's critical path.**
   Current design synchronously writes the MR bead from the polecat;
   a small write-ahead buffer (filesystem journal) that the refinery
   drains on its next cycle would decouple polecat completion from
   Dolt availability.
3. Short-term: widen the Dolt lock timeout and document the
   contention failure mode in `gt done`'s output so the witness
   doesn't nuke on a retryable error.

Adjacent to G6 (witness nuking working polecats) — these two
compound: Dolt lock timeouts look like stuck polecats, witness
false-positives as INSERT mode stalls, polecat gets nuked before
retry, work stranded. Either one alone would be survivable; together
they make the PR workflow unreliable under load.

### G10 — Layer 1 mail guard is bypassed by subprocess `gt mail send`

PR #9's `checkTestSendGuard` fires inside `mail.Router.Send()` when
`os.Args[0]` ends in `.test` — it catches any in-process call from
a Go test binary. That closes the "`go test` invokes Router.Send
directly" path, but misses the path the refinery actually uses in
production:

    // internal/refinery/engineer.go
    mailCmd := exec.Command("gt", "mail", "send", "mayor/", "-s",
        mailSubject, "-m", mailBody, "--permanent")

The refinery spawns a fresh `gt` subprocess for every MERGED / REWORK
mail. From that subprocess's perspective, `os.Args[0]` is `gt`, not
`*.test`. My guard doesn't fire. `gt mail send` runs normally and
pushes mail to the mayor.

Trigger in the Telegraph v1 dogfood: `internal/refinery/batch_test.go`
exercises `engineer.doMergePR()` with fixtures `mr-a` / `feature-a`
/ `test-rig` and `mr-b` / `feature-b` / `test-rig` etc. When a
polecat runs `go test ./...` in a worktree that includes the refinery
package, those tests execute the production code path, which spawns
`gt mail send` subprocesses carrying the fixture values. The mayor
sees real mail with the exact fixture placeholders — 10+ mails in
2 min on the mayor's escalation at 16:38, all the mr-*/feature-*/
test-rig shapes from `batch_test.go:232-306`.

**So the Layer 1 diagnosis was right AND wrong at once**: the `go
test` binary is the root cause, but the damage path is test →
production subprocess, not test → direct Send. My guard checks the
wrong process.

Fix direction (layered, pick-one or combine):

1. **Seam the refinery's mail exec path behind an interface**
   (cleanest but more code). `engineer.doMergePR` takes a `MailSender`
   interface with a real exec-based impl for production and a
   no-op / memory impl for tests. `batch_test.go` injects the no-op.
   Zero changes to the mail package needed.

2. **Environment-variable subprocess inheritance**
   (quickest, still fail-loud). The mail package's TestMain (or any
   test's TestMain that wants to catch subprocess propagation) sets
   `GT_MAIL_REFUSE_SUBPROC=1` in the process env. `gt mail send`
   checks it at startup and exits with an error. Because child
   processes inherit env by default, this blocks ALL subprocess-
   spawned `gt mail send` calls from any test binary, not just the
   mail package's own. Trade-off: test-author must remember to set
   it — same footgun as the general env-var approach, but scoped
   narrowly to mail, so it's less easy to forget.

3. **Parent-process walk** (defensive, platform-fragile). `gt mail
   send` at startup reads `/proc/<ppid>/cmdline` (Linux) or
   `ps -p <ppid>` (macOS) and refuses if any ancestor looks like a
   `.test` binary. Catches everything automatically with no test-
   author cooperation, but cross-platform robustness is a headache.

Recommended path forward: **land Option 1 first** (proper seam —
right thing architecturally), fall back to Option 2 as a quick
fix if the seam refactor is too large to fit in the current
iteration. Option 3 can be the safety net under Option 2 later.

Also worth noting: this bypass means every refinery test run in
every rig (not just gastown, not just Telegraph) could be
emitting real mail to its mayor. The blast radius is bigger than
"Telegraph v1 is blocked" — any rig that has a polecat running
`go test ./...` with the refinery package in scope is affected.

**Mayor's current ask** (hq-6g5, 16:38 — still open): mayor paused
dispatch pending guidance on (a) whether to nuke furiosa, (b)
whether to dispatch gt-eef/gt-ipc, (c) nuke stale nux worktree,
(d) mail overseer directly about this hole.

### G11 — `gt done`'s no-merge+pr path creates PRs outside the refinery workflow, so no review loop fires

Observed on PR #10 (`feat(telegraph): per-provider config + secret
surface (gt-8vr)`): the PR got created, real work landed on the
branch, but **no `augment review` comment was ever posted**. Only
Gemini Code Assist reviewed it — and only because Gemini runs on
every new PR at the repo level, independent of any trigger.

Root cause: `internal/cmd/done.go:819` has a shortcut path that
fires when `attachmentFields.NoMerge == true` AND the rig is on
`merge_strategy = "pr"`:

    ghCmd := exec.CommandContext(context.Background(), "gh", "pr", "create",
        "--base", defaultBranch, "--head", branch,
        "--title", prTitle, "--body", prBody,
    )

That path is invoked inside `gt done` running in the polecat's
worktree. It bypasses:

1. **The refinery** — no MR bead is created, so the refinery's
   patrol never picks up this work. Verified: `bd list --label=gt:merge-request --all` returned zero results for gt-8vr.
2. **The tap-guard** — the `gh pr create` call is a subprocess of
   `gt`, not an agent's Bash tool, so `tap guard pr-workflow`
   (matcher `Bash(gh pr create*)`) doesn't fire.
3. **The `request-review` step** — that step only runs inside the
   refinery formula's `merge-push` branch when `merge_strategy=pr`,
   and only when the refinery created the PR via
   `gt refinery pr create`. For PRs created via this
   no-merge+pr shortcut, the refinery has no handle to request
   review on.

So the invariant "every PR goes through pr-create → wait-ci →
request-review → wait-approval → merge" is only enforced when the
refinery owns the PR. Any path that creates a PR outside the
refinery (done.go's no-merge shortcut here; in principle also any
future shortcut) orphans it from the review/merge pipeline.

Sequence for PR #10:

- Mayor slung gt-8vr to furiosa with `no_merge=true` attachment.
  (The mayor may have set no_merge when interpreting my
  test-pollution pause — unclear from the history, but
  `attachmentFields.NoMerge` was true at `gt done` time, which is
  the only way this shortcut path fires.)
- Furiosa did the work, committed, ran `gt done`.
- `gt done` took the no-merge path, closed the bead, created PR #10
  via `gh pr create` subprocess.
- Refinery patrolled, saw empty queue + no MRs, marked most steps
  SKIP (see G7), did nothing about PR #10.
- Gemini auto-reviewed PR #10 at the repo level (no trigger
  required).
- No `augment review` comment was ever posted. The review loop
  never started.

Fix directions (pick one or combine):

1. **Make the no-merge+pr path request review itself.** After
   `gh pr create` succeeds in `done.go:819`, immediately `gh pr
   comment <N> --body "augment review"` (or call the same
   `gt refinery pr request-review` subcommand the refinery uses).
   Smallest change, keeps the shortcut but closes the orphaning.
2. **Always create an MR bead, even on the no-merge path.** Instead
   of force-closing the bead and shell-forking to `gh pr create`,
   create an MR bead that tells the refinery "this PR already
   exists, pick up the review+merge from here". The refinery's
   `merge-push` step's pr branch then gets an optional "PR already
   created" short-circuit that skips `pr create` and starts at
   `request-review`. Keeps the refinery as the single owner of
   PR-workflow state.
3. **Forbid the shortcut entirely** under merge_strategy=pr and
   always route through MR bead + refinery. Simplest model
   invariant but requires rethinking what no_merge means in
   pr-mode (presumably: still no MR → no merge, but also no PR
   without refinery involvement).

Recommended: **Option 1 as a fast fix**, **Option 2 as the proper
architectural resolution**. Option 3 is philosophically cleaner but
the most disruptive to existing rigs that rely on no-merge
behavior.

Also worth noting: this is the SECOND time my own B1/B2/B3
fix stack has had a silent gap only real-world dogfood exposes.
Pattern: the "pr workflow" is actually TWO workflows (refinery-
owned vs polecat-owned-with-shortcut) and the review/merge
invariants are written for the refinery-owned one. Every
polecat-owned shortcut needs its own invariant coverage or a
redirect back into the refinery-owned path.

### G12 — `pr-request-review` uses `gh pr edit --add-reviewer`, which never triggers Augment; refinery also holds off dispatching the review-fix polecat on existing threads while "waiting for augment"

Observed on PR #10 after the G11 fix landed and `gt-idw` was
backfilled. The refinery picked up the MR from the queue, ran
`gt refinery pr create` (idempotent — returned existing PR #10),
skipped `wait-ci` (no checks configured), and executed
`gt refinery pr request-review 10 --user augment`. That emitted
"✓ Review requested on PR #10 from augment" — but no `augment
review` comment was posted, no review from augment followed, and
`gh api repos/Phaware-Global/gastown/pulls/10/requested_reviewers`
returned `{"users":[],"teams":[]}`.

**Sub-issue a: request-review doesn't trigger Augment.**
`internal/git/git.go:GhPrRequestReview` implements request-review
as `gh pr edit <N> --add-reviewer augment`. That is the
GitHub-native "request review" metadata action — which either
(i) silently succeeds as a no-op because `augment` isn't a valid
GitHub user on this repo (the actual bot is a GitHub App whose
comments appear under `augmentcode[bot]` or `augmentcode-free[bot]`
depending on the installation, neither of which can be requested as
a reviewer via `--add-reviewer`), or (ii) adds a dismissible
metadata marker that nothing responds to. Augment Code's trigger is a PR
**comment** whose body is exactly `augment review`. That's what
the review loop used in this dogfood's earlier PRs (#6-#9, #11)
to trigger augment — a hand-typed comment, not a reviewer
request. The refinery code path for `merge_strategy=pr` was
written to match the GitHub metadata model, not augment's
comment-trigger model, so the automated loop never actually
invokes augment even though the formula ran the right step.

Manual unstick (this session): `gh pr comment 10 --body
"augment review"` — augment responds to that.

**Sub-issue b: refinery withholds review-fix dispatch on
non-augment threads.** After `request-review`, the refinery's
PR.5 step polled `gt refinery pr threads 10` and saw 3 unresolved
threads — all from gemini-code-assist (repo-level auto-review,
not triggered by the workflow). With `review_loop_iter=0` and
`max=3`, the formula invariant is "threads > 0 AND iter < max →
sling a review-fix polecat". The refinery instead filed a patrol
report with language *"waiting for augment's response before
dispatching review-fix polecat"* and set `iter=0` — parking the
MR instead of dispatching.

That's a silent policy the formula doesn't carry. There's
nothing in `mol-refinery-patrol` that says "only augment's
threads count"; the step description says "threads" (any
unresolved thread). The refinery LLM added author-awareness on
its own, presumably reasoning that gemini threads aren't
"officially requested" and dispatching a polecat to address them
is unnecessary work. But the threads ARE unresolved, the PR
CAN'T merge with them open (augment's eventual approval won't
resolve gemini's unresolved threads; the wait-approval step
gates on distinct approvals AND thread resolution), and the
whole point of the review-fix loop is to get threads to zero.

So the cost of the LLM's "wait for augment" heuristic is: with
sub-issue (a) in effect, augment NEVER responds, so the refinery
waits forever, gemini's threads accumulate, the PR stays parked.
Even if (a) is fixed so augment does respond, (b) still wastes a
full patrol cycle (the one before augment's first review
arrives) where the gemini threads could have been addressed in
parallel.

Fix direction:

1. **(a) Change `GhPrRequestReview` to post an `augment review`
   PR comment when the reviewer is `augment`** (the configured
   `PRReviewer` for this rig), and fall back to the
   `--add-reviewer` path only for non-augment reviewers. This
   keeps the interface (`RequestReview(prNumber, reviewers)`)
   but makes the implementation reviewer-aware. A richer
   generalization: `PRProvider` grows a notion of "review
   trigger style" per reviewer — `comment` for augment,
   `request` for human/team reviewers — and the GitHub impl
   dispatches accordingly. Short term, the if-reviewer-is-augment
   special case is enough.

2. **(b) Tighten the formula's PR.5 step description to be
   author-agnostic.** The step today reads
   "If zero unresolved threads: advance to `pr-wait-human`.
   Else if `review_loop_iter >= PRReviewLoopMax`: escalate.
   Else: sling a review-fix polecat". Add an explicit sentence
   like "Threads from ANY author count — gemini's auto-review
   comments, augment's review, human review all trigger the
   review-fix loop. Do not distinguish by author." A
   substring-guard test on the parsed formula step (same
   pattern as the Phase 4 acceptance test described in the
   Implementation Plan) can pin this rule so a future LLM
   interpreting the step can't silently reintroduce the
   author-aware variant.

3. **Integration test for the trigger mechanism.** An end-to-end
   test that provisions a rig with `pr_reviewer=augment`, opens
   a real PR, runs `gt refinery pr request-review`, and asserts
   an `augment review` comment appears on the PR within N
   seconds. This is the cheap test that would have caught (a)
   before it shipped.

Recommended ordering: ship (1) as a one-line fix in
`GhPrRequestReview` so the automated loop works for the current
Telegraph v1 run; ship (2) in the same PR as formula rule
tightening; (3) as a follow-up PR.

This is the THIRD silent gap in my B1/B2/B3/G11 stack that
real-world dogfood has exposed. The pattern is now clear enough
to generalize: **formula steps that embed policy (what counts,
when to wait, which author matters) should either be expressed
as explicit data (`pr_reviewer_trigger_style` config) or
protected by static tests on the parsed formula text**. The
free-form natural-language steps give the LLM too much room to
insert its own policy, and "silent wait forever" is a frequent
failure mode.

### G13 — review-fix polecat replies to threads but never resolves them; review loop cannot converge

Observed on PR #10 iter 1 (polecat furiosa, bead gt-5pk). The
polecat correctly addressed all 3 gemini-code-assist threads:
added `String()`/`GoString()` redaction to `ResolvedProvider`,
added `Validate()` rejection of negative `NudgeWindow`, added
`Validate()` rejection of enabled-but-empty `Events`. Committed
as `419a320`, pushed to `polecat/furiosa-mo6c8x8n`, replied to
each thread. But after `gt done` exited clean, all 4 threads on
PR #10 (3 gemini + 1 new augment inline) were still
`isResolved: false`.

The polecat used `~/.claude/skills/address-pr-comments/scripts/respond-to-thread.mjs`
with `--reply "..."` but without `--resolve <THREAD_ID>`. The
skill supports both (examples at lines 149, 199, 206 of the
skill doc show the `--reply ... --resolve` pattern), but the
dispatch args from the refinery only said *"reply to each
GitHub thread, then run gt done"* — they never instructed the
polecat to resolve.

**User's explicit policy (confirmed during dogfood):**

> The threads should be resolved by the polecat after creating
> fixes, there will never be an auto-resolve and there should
> not be a requirement for help from a human in this review
> loop.

Three facts that compound to make this load-bearing:

1. **No auto-resolve exists.** The gemini-code-assist bot never
   resolves its own threads. Augment doesn't either unless
   explicitly prompted. GitHub doesn't auto-resolve on replies
   or on commit addressing.
2. **The review loop's exit condition is "zero unresolved
   threads"** (PR.5 step: "If zero unresolved threads: advance
   to `pr-wait-human`"). If the polecat never resolves, the
   count never drops, the loop never advances.
3. **No human escalation inside the loop.** PR.5's only
   escape hatch before hitting `PRReviewLoopMax` is polecat
   success; there is no "park for human help with
   resolution". Hitting `PRReviewLoopMax` opens an escalation
   but that's the wrong signal — the CODE is fine, only the
   thread-close bookkeeping isn't done.

Consequence on PR #10 iter 1: furiosa's code was correct, but
the refinery's next patrol saw 3 still-unresolved gemini threads
+ 1 augment thread that arrived mid-work, (correctly) dispatched
iter 2 (nux) against the same bead. Iter 2 then had to redo the
thread-resolution work. In the pessimistic case where every
iteration replies-without-resolving, the loop burns all
`PRReviewLoopMax` iterations, escalates, and lands a PR whose
threads are still open — a confusing state for the human who
gets paged.

**Fix direction (three layers, most-impactful first):**

1. **Refinery dispatch args MUST include "reply AND resolve"
   wording.** Change `mol-refinery-patrol.formula.toml` PR.5's
   sling-args template (or the template fragment that becomes
   the `attached_args`) to explicitly read *"Commit fixes, then
   for each thread: reply AND resolve (use
   `respond-to-thread.mjs --reply ... --resolve <THREAD_ID>`
   or `--commit <hash> <hash> <owner> <repo> <pr> --resolve
   <THREAD_ID>`). Leaving a thread unresolved means the review
   loop cannot converge; there is no human fallback for
   resolution."* This is the load-bearing fix — the refinery
   LLM follows the formula's review-fix template verbatim, and
   today that template doesn't mention resolution.

2. **Address-pr-comments skill doc: bold a top-of-file
   invariant.** Add a prominent "POLECAT REVIEW-FIX
   INVARIANT" section near the top of the skill doc that
   reads *"When invoked by a review-fix polecat dispatch
   (`attached_args` mentions review threads), you MUST
   resolve every thread you address. Replying without
   resolving leaves the review loop unable to converge — the
   refinery has no auto-resolve and no human fallback inside
   the loop."* Some polecats may reach the skill through
   instruction paths that don't route through PR.5's
   dispatch args (future formula changes, reused skill calls);
   this invariant lives at the skill layer so it survives
   formula rewrites.

3. **Refinery PR.5 pre-dispatch sanity check.** Before
   advancing to `pr-wait-human`, the refinery should assert
   that the count of unresolved threads is *zero*, not just
   "the polecat returned OK". If the polecat exited clean but
   unresolved threads remain, that's a policy violation —
   treat it as a failed iter (same as a non-zero exit) and
   re-dispatch with explicit "you MUST resolve" wording. This
   is the safety net that makes (1) and (2) load-bearing: if
   the polecat layer ever regresses on resolution, the
   refinery catches it immediately and re-dispatches.

**Short-term action taken this session:** nudged nux (iter 2)
and the refinery directly. Nux was already mid-work on iter 2;
the nudge adds "also resolve each thread" to its context. The
refinery was nudged with the policy statement above so its
next PR.5 dispatch's sling-args will include the resolve
wording. Both are band-aids; the permanent fix is (1)-(3)
above.

Pattern connection: G13 is the same family as G12b (LLM
drifting from what the formula actually prescribes). The
difference is G12b was "LLM added a policy the formula
doesn't have", G13 is "LLM omitted a policy the formula
*should* have but doesn't". Both argue for (a) tightening
formula wording and (b) static tests that pin the formula
text so future rewrites can't silently weaken the
contract.

### G14 — refinery dispatch args enumerate a paraphrased subset of unresolved threads, silently dropping one per iteration

Observed on PR #10 iter 2 (polecat nux, bead gt-ag4). At
dispatch time, `gt refinery pr threads $PR --unresolved
--json` returned four unresolved threads (the three gemini
threads still carrying `isResolved: false` from iter 1 plus
one freshly-posted augment inline thread on `os.Unsetenv`).
The refinery's PR.5 dispatch step took that JSON and
composed a narrative `--args` payload that enumerated only
three of the four:

    Three unresolved threads on PR #10 (polecat/furiosa-mo6c8x8n):
      1. [AUGMENT] config_test.go:218 and :268 — os.Unsetenv in t.Parallel()…
      2. [GEMINI] ResolvedProvider.String() redaction — ALREADY FIXED in 419a320
      3. [GEMINI] Empty events list validation — ALREADY FIXED in 419a320

The fourth gemini thread (`NudgeWindow` validation) was
ALSO already fixed in 419a320 but was omitted from the
dispatch args entirely. Nux processed the three it was
handed, replied-and-resolved each, ran `gt done`, and
exited clean. The refinery's next patrol re-polled and
found one still-unresolved thread — the dropped one — so
it dispatched iter 3 against the same bead.

That's one full iteration wasted. In the pessimistic case
where every iter drops one thread, the loop chews through
all `PRReviewLoopMax` iterations and escalates a PR whose
code has been correct since iter 1, with only
thread-resolution bookkeeping outstanding on a thread the
polecat was never told about.

Root cause: the formula text used to read

    --args "Address these PR threads:
    $THREADS" \

with `$THREADS` being the full JSON. The refinery LLM,
when acting on that step, replaced the JSON with its own
prose summary. That's the same class of deviation as G12b
(LLM inserts policy the formula doesn't have) and G13 (LLM
omits policy the formula should have). Here the LLM
substituted narrative for data — an arguably well-meaning
"cleanup" that silently weakened the dispatch contract.

Fix direction:

1. **Mandate verbatim enumeration in the dispatch-args
   template.** The formula's PR.5 step now reads "Do not
   paraphrase it into your own list" and pins `$THREADS`
   explicitly inside the `--args` string, so the LLM has
   no ergonomic path to rewrite the payload. Paired with
   the G13 mandate to pass the fresh poll result.

2. **Regression test pins `$THREADS` + "do not paraphrase"
   markers.** Same mechanism as G12b/G13 protections —
   `TestRefineryPatrolReviewLoopEnforcesResolveAndEnumeration`
   asserts both markers are present in the PR-mode branch
   of the merge-push step. A future "cleanup" PR that
   drops either marker fails the test.

3. **(Follow-up)** Once the formula is stable across a few
   real runs, consider adding a sanity check inside PR.5:
   after dispatching, re-poll threads and compare the
   count the polecat's task description received (parsed
   out of the bead) against the fresh count. If they
   differ, log a warning — the next iter will pick up the
   missing threads, but the warning catches the drift for
   review.

Pattern connection: G14 is the third variant of the same
underlying issue G12b and G13 point at — the refinery
LLM treating the formula as a flexible suggestion rather
than an invariant contract. The more the formula embeds
policy (what counts, when to wait, what data to pass
through), the more mechanism we need to pin that policy
against silent rewrites. Free-form prose loses; explicit
text + static tests + runtime sanity checks together hold
the line.

### G15 — `gt down` purges embedded Dolt; restart with a newer binary then hits schema drift that blocks patrols

Observed when restarting gastown on 2026-04-20 after the
stack merge (PR #12–#15). The stop sequence I ran earlier
was `cd ~/gt && gt down`, which terminated services *and*
executed `Beads dolt dirs: removed 1` on the embedded
Dolt directory. Intended behavior per the `gt down`
output, but load-bearing data went with it:

- `gt-idw` (the MR bead tracking PR #10, backfilled
  manually during the G11 session) was gone on restart.
  `bd list --all --label gt:merge-request` returned "No
  issues found" in the rig DB. Without the MR bead, the
  refinery's `gt mq list gastown` is empty; PR #10 never
  re-enters the queue; the review/merge path has no
  handle to pick it up.
- Mayor's post-restart hook queries began failing with
  `column started_at could not be found in any table in
  scope`, escalated as hq-s2l: *"bd list --json
  --status=hooked returns error. Hook cannot be read.
  Refinery patrol blocked."*. Mayor then nudged the
  refinery: *"Hook query failure is schema drift in
  gastown Dolt DB (missing started_at column), not
  transient. Hold patrol. Investigating infra fix."* The
  refinery is parked on that instruction until the schema
  drift is fixed.

Two distinct problems surfaced by one `gt down`:

1. **Data-loss on stop.** `gt down` purges embedded Dolt
   directories for rigs that appear to be "only" holding
   ephemeral state, but merge-request beads (created by
   `gt done`'s handoff or backfilled manually) live in
   that same Dolt. When those beads disappear between
   `gt down` and `gt up`, the refinery loses its map of
   in-flight PRs. The workflow has no recovery path to
   re-synthesize gt-idw from origin state — it would
   have to be re-backfilled by hand.

2. **Schema drift on restart with newer binary.** The gt
   binary from this stack (v1.0.0-99-gc3aac53b) expects
   a Dolt schema where the hooks/wisps table has a
   `started_at` column. The existing Dolt DB doesn't
   carry that column. The mismatch surfaces on the very
   first `bd list --json --status=hooked` query that the
   mayor/refinery runs. No automatic migration fires on
   binary-upgrade, and the older DB on disk survived the
   `Beads dolt dirs: removed 1` (evidently only the
   *removed* rig dolt dir was purged; another subsystem
   carries the schema I'm hitting).

Fix direction (each standalone; this section lives as a
follow-up for whoever picks up the infra side):

1. **Make `gt down` conservative about Dolt purges.**
   Before removing a `.beads/embeddeddolt` directory,
   snapshot any `gt:merge-request`-labeled beads whose
   corresponding branch still exists on origin and
   which have `review_pr` set — those are in-flight PRs
   the refinery owns. Either refuse to remove them,
   exit non-zero with a prompt, or export them to a
   JSON that a subsequent `gt up` auto-imports.

2. **Schema migration on binary upgrade.** `gt up` /
   service start should detect schema-version drift
   between the binary's expected schema and the
   on-disk Dolt schema and run migrations before the
   first query fires. If migration isn't safe (data
   loss risk), exit loud with instructions rather than
   letting the mayor/refinery hit failing queries in a
   loop.

3. **Refinery self-recovery from missing MR beads.**
   Orthogonal to G15 but highlighted by it: when the
   refinery finds a polecat branch on origin with an
   open PR but no MR bead, the formula today has an
   `orphan-branch-check` step that escalates. Under
   merge_strategy=pr, that escalation could
   alternatively *synthesize* an MR bead (same content
   as G11's `handoffPRToRefinery` backfill) rather
   than requiring manual backfill. Not load-bearing;
   quality-of-life.

Status for the Telegraph v1 dogfood: PR #10's four review
threads are all resolved as of 2026-04-20 (the NudgeWindow
thread `58Dm_p` closed in the time between `gt down` and
`gt up` — possibly by a post-hoc resolve that hit before
the Dolt purge; exact attribution unclear). The PR itself
is open, approvable, and mergeable; it just needs a human
(kevinpjones) to review and merge because the automated
escalation path from refinery → mayor mail requires the
patrol cycle to fire, which schema drift is blocking. The
workflow fixes from this stack (G12a / G12b / G13 / G14
plus the memory-write guard and findMRForBranch robust
dedup) are all in the binary on disk at
`~/.local/bin/gt`; they'll exercise against the next MR
the refinery processes once schema drift is resolved.

Binary-level verification seen this restart:
`gt refinery pr request-review 10 --user augmentcode` was
called by the patrol — the new GhPrRequestReview code
path executed, though the refinery LLM substituted
`augmentcode` for `augment` in the args, sidestepping the
case-insensitive "augment" match. So the code is live but
only protects the name the formula / rig config passes
verbatim. That's arguably a sub-issue worth a follow-up
(expand match to also accept `augmentcode` case-
insensitively, and/or pin the rig-config value through
to the subcommand without LLM reshaping).
