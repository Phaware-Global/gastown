+++
name = "closed-bead-open-pr"
description = "Detect review-fix beads closed while their linked PR is still open (possible false closure)"
version = 1

[gate]
# HELD manual 2026-08-10 by deacon: mayor's 8-of-8 sweep (hq-wisp-csht0) that
# motivated this plugin was retracted (hq-wisp-6eww0) - most round-scoped
# review-fix beads closing while their PR stays open is NORMAL, not a
# defect. This plugin's v1 run.sh does not distinguish round-scoped beads
# and fired noisy findings/mail before the correction was read. DO NOT
# re-enable (cooldown) until run.sh is redesigned per mayor's narrower ask:
# flag only non-round beads, or beads whose PR has had no new commits since
# closure. See run.sh for the outdated v1 design - needs a rewrite, not just
# a gate flip.
type = "manual"

[tracking]
labels = ["plugin:closed-bead-open-pr", "category:integrity"]
digest = true

[execution]
timeout = "3m"
notify_on_failure = true
severity = "high"
+++

# Closed Bead / Open PR Sentinel

Detects the "closed bead + open PR" pattern that let review-fix work silently
disappear on 2026-08-09/10: a review-fix bead (title mentions `PR #<N>`) gets
closed by `gt done` or similar, but the PR it targets is still open — often
with unresolved review threads. bd/`bd ready` then reads clean while the PR
sits abandoned. Sweep that night found 8 of 8 dispatched review-fix beads hit
this, 6 with real outstanding work.

**DETECT ONLY. Never reopen, never comment on the PR, never dispatch.**
Reopening decisions require human/mayor judgment — some closures are
deliberately superseded by a successor bead scoping the same PR, and
auto-reopening would create duplicate ownership of the same threads (the
dispatch-storm shape). This plugin's only job is to make the pattern visible.

## Run

All logic lives in `run.sh` (enumerate rigs with a GitHub remote, find closed
beads from the last 48h whose title mentions `PR #<N>`, check each PR's state,
report findings once per bead). This file intentionally carries no executable
copy of it. The gate is held `manual`; see the frontmatter comment.

```bash
cd <plugin dir> && bash run.sh
```

Run the command exactly as shown and report the output.
