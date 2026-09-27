+++
name = "orphan-pr-sentinel"
description = "Detect open PRs with no open bead owning them (unresolved threads or failing CI, nobody tracking it)"
version = 1

[gate]
type = "cooldown"
duration = "1h"

[tracking]
labels = ["plugin:orphan-pr-sentinel", "category:integrity"]
digest = true

[execution]
timeout = "3m"
notify_on_failure = true
severity = "high"
+++

# Orphan PR Sentinel

v2 of `closed-bead-open-pr` (retired, see that plugin's `plugin.md` — gate
held to `manual`, kept for history). That v1 flagged every closed review-fix
bead whose PR stayed open, which turned out to be mostly noise: round-scoped
review-fix beads closing while their PR waits for the next review round is
normal, correct behaviour, not a defect (mayor's correction, hq-wisp-6eww0,
2026-08-10).

The real gap, found by inverting the check by hand (hq-wisp-rvpxe): a PR that
**no open bead references at all**. Round closures never trigger this as
long as one bead somewhere still owns the PR — low-noise by construction.
First run found gastown PR #185 (session-bricking HIGH regression, zero
owning beads) minutes before mayor filed gt-3zow for it by hand.

**DETECT ONLY.** Never files a bead, never comments on the PR, never
dispatches. Filing/claiming an orphan PR is a scope/ownership decision for a
human or the rig's witness/refinery, not this plugin.

Full logic lives in `run.sh` (this plugin has no separate plugin.md bash
blocks — see `run.sh` directly, same convention as `stuck-agent-dog`).
`run.sh` accepts `DRY_RUN=1` to print findings without sending mail; always
dry-run after editing the script before letting the cooldown gate fire it.

## What it checks, per rig with a GitHub remote

1. List open PRs via `gh pr list`.
2. For each, search open beads (title or description) for `PR #<N>`. If any
   open bead references it, skip — it's owned, whether or not previous
   round-beads for it have closed.
3. If no open bead owns it, check unresolved review-thread count
   (`gt refinery pr threads`) and CI status. Flag only if there's unresolved
   work or a failing check — a quiet, clean, unowned PR isn't urgent.
4. Report findings once per PR (not per historical round-bead) to mayor as a
   table, and to the rig's witness individually for local visibility.

## Known limitation

Ownership detection is a text match on `PR #<N>` in bead title/description.
A bead that references the PR only by branch name, or a different citation
style, would not be found and could produce a false orphan flag. Acceptable
for a detect-only visibility tool — a human dismisses false positives same
as any other patrol finding — but worth knowing if a flagged PR turns out to
actually be owned.
