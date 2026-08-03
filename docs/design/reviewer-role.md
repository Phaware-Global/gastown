# Reviewer: Rig-Level Local Code Review Role

**Status:** In progress (P23-2376) — Phase 1 (role plumbing) + Phase 2 review
posting (`gt reviewer post`/`checkout`/`perspectives`, `PRProvider.SubmitReview`,
priority-badge parser widening) + the per-rig config surface (`review` block,
`reviewer_local`, `reviewer_token_env`) landed. Remaining: Phase 2 dispatch
(`gt reviewer request`/`done` + reviewer-worktree provisioning + session
lifecycle) and Phase 3 (refinery patrol wiring) landed; deterministic
perspective-prompt generation + parallel-subagent execution (`gt reviewer
prompt`/`consolidate`) landed. Remaining: Phase 4 (crew), Phase 5 (Telegraph).
**Created:** 2026-06-10
**Replaces:** Augment (GitHub-org review app) in the refinery PR review loop
**Cross-references:** [refinery-pr-workflow.md](refinery-pr-workflow.md),
[dog-execution-model.md](dog-execution-model.md), [telegraph.md](telegraph.md),
[../agent-provider-integration.md](../agent-provider-integration.md)

## Overview

The Reviewer is a rig-level agent role that performs AI code review on PRs,
replacing the externally-hosted Augment app. It is dispatched on demand by the
refinery when a PR is ready for review, checks out the PR branch in its own
worktree, reviews the diff from a configurable set of perspectives
(adversarial, security, language/framework-specific, …) using codegraph for
call-graph-aware context, and posts its findings as a GitHub PR review with
inline comment threads under its own GitHub identity.

The design's central constraint: **the refinery review loop does not change.**
Augment's integration contract with the refinery is already narrow and fully
abstracted behind rig config — a trigger, a GitHub review by a configured
login, and unresolved-thread polling. The Reviewer satisfies the same contract
from inside the town, so `await-review`, review-fix polecat dispatch, thread
resolution, and the human-approval merge gate all continue working unmodified.

### Why a role, not a service

Per the execution-model principle in
[dog-execution-model.md](dog-execution-model.md): deterministic,
reliability-critical work belongs in imperative Go; work where **agent
intelligence adds value** belongs in agent sessions. Code review is the
canonical agent-judgment workload. All the deterministic plumbing around it
(PR creation, CI gating, thread polling, fix dispatch, merge gating) already
exists in `internal/cmd/refinery_pr*.go` and `internal/refinery/`, and the
inbound-transport problem that forced Augment to be webhook-driven disappears
entirely: the review request originates *inside* the town, from the refinery,
which already knows the moment a PR is ready.

An independent service (LangChain or otherwise) would duplicate the town's
session management, provider abstraction, dispatch, supervision, and audit
trail — and would sit outside `gt session peek`, beads, and tap-guard
observability. Rejected.

## Goals

1. A rig opts in by setting `merge_queue.pr_reviewer` to the Reviewer's GitHub
   login; no refinery code changes are required for the review loop itself.
2. Review perspectives are configurable per rig as plain markdown prompt
   files; operators can add/remove/edit perspectives without touching Go.
3. Model and provider are configurable through the existing role
   `start_command` override and `agents.json` presets (Claude, Gemini, Codex,
   Cursor, AMP, OpenCode, Copilot, …) — no new provider plumbing.
4. Reviews are call-graph-aware: the Reviewer uses codegraph
   (callers/callees/impact/explore) to evaluate blast radius and convention
   conformance, not just the diff text.
5. Findings flow into the existing review-fix loop: inline threads with
   priority markers that `parseThreadPriority` already understands.
6. The Reviewer **submits a real verdict** (APPROVE, REQUEST_CHANGES, or
   COMMENT) but **never merges**. Its APPROVE does not satisfy the named
   `pr_approver` gate — config validation rejects `pr_approver == pr_reviewer`,
   so the two are always different identities. It **does** count toward the
   `pr_required_approvals` count gate (`GhPrApprovalCount` folds every distinct
   approving login with no bot exclusion) and toward any branch-protection
   approval rule. Rigs must size those gates knowing one approval may come from
   the Reviewer. See "Approval semantics" below.
7. Crew-authored PRs keep automated review coverage after Augment is
   removed: crew requests the same Reviewer through a standalone (no-MR)
   request mode, and the Reviewer is the **only** agent in the town that
   posts GitHub reviews.

## Non-Goals (v1)

- Reviewing PRs on repos that are not registered rigs.
- Comment-triggered reviews of human-authored PRs (Phase 5, via Telegraph).
- Multi-*session* perspective review (one ephemeral agent session per
  perspective). v1 runs perspectives in parallel as **subagents within the one
  reviewer session** (`gt reviewer prompt` per lens → parallel subagents → `gt
  reviewer consolidate`); a multi-session variant remains a possible later
  optimization but is not required for parallelism.
- Replacing the human approver, the count gate, or any merge-gate semantics.
- Bitbucket. The review loop's SHA-scoped engagement check (G37) is
  GitHub-only today; the Reviewer follows the same scoping.

## The Contract Being Preserved

What the refinery observes of "a reviewer" today (all in
`internal/cmd/refinery_pr.go` and `internal/refinery/await_review.go`):

| Contract point | Mechanism | Config |
|---|---|---|
| Wake the reviewer | With `reviewer_local`, the in-town Reviewer is dispatched directly (no PR comment); otherwise an optional external-bot trigger comment, body from `pr_trigger_comment` (default empty — no comment posted) | `merge_queue.reviewer_local` / `merge_queue.pr_trigger_comment` |
| Recognize engagement | A PR review by login `pr_reviewer`, case-insensitive exact match, SHA-scoped to current PR HEAD (G37) | `merge_queue.pr_reviewer` |
| Findings | Inline review comment threads; priority parsed from `![high\|medium\|low](…priority.svg)` shields (`internal/refinery/threads.go:121`) | — |
| Pacing | min-wait then poll within timeout; exit codes 0/1/2/3/4 from `await-review` | `pr_review_wait` (5m), `pr_review_timeout` (30m) |
| Fix loop | Unresolved threads → review-fix polecat dispatch, bounded by `pr_review_loop_max` | `merge_queue.pr_review_loop_max` |

The Reviewer changes only the *wake* mechanism (direct dispatch instead of a
trigger comment) and the *identity* behind the review. Everything else is
untouched.

## Architecture

### Actors and flow

```
Polecat            Witness          Refinery                Reviewer              GitHub
  │                   │                │                       │                     │
  ├─ commit+push ────▶│                │                       │                     │
  │                   ├─ MERGE_READY ─▶│                       │                     │
  │                   │                ├─ rebase, push, PR ────┼────────────────────▶│ PR#N
  │                   │                ├─ wait CI ─────────────┼────────────────────▶│
  │                   │                │                       │   ◀── CI green ─────│
  │                   │                ├─ gt reviewer request ─▶ (bead + session)    │
  │                   │                ├─ await-review          ├─ checkout PR branch│
  │                   │                │   --no-trigger (poll)  ├─ codegraph context │
  │                   │                │                       ├─ N perspective     │
  │                   │                │                       │   subagents ∥ then  │
  │                   │                │                       │   consolidate       │
  │                   │                │                       ├─ gh: post review ──▶│ threads
  │                   │                │                       ├─ gt reviewer done   │
  │                   │                │   ◀── review by pr_reviewer login ──────────│
  │                   │                ├─ threads unresolved? ─┼────────────────────▶│
  │  ◀─ review-fix sling ──────────────┤ (existing loop, unchanged)                  │
  │                   │                ├─ wait-approval (HUMAN) ─────────────────────▶│
  │                   │                ├─ squash-merge ────────┼────────────────────▶│
```

Two changes versus today, both at the "request review" step:

1. `gt refinery pr request-review` (or the patrol formula step that wraps it)
   gains a *local dispatch* path: instead of posting `pr_trigger_comment` and
   hoping an external bot notices, it runs `gt reviewer request <pr> --mr <id>`.
2. `await-review` runs with `--no-trigger` (already supported,
   `refinery_pr.go:215`) so it polls without posting a comment. Its exit-code
   contract, SHA-scoping, persistence on the MR bead, and escalation behavior
   are untouched.

### Why dispatch-by-refinery, not webhook or patrol

- **Not webhook**: there is no external event. The request originates in the
  refinery's own patrol step. Telegraph stays out of this path entirely
  (see [telegraph.md](telegraph.md) — Telegraph is Mayor-addressed inbound
  transport, not intra-town dispatch).
- **Not daemon-ensured persistent session**: review demand is bursty.
  A persistent idle Claude session per rig burns tokens and context for
  nothing (the same reasoning that keeps Boot ephemeral —
  [dog-infrastructure.md](dog-infrastructure.md)). The Reviewer is
  spawn-on-demand, one session per review request, self-terminating.

### Session lifecycle

Modeled on the dog dispatch pattern (`internal/daemon/handler.go:
dispatchPlugins`), but rig-scoped and refinery-initiated:

1. `gt reviewer request <pr> [--mr <mr-bead-id>]` —
   creates a `review-request` bead carrying PR number, head SHA, branch,
   round number, origin (`refinery` with `--mr`, `crew` without — see Crew
   PRs section), and the MR bead ID when present; sends it as mail to
   `<rig>/reviewer`; starts the tmux session (`{prefix}-{rig}-reviewer`)
   with the role's `start_command`. If a reviewer session is already
   running for the rig, the request queues in its mailbox (mail is the work
   queue; sessions drain it).
2. The session primes via `gt prime` (role template below), finds the request
   on its hook/mailbox, and executes the review formula.
3. On completion the Reviewer posts the GitHub review, writes a summary to
   the review bead, closes it, and runs `gt reviewer done` — the analog of
   `gt dog done`: clears state, auto-terminates the session.
4. Staleness backstop: the reviewer heartbeat (below) makes review progress
   observable, and the daemon's reviewer reaper nudges and then kills a
   session that stops progressing or exceeds its absolute runtime cap.
   `await-review`'s own timeout (exit 3 → escalation) remains the
   merge-side safety net: a hung Reviewer never blocks a merge silently.

   > **History.** This step originally asserted that "the existing daemon
   > stale-session machinery pattern (cf. `detectStaleWorkingDogs`) applies."
   > It never did. Every daemon patrol enumerates its targets from either a
   > persistent registry (witness/refinery) or a worktree directory plus an
   > agent bead (polecats/dogs); the Reviewer has neither, so it was invisible
   > to all of them and nothing ever reaped a hung reviewer. The heartbeat and
   > the reaper exist to make the claim true.

### State vs telemetry

The Reviewer is ZFC: it keeps **no lifecycle state file**. The tmux session is
the source of truth for "is there a reviewer", and mail is the work queue. That
rule is what makes the role crash-safe — there is no state to reconcile, so a
killed session leaves nothing stale behind and a re-dispatch is always clean.

The heartbeat at `<rig>/reviewer/heartbeat.json` does **not** violate that rule,
because it is a different category of file. The distinction that matters is
*who reads it and what changes if it's gone*:

| | Lifecycle state (forbidden) | Telemetry (this heartbeat) |
|---|---|---|
| Read by | the role itself, to decide what to do next | supervisors only — the reaper, `gt reviewer status`, an operator |
| If deleted mid-review | behavior changes; the role is confused or wedged | nothing changes; the review proceeds identically |
| If it disagrees with reality | reality is corrupted — two sources of truth | reality wins; the file is re-derived on the next phase |
| Authority | authoritative | strictly derived and disposable |

No `gt reviewer` command ever reads the heartbeat to decide what to do. It is
written forward-only, never branched on, and deleting it at any moment changes
nothing about the review in flight — it only blinds the supervisor. That is the
test: **state you must not lose, versus telemetry you may always throw away.**

The precedent is `deacon/heartbeat.json`, which coexists with the Deacon's own
ZFC design for exactly this reason.

Two fields carry the load, and they answer different questions:

- **`timestamp`** — when the current phase was entered. Its age is a *progress*
  signal. Note that the perspective subagents run entirely between the last
  `gt reviewer prompt` and `gt reviewer consolidate`, so a heartbeat parked at
  `prompt` is the normal shape of a review in flight, not by itself evidence of
  a wedge.
- **`started_at`** — when the review was dispatched, preserved across every
  phase transition. `elapsed` measures total review wall time. Only the
  dispatcher sets it, so an in-session phase touch cannot reseed the clock, and
  it is reset whenever the `(pr, round, sha)` identity changes — round 2 of the
  same PR is a new review with its own budget, not a continuation of round 1.

  `elapsed` is a **self-reported lower bound, not a tamper-proof one.** A
  process that deletes or corrupts the heartbeat gets a fresh clock on its next
  touch, and nothing in this file can prevent that. A supervisor needing an
  unforgeable runtime bound must anchor on something the reviewed process does
  not own — the tmux session's start time — and treat `elapsed` as a floor.

The heartbeat is seeded by `gt reviewer request` on the **dispatcher's** side,
before the session exists, so a reviewer that is requested but never starts is
distinguishable from a rig where no review was ever requested. It is cleared by
`gt reviewer done`, so an idle rig never looks stalled. It lives in
`<rig>/reviewer/`, deliberately outside the `<rig>/reviewer/rig/` worktree, so
it never appears in `git status` and cannot be swept by a detached checkout.

Writes are atomic (temp + rename) so the daemon never reads a torn file, and
every write is best-effort: telemetry must never be able to fail the review step
that produced it.

## Role Definition

### `internal/config/roles/reviewer.toml`

```toml
# Reviewer role definition
# Rig-level on-demand code reviewer. Dispatched by the Refinery.

role = "reviewer"
scope = "rig"
nudge = "Check your hook/mail for a review request."
prompt_template = "reviewer.md.tmpl"

[session]
pattern = "gt-{rig}-reviewer"
work_dir = "{town}/{rig}/reviewer"
needs_pre_sync = true
start_command = "exec claude --dangerously-skip-permissions"

[env]
GT_ROLE = "reviewer"
GT_SCOPE = "rig"

[health]
ping_timeout = "30s"
consecutive_failures = 3
kill_cooldown = "5m"
stuck_threshold = "45m"   # reviews are bounded; well past pr_review_timeout
```

Notes:

- `scope = "rig"`: the Reviewer reviews one rig's PRs and lives in that rig's
  directory tree (`<rig>/reviewer/rig` worktree, mirroring
  `<rig>/refinery/rig`).
- Model/provider override is the **existing** mechanism: a town- or rig-level
  role override file (`<town>/roles/reviewer.toml` or
  `<rig>/roles/reviewer.toml`, resolution already implemented in
  `LoadRoleDefinition`, `internal/config/roles.go:141`) can replace
  `start_command` with any `agents.json` preset launch (e.g. a Gemini or
  Codex CLI). No new provider plumbing.
- `needs_pre_sync = true`: the worktree must fetch before checking out the PR
  branch.

### `internal/templates/roles/reviewer.md.tmpl` (structure)

Follows the dog/witness template conventions (wait-for-assignment guard,
propulsion principle, completion protocol):

1. **Identity & guard** — you are the Reviewer for `{{ .RigName }}`; work
   arrives via hook/mail; do NOT freelance, do NOT review unrequested PRs.
2. **Review protocol** — for the PR in the request bead:
   - `git fetch && gt reviewer checkout <pr>` (worktree to the PR head SHA
     recorded in the request — review the SHA you were asked to review; if
     upstream HEAD moved, note it and review the requested SHA anyway, the
     refinery's SHA-scoped gate handles staleness).
   - Build context with codegraph **before** reading the full diff (see
     Codegraph Integration below).
   - On fix rounds (round ≥ 2): the request payload already contains the
     prior rounds' review threads and responses (assembled by Go at
     dispatch). Re-review the **full diff** at the new SHA, but do NOT raise
     new findings on code unchanged since the last reviewed SHA unless a fix
     commit changed its behavior — no relitigation.
   - Generate each enabled perspective's prompt (`gt reviewer prompt`) and run
     the passes **in parallel** as subagents; collect one `PerspectiveResult`
     per perspective.
   - Consolidate (`gt reviewer consolidate`) to deduplicate findings
     deterministically, then post **one** GitHub review via
     `gt reviewer post --pr <n> --findings <json>` — the only sanctioned
     posting path (raw `gh pr review` is tap-guard-blocked).
3. **Output contract** (load-bearing — see next section).
4. **Completion** — write summary to the review bead, `bd close`, notify
   refinery (`gt nudge <rig>/refinery "REVIEW_POSTED: PR #<n>"` — advisory;
   the poll loop is the source of truth), then `gt reviewer done`.
5. **Hard prohibitions** — never `gh pr review --approve`, never
   `gh pr merge`, never resolve review threads, never push to any branch.
   (Enforced by tap-guard, not just prose — see Security.)

## Output Contract

The review the Reviewer posts must satisfy the parsers the refinery already
runs:

1. **Review event**: a PR review (state `COMMENTED`) authored by the login in
   `merge_queue.pr_reviewer`, submitted at-or-after the request's head SHA —
   this is what `AwaitReviewStep` matches (login match is case-insensitive
   exact; G37 SHA-scoping).
2. **Inline threads**: one review comment thread per finding, anchored to
   file/line via the GitHub review API.
3. **Priority shields**: each finding's body **starts** with a neutral badge
   (shields.io-style), e.g.
   `![high](https://img.shields.io/badge/priority-high-red.svg)`.
   `parseThreadPriority` (`internal/refinery/threads.go`) is widened to
   accept `![<priority>]` plus a URL containing `priority` and `.svg`
   (today it requires the contiguous substring `priority.svg`, which only
   the legacy gstatic form satisfies; that form stays accepted for interim
   external bots). This flows priority into the review-fix polecat's
   dispatch payload for free. The post wrapper emits this format — emitter
   and parser share test fixtures.
4. **Finding body shape** (per thread, emitted by the post wrapper):
   ```
   ![high](https://img.shields.io/badge/priority-high-red.svg)
   **[adversarial]** <one-line summary>

   <explanation, with codegraph evidence: callers affected, tests missing, …>

   Suggested fix: <concrete change or diff suggestion block>
   ```
   The `[perspective]` tag attributes the finding and lets review-fix
   polecats and humans see which lens produced it.
5. **Summary comment**: the review's top-level body lists per-perspective
   verdicts and a count of findings by priority, plus the reviewed SHA.
   No finding → explicit "no findings from perspective X", never silence.

## Perspectives

### Configuration

Perspectives are markdown prompt files in the rig:

```
<rig>/settings/review/perspectives/
├── adversarial.md       # try to break it: edge cases, error paths, races
├── security.md          # injection, authz, secrets, unsafe input handling
├── go-idioms.md         # language/framework conventions for this rig
└── simplify.md          # reuse, altitude, dead code (optional)
```

Enablement and order live in the rig settings file alongside `merge_queue`:

```jsonc
"review": {
  "perspectives": ["adversarial", "security", "go-idioms"],
  "max_findings_per_perspective": 8,     // cap noise; overflow summarized
  "fail_silent_perspectives": false      // missing file = config error, not skip
}
```

Resolution: `<rig>/settings/review/perspectives/<name>.md`, falling back to
`<town>/settings/review/perspectives/<name>.md` for town-shared perspectives.
Built-in defaults for `adversarial` and `security` ship embedded (same
`go:embed` pattern as role TOMLs) so a rig with an empty config still gets a
useful review.

### Perspective file format

Plain markdown — a perspective is a prompt, no schema, no TOML. A perspective
file carries **only the lens** ("what to look for"). It does **not** restate how
to review: the shared execution contract (diff/SHA targeting, round handling,
required codegraph tooling, the evidence standard, the output JSON schema) is
centralized in one templated block (`internal/reviewer/prompt/execution.md.tmpl`)
and injected into every generated prompt by `gt reviewer prompt`. So a lens file
is just:

```markdown
# Adversarial lens

You are hostile to this change. Assume it is broken and find out how.
- For every changed function, enumerate callers (codegraph_callers) and ask
  which caller's assumptions the change violates.
- Check error paths and zero-values on every new branch.
- Flag any changed exported symbol with "no covering tests" in codegraph's
  blast radius output.
Your verdict should name the worst unhandled failure you found.
```

### Execution model

The per-perspective *prompts* are generated deterministically, not hand-written
by the agent:

1. `gt reviewer checkout <pr> --sha <sha>` — one detached checkout at the
   reviewed SHA, one warmed codegraph context.
2. `gt reviewer prompt <name> --pr <pr> --sha <sha> --round <r>` per enabled
   perspective — emits the lens + the shared execution contract + the output
   schema. This is the single source of truth a pass executes from.
3. The reviewer launches **one subagent per perspective in parallel**, each
   reviewing strictly from its generated prompt and returning a
   `PerspectiveResult` JSON object (`perspective` + required `verdict` +
   `findings`).
4. `gt reviewer consolidate perspective-*.json` deduplicates findings
   deterministically (by `path:line:title`, highest priority wins, perspective
   tags unioned) and builds a summary with a verdict line for every perspective.
5. `gt reviewer post` posts the single consolidated review.

Determinism is the design point: prompt generation, dedup, and the output
contract are tested Go (`internal/reviewer/prompt.go`, `consolidate.go`); only
the per-lens judgment is left to the subagents. The `review.parallel` config flag
is retained for a possible later multi-*session* variant, but parallelism across
perspectives no longer depends on it — it is the default execution shape.

## Codegraph Integration

The Reviewer's defining advantage over diff-only reviewers (Augment
included): graph-aware context. The rig worktree must be codegraph-indexed
(`codegraph init` becomes part of reviewer worktree setup; the watcher keeps
the index ~1s behind the checkout).

The role template instructs, per review (before any perspective pass):

1. **Blast radius**: for each changed exported symbol →
   `codegraph_callers` / `codegraph_impact`. "What depends on what this PR
   touched" — including callers the diff never shows. Surface "no covering
   tests found" annotations directly to the adversarial pass.
2. **Flow check**: `codegraph_explore` naming symbols that span any changed
   flow, to verify the change against how the code is actually reached
   (dynamic-dispatch hops included).
3. **Convention conformance**: for new implementations of an existing
   interface/pattern, explore sibling implementations so "this doesn't match
   how the other translators do it" findings cite real code.

Session wiring: the reviewer worktree gets a `.mcp.json` (or the role's
settings sync) registering the codegraph MCP server with
`--path <reviewer worktree>`. This is per-worktree config, not new Go code.

## Crew PRs (Second Request Source)

Crew members are the town's other PR authors. In this fork, crew work lands
via branch → `gt commit` → push → `gh pr create` (the `/crew-commit` skill),
and those PRs receive their AI review today from the same org-wide Augment
app the refinery uses. Removing Augment without this section would orphan
crew PRs: refinery PRs keep automated review via the new role, crew PRs
silently lose it.

The fit follows from constraints already in force: crew is forbidden from
posting GitHub reviews (`crew.md.tmpl` — "PR reviews are report-back work…
Do NOT post comments or reviews to GitHub"), and the Reviewer becomes the
only agent that posts them. **Crew is a request source, never a reviewer.**

### Standalone request mode

`gt reviewer request <pr>` works without `--mr`:

- Creates the review-request bead with no MR linkage and `origin=crew`
  (refinery dispatches carry `origin=refinery`). No await-review-facing
  state is written — there is no gate consuming it.
- Everything downstream is identical: same perspectives, same codegraph
  protocol, same `gt reviewer post` wrapper, same priority badges. Round
  tracking and prior-thread context assembly (Resolved Decision #2) key on
  the PR number instead of the MR bead.
- `/crew-commit` gains a step after PR creation: `gt reviewer request <pr#>`.

### Fix-loop asymmetry

| | Refinery PRs | Crew PRs |
|---|---|---|
| Findings consumer | `await-review` exit 2 → review-fix polecat dispatch | The crew member (persistent session, full context) |
| Thread resolution | Authoring polecat (GraphQL mutation) | Authoring crew member — same author-owns-threads rule |
| Re-review | Refinery re-dispatches per round | Crew re-runs `gt reviewer request` after pushing fixes (round ≥ 2, no-relitigation applies) |
| Gate | `await-review` exit codes + human approval | Human judgment only — the review is input, not a blocking loop |

### Queueing

Crew requests share the per-rig reviewer mailbox with refinery requests.
Refinery requests are merge-blocking (30m `pr_review_timeout`); crew
requests are advisory. v1 drains FIFO — acceptable at current volume — but
the `origin` field exists so drain order can prefer refinery requests later
with no schema change.

### What deliberately does not change

- Crew's `gh pr review` prohibition stays, with stronger rationale: review
  posting is now a role with a dedicated identity, output contract, and
  tap-guard enforcement. Crew "reviews" remain report-back analysis for the
  overseer (the pr-sheriff NEEDS-CREW flow) — a different product than the
  Reviewer's contract-formatted threads, and the two coexist.
- Crew members do not serve as the Reviewer: they are human-paired,
  context-laden, and identity-wrong (the engagement gate needs a stable
  dedicated login), and a review dispatch would preempt whatever the human
  was doing with that workspace.
- Pre-PR WIP review stays in-session (`/code-review`); the Reviewer role is
  PR-anchored.

Standalone mode is also most of the machinery Phase 5 (Telegraph-triggered
reviews of external PRs) needs — that phase reduces to the Mayor calling
the same command.

### Instruction convergence plan (crew template vs. crew-commit skill)

The crew role template and this fork's crew workflow currently contradict
each other:

- `internal/templates/roles/crew.md.tmpl` ("No PRs in Maintainer Repos"):
  "NEVER create GitHub PRs — push directly to main instead… PRs are for
  external contributors."
- `.claude/skills/crew-commit/SKILL.md`: "NEVER commit directly to main.
  All crew work goes through branches and pull requests."

The skill reflects this fork's reality (`merge_strategy=pr`, branch
protection, human approval gate); the template carries upstream's
direct-merge assumption. Convergence:

1. **Canonical policy is per-rig, derived from `merge_queue.merge_strategy`**
   — not a global flip. Direct-merge rigs keep upstream's guidance
   (polecats `gt done` → refinery; crew push to main); `pr`-strategy rigs
   require branch + PR via `/crew-commit`.
2. **Make the template strategy-aware.** Replace the unconditional "No PRs
   in Maintainer Repos" section with guidance keyed on the rig's merge
   strategy. The durable mechanism is `gt prime`-time rendering: prime
   already injects rig context, and the rig's `MergeQueueConfig` is
   available where role templates are rendered. (Interim acceptable: a
   static rewrite of the section describing both strategies and telling
   crew to check the rig's `merge_queue.merge_strategy`.)
3. **Update `/crew-commit`** in the same change: add the
   `gt reviewer request <pr#>` step (Phase 4 below) and a pointer to the
   strategy rule so the skill and template cite the same source of truth.
4. **Mark the delta.** The template edit is a deliberate fork divergence
   from upstream; annotate it with a fork-delta comment marker so upstream
   merges don't silently reintroduce the contradiction.
5. **Related cleanup (same sweep, not blocking):** the `pr-sheriff` skill
   still scopes itself to `steveyegge/gastown` — stale upstream coordinates
   for this fork; re-point its repo scope at the fork's repos.

## Identity, Auth, and Security

### GitHub identity

- The Reviewer posts under a dedicated **machine user** (e.g.
  `<org>-reviewer-bot`) with `repo` scope on the rig's repos. The token is
  resolved at dispatch from the env var named by
  `merge_queue.reviewer_token_env` (default `GT_REVIEWER_GITHUB_TOKEN`),
  exported as `GH_TOKEN` into the session — telegraph's secret rule applied
  unchanged: config stores the env var *name*, the value never touches disk
  or logs, and an unset var fails fast at dispatch rather than mid-review.
  Multi-org towns set a different env name per rig; single-org towns use
  the default and one machine user.
- `merge_queue.pr_reviewer` is set to this login. Because the author identity
  (polecat pushes / refinery PR creation) differs from the reviewer identity,
  GitHub accepts the review normally.
- The machine user must NOT be the `pr_approver` and must not have merge
  rights on protected branches. This is **enforced**, not conventional:
  `validateMergeQueueConfig` rejects `pr_approver == pr_reviewer` (case-
  insensitive). Branch protection is the backstop to tap-guard.

### Approval semantics

The Reviewer's APPROVE is a real GitHub approval. Exactly what it does and does
not buy matters, because `VerifyPRApproval` (`internal/refinery/approval.go`)
evaluates **two independent gates** and the Reviewer sits outside only one:

| Gate | Mechanism | Reviewer's APPROVE |
|---|---|---|
| Named approver | `IsPRApprovedBy(pr, cfg.PRApprover)` | **Cannot satisfy it.** Config validation forces `pr_approver != pr_reviewer`, so the login never matches. |
| Count | `CountApprovals(pr) >= pr_required_approvals` | **Counts.** `GhPrApprovalCount` folds every distinct login whose latest terminal state is APPROVED. There is no bot exclusion and no permission filter. |

The same applies to GitHub branch-protection rules requiring N approving
reviews, which `gh pr merge` defers to.

**Consequence for operators.** A rig setting `pr_approver: alice` and
`pr_required_approvals: 2` — intending "alice plus one more human" — will merge
after *one* human approval, because the Reviewer's automatic APPROVE on a clean
pass supplies the second. Size the count gate knowing one approval may be the
bot's, or raise it by one.

Earlier revisions of this document and the role's help text claimed the
Reviewer's approval was "informational, not the merge gate" without
qualification. That was true of the named-approver gate and false of the count
gate. The wording is now split per-gate everywhere it appears.

**Deliberately not fixed in code here.** Excluding `cfg.PRReviewer` from
`GhPrApprovalCount` would make the simpler claim true by construction, and is
the better end state. It is out of scope for a documentation-correctness change
because it silently changes merge behavior for every rig already running the
in-town Reviewer — a rig at `pr_required_approvals: 1` that merges today would
stop merging. That belongs in its own change with its own migration note.

### Tap-guard boundary (new role override)

Add a `reviewer` entry to `DefaultOverrides()`
(`internal/hooks/config.go:201`) blocking, at the Bash-tool layer:

- `gh pr merge` and **all of `gh pr review`** (including `--approve` and
  `--comment`): this is a channel restriction, not a limit on the verdict.
  Posting goes exclusively through `gt reviewer post`, which submits the
  review event (APPROVE / REQUEST_CHANGES / COMMENT) derived from finding
  severity; a raw-`gh` path would bypass the tested output contract
- the GraphQL review-submission mutations `addPullRequestReview` and
  `submitPullRequestReview`. `gh api graphql` matches neither the `gh pr
  review` pattern nor the REST `*gh api*pulls*reviews*` one, so without these
  matchers the channel restriction above has a hole wide enough to submit an
  approving review under the machine-user token with no output-contract
  validation
- the GraphQL `resolveReviewThread` mutation (thread resolution belongs to
  the authoring polecat — actor-boundary rule in
  [refinery-pr-workflow.md](refinery-pr-workflow.md))
- `git push` (any), `gt refinery pr *`, `bd close` on MR beads

This mirrors how the polecat/refinery write surfaces are kept disjoint: the
Reviewer's only write surfaces are its review bead, its own worktree
(checkout only), and PR review comments.

### Prompt-injection posture

PR diffs and commit messages are attacker-influenced text (third-party
dependency bumps; eventually human/external PRs). The Reviewer reads them as
data inside a session whose write surface is deliberately tiny (see
tap-guard list): the worst realistic injection outcome is a bad review
comment, which the human approval gate already absorbs. The role template
still includes the standard untrusted-content note for diff/commit text, and
the review request bead carries only town-generated metadata (PR number,
SHA, branch) — never raw PR body text.

## Config Surface Summary

| Setting | Location | Purpose |
|---|---|---|
| `merge_queue.pr_reviewer` | rig settings | Reviewer bot's GitHub login (existing field, `internal/refinery/engineer.go`) |
| `merge_queue.pr_trigger_comment` | rig settings | Set `""` on local-reviewer rigs (no comment trigger needed; await-review still gates) |
| `merge_queue.reviewer_local` | rig settings (**new**, both `MergeQueueConfig` copies: `internal/config/types.go:1252` and `internal/refinery/engineer.go`) | `true` → request-review step dispatches `gt reviewer request` instead of relying on an external bot |
| `review.perspectives` etc. | rig settings (**new** block) | Perspective enablement/order/caps |
| `<rig>/settings/review/perspectives/*.md` | rig | Perspective prompt files |
| `merge_queue.reviewer_token_env` | rig settings (**new**, both `MergeQueueConfig` copies) | Name of the env var holding the machine-user token; default `GT_REVIEWER_GITHUB_TOKEN`; resolved fail-fast at dispatch |
| `<town or rig>/roles/reviewer.toml` | role override | Model/provider (`start_command`), health thresholds |
| `GT_REVIEWER_GITHUB_TOKEN` | environment (default name) | Machine-user token value; per-rig identities point `reviewer_token_env` at a different var |

## Implementation Guide

### Phase 0 (optional, immediate): interim external replacement

No code. Self-host PR-Agent (Qodo Merge OSS), set
`merge_queue.pr_trigger_comment = "/review"` and `pr_reviewer` to its bot
login. Augment is out of the loop while the role is built. Verify its review
threads satisfy the unresolved-thread polling before relying on the fix loop.

### Phase 1: role plumbing

Goal: `gt role def reviewer` works; a reviewer session can be started by hand
and primed.

1. `internal/config/roles.go` — add `"reviewer"` to `AllRoles()` and
   `RigRoles()` (lines 108–121). This is the validation gate
   (`isValidRoleName`) for `LoadRoleDefinition`.
2. `internal/config/roles/reviewer.toml` — as specified above (picked up
   automatically by `//go:embed roles/*.toml`).
3. `internal/templates/roles/reviewer.md.tmpl` — role prompt per the
   structure above. Check the template loader/registry alongside the other
   role templates and register if enumeration is explicit.
4. `internal/config/types.go` `BuiltinRoleThemes()` (line 1243) — add a
   reviewer theme (suggest `"moss"`/green: evaluative, go/no-go).
5. `internal/hookutil/roletype.go` — extend role-type detection so
   `GT_ROLE=reviewer` and the `<rig>/reviewer` path resolve correctly.
6. `internal/hooks/config.go` `DefaultOverrides()` — tap-guard entry per the
   Security section.
7. Worktree provisioning: `<rig>/reviewer/rig` worktree creation, mirroring
   how `<rig>/refinery/rig` is provisioned (see `NewEngineer`'s gitDir
   resolution, `internal/refinery/engineer.go:381-387`, and rig add
   plumbing). Run `codegraph init` here if absent.
8. Tests: role load/override resolution (`internal/config/roles_test.go`
   patterns), roletype detection, hooks override merge.

Exit criteria: manually started reviewer session primes with the right
identity, finds no work, and waits per the guard.

### Phase 2: dispatch and completion commands

Goal: `gt reviewer request <pr> --mr <id>` produces a posted review
end-to-end on a test PR.

1. `internal/cmd/reviewer.go` — new cobra command group:
   - `gt reviewer request <pr> [--mr <bead-id>]`: create review-request bead
     (rig-prefixed task, label `review-request`, body = PR number + head SHA
     + branch + round number + `origin` (`refinery` when `--mr` given,
     `crew` otherwise) + MR id when present), mail it to the reviewer
     address, start the session via the session manager (reuse the dog
     `SessionManager` start/rollback pattern,
     `internal/daemon/handler.go:284-325`: assign → mail → start, roll back
     assignment on failure). If session already live: mail only. `--mr` is
     optional — standalone (crew) requests write no await-review-facing
     state. On round ≥ 2, the command fetches the prior rounds' review
     threads **and their responses** (reusing
     `internal/refinery/threads.go` plumbing) and embeds them, formatted, in
     the request payload — prior-round context is assembled in Go, never
     gathered by the agent; rounds key on the PR number so the logic is
     identical for both origins. Resolve `reviewer_token_env` here and fail
     fast if unset.
   - `gt reviewer checkout <pr>`: positive-half helper (the only sanctioned
     way the Reviewer touches git): fetch + detached checkout of the request
     SHA in the reviewer worktree.
   - `gt reviewer done`: clear state, close out, self-terminate session
     (model on `gt dog done`).
   - `gt reviewer stop [--rig] [--force]`, `gt reviewer status [--json]`,
     `gt reviewer list [--json]`: the operator's side of the lifecycle.
     `Manager.Stop()`/`Status()` existed from the start but had no commands
     attached, so ending a stuck reviewer meant `tmux kill-session` by hand.
     `status` combines session liveness with the heartbeat and reports the same
     four states the reaper acts on, in the same vocabulary; `list` sweeps every
     rig, which is otherwise impossible for a role with no registry entry.
2. `gt reviewer post --pr <n> --findings <json>` (**decided** — see Resolved
   Decisions #1): wraps the GitHub review API for atomic submission (one
   review, all inline threads). Owns the output contract — neutral
   priority badges, perspective tags — in tested Go; tap-guard blocks all
   raw `gh pr review` so this is the only posting path.
3. Widen `parseThreadPriority` (`internal/refinery/threads.go:121`) to
   accept `![<p>]` plus a URL containing `priority` and `.svg`, keeping the
   legacy gstatic `priority.svg` form. The post wrapper's emitter and the
   parser share fixtures in the same PR.
4. Reaper/wisp hygiene: review-request beads auto-close on completion;
   ensure the reaper's auto-close covers orphaned ones (cf. the
   plugin-receipt pileup fix, PR #91).
5. Tests: dispatch rollback paths, checkout SHA pinning, prior-round
   thread-context assembly, post wrapper thread/priority formatting against
   the widened `parseThreadPriority` (both badge forms) and
   `AwaitReviewStep` fixtures (`internal/refinery/await_review_test.go`,
   `threads_test.go` have the harness).

### Phase 3: refinery integration

Goal: a `merge_strategy=pr` rig with `reviewer_local=true` runs the whole
loop with zero operator action between MERGE_READY and the human approval.

1. Add `ReviewerLocal bool \`json:"reviewer_local,omitempty"\`` and
   `ReviewerTokenEnv string \`json:"reviewer_token_env,omitempty"\`` to
   **both** `MergeQueueConfig` structs (`internal/config/types.go:1252`
   block and `internal/refinery/engineer.go` block) and to config
   load/validation: `reviewer_local=true` requires non-empty `pr_reviewer`
   (the login still drives the engagement gate); `reviewer_token_env`
   defaults to `GT_REVIEWER_GITHUB_TOKEN` when empty.
2. Request-review step: where the patrol formula currently posts the trigger
   (`gt refinery pr await-review` first call posts `pr_trigger_comment`),
   branch on `reviewer_local`:
   - dispatch `gt reviewer request <pr> --mr <id>` once per review round
     (including each review-fix round — after a fix lands, the next round
     re-dispatches so the Reviewer re-reviews the new SHA),
   - run `await-review` with `--no-trigger`.
   Keep `await-review`'s exit codes, MR-bead persistence, and escalation
   untouched — they are the loop's spine.
3. Consider lowering `pr_review_wait` default for local dispatch (the
   5-minute physical-reality gate assumed an external bot's queue; a local
   session engages in ~1–2 min). Make it config, not code: document
   `pr_review_wait: "1m"` for `reviewer_local` rigs.
4. Tests: formula-level integration test on a fixture rig — request →
   review posted → threads detected → review-fix dispatch → re-review →
   gates. The tap-guard PR-workflow test file
   (`internal/cmd/tap_guard_pr_workflow_test.go`) shows the harness for
   boundary enforcement tests; add reviewer-boundary cases.

### Phase 4: crew integration and instruction convergence

Goal: crew PRs get the same automated first-pass review as refinery PRs,
and the crew template / `/crew-commit` contradiction is resolved.

1. Standalone request mode ships in Phase 2 (the `--mr`-optional command);
   this phase wires the crew side.
2. `/crew-commit` (`.claude/skills/crew-commit/SKILL.md`): add a step after
   PR creation — `gt reviewer request <pr#>` — plus a short "addressing
   review findings" note (push fixes, resolve own threads, re-request;
   round ≥ 2 carries prior threads automatically).
3. `internal/templates/roles/crew.md.tmpl`: replace the unconditional
   "No PRs in Maintainer Repos" section with merge-strategy-aware guidance
   per the convergence plan (Crew PRs section above); preferred mechanism
   is rendering on the rig's `merge_queue.merge_strategy` at prime time,
   interim static both-strategies rewrite acceptable. Annotate as a
   deliberate fork delta. Strengthen the existing "report-back reviews
   only" rule to name the Reviewer role as the sole GitHub-review poster.
4. Related cleanup: re-point the `pr-sheriff` skill's repo scope from
   `steveyegge/gastown` to this fork's repos.
5. Tests: standalone request bead shape (origin, no MR state), and a
   crew-path integration test — request → review posted → re-request round
   2 includes round-1 threads.

### Phase 5 (future): human-PR reviews via Telegraph

For PRs not created by the refinery or crew: Telegraph already translates
`issue_comment` events. A `gt review` comment on a PR (mayor-relevant per
existing filters, or a new reviewer-trigger relevance rule) → Mayor mail →
Mayor calls the same standalone `gt reviewer request <pr>` from Phase 2.
Deliberately deferred: it crosses the trust boundary from town-authored PRs
to external-authored PRs and needs its own injection review. Telegraph's
transport/translation layers need no changes for this; it is a Mayor-prompt
+ relevance-rule change.

### Quality gates (all phases)

Per repo standards: `go test ./...`, linting 100% green before any phase is
considered complete; no disabled tests. Beads: file implementation beads per
phase in the gastown rig (`bd create --rig gastown`), epic-linked.

## Failure Modes

| Failure | Detection | Outcome |
|---|---|---|
| Reviewer session crashes mid-review | Session dead, heartbeat present and ageing | Daemon reviewer reaper clears the heartbeat; next `await-review` poll still inside timeout → refinery re-dispatches (request is idempotent per PR+SHA: re-request with same SHA reuses the open bead) |
| Reviewer hangs | Heartbeat `timestamp` age > `stuck_threshold` (45m), or `elapsed` past the absolute cap | Reaper nudges once, then kills on the next cycle and emits a kill feed event; `await-review` exit 3 → escalation (existing path) at 30m regardless |
| Reviewer dispatched but never starts | Heartbeat stuck at `dispatched` with no live session | Reaper clears it and escalates — previously indistinguishable from "no review requested" |
| Reviewer loops without progressing | Heartbeat keeps refreshing but `elapsed` exceeds the absolute cap | Hard kill on `elapsed`, which a refreshing `timestamp` cannot evade |
| Review posted but malformed (no priority shields) | Threads parse with empty Priority | Loop still works — priority is advisory; review-fix dispatch carries the thread body regardless |
| Token invalid/expired | `gh` fails in-session | Reviewer escalates via `gt escalate`; `await-review` times out → exit 3; never blocks merge silently |
| Two requests race for one rig | Second `request` sees live session | Mail-queue semantics: one session, sequential drain |
| Crew request queued behind refinery rounds | `origin` field on request bead | v1 FIFO (advisory crew reviews tolerate latency); origin-aware drain order is a later, schema-compatible change |
| Crew never acts on findings | No gate exists for crew PRs by design | Human sees unaddressed threads at approval time — the review is input to human judgment, not a blocking loop |

## Resolved Decisions (overseer review, 2026-06-10)

1. **Review posting goes through a Go wrapper.** `gt reviewer post --pr <n>
   --findings <json>` is the only sanctioned write path; tap-guard blocks raw
   `gh pr review` entirely (not just `--approve`/`--request-changes`). The
   output contract lives in tested Go, and the Reviewer follows the
   positive-half-command pattern like every other actor.
2. **Fix rounds get a full re-review with a no-relitigation rule, and
   prior-round context is assembled deterministically.** Each round reviews
   the full PR diff at the new SHA. `gt reviewer request` embeds the prior
   rounds' review threads and their responses in the request payload —
   fetched and formatted by Go (reusing `internal/refinery/threads.go`
   plumbing), never gathered by the agent. The role template forbids raising
   new findings on code unchanged since the last reviewed SHA unless a fix
   commit changed its behavior. Context *assembly* is mechanical; context
   *judgment* is the agent's.
3. **Neutral priority badges; parser widened.** The post wrapper emits
   shields.io-style badges; `parseThreadPriority` is widened (~2 lines) to
   accept `![<p>]` plus a URL containing `priority` and `.svg`, with the
   legacy gstatic form still accepted so interim external bots coexist
   during migration. Emitter and parser ship together with shared fixtures.
4. **Token env-var name is rig-configurable with a town-wide default.** New
   `merge_queue.reviewer_token_env` (default `GT_REVIEWER_GITHUB_TOKEN`),
   following telegraph's secret-resolution pattern: config stores the env
   var *name*, never the value; unset resolves to a fail-fast at dispatch.
   One machine user in practice today; per-rig identities are pure config.
