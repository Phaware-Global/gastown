# PR Merge Strategy: Known Issues and GitHub Context

## Summary

The `merge_strategy=pr` feature is partially implemented. The Go engineer code
works correctly, but the formula/template/guard layers are not wired up to
support it end-to-end. Multiple community members have filed issues about this.
The feature was designed for repos with GitHub branch protection rulesets that
require PRs for main — a common enterprise setup.

## GitHub Issues (steveyegge/gastown)

### #3601 — "Refinery: support merge_via: github-pr mode for PR-based merge flow"
- Filed by: vbtcl (2026-04-11)
- Describes the exact problem we hit: refinery pushes directly to main, which is
  blocked by GitHub rulesets requiring PRs
- Proposes `merge_via: github-pr` config (essentially what `merge_strategy=pr` is)
- Notes that without this, "the Refinery is running but doing nothing — all merge
  coordination is manual"
- Affects multiple rigs across the reporter's deployment

### #3588 — "Polecats/refinery unable to reliably create and merge PRs"
- Filed by: vbtcl (2026-04-10)
- Documents three failure modes:
  1. Refinery consolidation stalls (dirty worktree, no PR created)
  2. Polecats mark done without committing/pushing
  3. PRs get stuck in GitHub merge queues without recovery
- Impact: "Mayor ends up doing manual git operations that polecats/refinery
  should handle autonomously"

### #3602 — "Polecats cannot work on existing PR branches — always create duplicates"
- Filed by: vbtcl (2026-04-11)
- When re-slinging a bead with an existing PR, the polecat creates a new branch
  and duplicate PR instead of checking out the existing one
- Directly relevant to our review-fix polecat design: the polecat dispatched to
  address PR comments needs to check out the existing PR branch
- Proposes `--pr <number>` or `--branch <name>` flag on `gt sling`

### #3198 — "Refinery should not close upstream GitHub PRs or delete branches with open PRs"
- Filed by: jholm117 (2026-03-23)
- Refinery deletes branches with open PRs, causing GitHub to auto-close them
- The `gas-fk4` fix (commit 1f54e082) partially addresses this by guarding
  branch deletion when an open PR exists

### #3320 — "gt sling does not stamp MergeStrategy or ConvoyID on work bead"
- Filed by: alinsim (2026-03-26)
- `gt sling --merge=local` creates the convoy correctly but never stamps
  `merge_strategy` on the work bead
- `gt done` can't find the strategy, falls through to default MR behavior
- All infrastructure exists (struct fields, parse/format, store/load) — the
  actual assignment in sling.go is just missing

### #3406 — "feat(refinery): configurable pr_requirements in MergeQueueConfig"
- Filed by: azanar (2026-03-29)
- Proposes `pr_requirements` map for fine-grained review control
- Implementation available on a fork branch

## Implementation History (git log)

| Commit | Description |
|--------|-------------|
| `207f1a5c` | Initial merge_strategy config added to refinery |
| `07a89fcf` | merge_strategy added to patrol formula vars |
| `73360130` | Fix: inject merge_strategy from rig settings into formula vars |
| `6a120e00` | Fix: pass rig-level merge_strategy to refinery patrol formula |
| `a0b7582c` | PR #3498: `doMergePR()` — Go engineer code for PR merge via `gh pr merge` |
| `1f54e082` | gas-fk4: Guard PR branch deletion + review approval check |
| `5fd8db81` | Fix: PR mode must wait for CI and merge before sending MERGED |

## What Works

- `MergeQueueConfig` has `merge_strategy` and `require_review` fields
- Formula vars propagate `merge_strategy` to patrol steps
- `doMergePR()` in engineer.go correctly finds PR, checks approval, merges
- Formula step descriptions include PR workflow instructions (with overlay)
- gas-fk4 guards against premature branch deletion with open PRs

## What Doesn't Work

1. **PR guard blocks refinery** — `gt tap guard pr-workflow` blocks `gh pr create`
   from any agent context including the refinery. Never updated for merge_strategy=pr.
   (See: pr-guard-merge-strategy-mismatch.md)

2. **Template summary contradicts overlay** — Hardcoded quick-reference in
   refinery.md.tmpl always describes direct merge. Agents follow summary over
   detailed step. (See: refinery-template-contradicts-overlay.md)

3. **`doMergePR()` expects PR to already exist** — The Go code calls
   `FindPRNumber()` but never creates. The formula tells the agent to create it
   via `gh pr create`, but the guard blocks it (gap 1) and the template tells
   the agent to direct-merge instead (gap 2).

4. **Polecats can't work on existing PR branches** (#3602) — When dispatching
   review-fix tasks, polecats create new branches instead of checking out the
   PR branch. No `--pr` or `--branch` flag on `gt sling`.

5. **merge_strategy not stamped on work beads** (#3320) — `gt sling` doesn't
   propagate merge_strategy to the bead, so `gt done` can't find it.

## Conclusion

This is not just our issue. Multiple users have hit the same gaps. The Go
engineer layer is solid, but the agent-facing layer (guards, templates, formula
execution, sling flags) was never completed for the PR workflow. A fork fixing
items 1-3 above would make merge_strategy=pr functional. Items 4-5 are
additional improvements needed for the full review-loop workflow we designed.
