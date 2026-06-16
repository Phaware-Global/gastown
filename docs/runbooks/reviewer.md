# Local Reviewer Operator Runbook

The **Reviewer** is a rig-level, on-demand agent role that performs AI code
review on pull requests and posts its findings as a GitHub PR review under a
dedicated machine-user identity. It replaces the externally-hosted Augment app
in the refinery's PR review loop. The Reviewer **never approves and never
merges** — human approval stays the merge gate.

This runbook covers configuring, operating, and troubleshooting a local
Reviewer for a rig. Design reference: [`docs/design/reviewer-role.md`](../design/reviewer-role.md).
Jira: **P23-2376**.

---

## How it fits together

```
Refinery (merge_strategy=pr)                Reviewer (gt-<rig>-reviewer)        GitHub
  │ MERGE_READY → rebase, push, open PR ───────────────────────────────────────▶ PR#N
  │ wait CI green                                                                  │
  ├─ reviewer_local? → gt reviewer request <pr> --mr <id> ──▶ (bead + session)     │
  │                                          ├─ gt reviewer checkout <pr> --sha …  │
  │                                          ├─ gt reviewer prompt <p> (per lens)   │
  │                                          ├─ N perspective subagents IN PARALLEL │
  │                                          ├─ gt reviewer consolidate *.json      │
  │                                          ├─ gt reviewer post --pr N --findings ▶ inline threads
  │                                          └─ gt reviewer done (self-terminate)   │
  ├─ gt refinery pr await-review --no-trigger (polls for the review)               │
  ├─ unresolved threads → review-fix polecat → re-request (round+1)                │
  └─ wait-approval (HUMAN) → squash-merge                                          │
```

The refinery loop contract is unchanged: `await-review`'s exit codes, the
SHA-scoped engagement gate, unresolved-thread polling, review-fix dispatch, and
the human-approval merge gate all work exactly as before. The only thing
`reviewer_local` changes is *who* produces the review (an in-town Reviewer
instead of an external bot) and *how* it's woken (direct dispatch instead of a
trigger comment).

---

## 1. Create the machine user + token

The Reviewer posts under a dedicated GitHub identity so the review's author
differs from the PR author (GitHub then accepts the review).

1. Create a machine user, e.g. `yourorg-reviewer-bot`, with **`repo`** scope on
   the rig's repositories.
2. **It must NOT** be the `pr_approver`, and **must NOT** have merge rights on
   protected branches. Branch protection is the backstop to the Reviewer's
   tap-guard.
3. Generate a personal access token for it.

## 2. Store the token durably — `~/gt/settings/daemon.env`

The token's **value** lives only in the daemon's environment; config stores
only the env var **name**. Put it in `settings/daemon.env`, the same place the
Telegraph secrets live — it's sourced by the launchd/systemd unit that starts
the daemon, so it survives daemon restarts **and** system reboots.

```bash
echo 'export GT_REVIEWER_GITHUB_TOKEN=ghp_yourtoken' >> ~/gt/settings/daemon.env
chmod 600 ~/gt/settings/daemon.env        # owner-only; never commit this file
```

Use `export` — the installed launchd plist sources the file with a plain
`. daemon.env` (no `set -a`), so only exported vars reach the daemon.

A **running** daemon won't see the new var until it re-sources the file:

```bash
gt daemon restart
# or: launchctl kickstart -k gui/$(id -u)/com.gastown.daemon   (macOS)
```

> Multi-org towns can point each rig at a different token by setting
> `merge_queue.reviewer_token_env` to a distinct var name (see below) and adding
> that var to `daemon.env`. Single-org towns use the default name and one
> machine user.

## 3. Configure the rig — `<rig>/settings/config.json`

Two blocks. In `merge_queue` (only meaningful when `merge_strategy: "pr"`):

```jsonc
"merge_queue": {
  "merge_strategy": "pr",
  "pr_reviewer": "yourorg-reviewer-bot",            // the bot's GitHub LOGIN — drives the engagement gate
  "reviewer_local": true,                            // dispatch the in-town Reviewer instead of an external bot
  "reviewer_token_env": "GT_REVIEWER_GITHUB_TOKEN",  // env var NAME holding the token (default if omitted)
  "pr_trigger_comment": "",                          // no external comment trigger needed for local dispatch
  "pr_review_loop_max": 3                            // max review→fix→re-review iterations
}
```

A new top-level `review` block (sibling of `merge_queue`):

```jsonc
"review": {
  "perspectives": ["adversarial", "security", "go-idioms"],  // enabled lenses, in order
  "max_findings_per_perspective": 8,        // cap per pass (default 8); overflow summarized
  "fail_silent_perspectives": false,        // missing perspective file = config error, not silent skip
  "max_rounds": 3,                          // per-rig review-iteration override (see below)
  "parallel": false                         // reserved for v2 parallel perspectives
}
```

**Validation** (enforced at `gt rig settings set` and at refinery load):
`reviewer_local: true` requires a non-empty `pr_reviewer`; negative caps/rounds
are rejected; `reviewer_local` requires `merge_strategy: "pr"`.

**Number of review iterations is per-rig.** It resolves in this order:

| Source | When |
|---|---|
| `review.max_rounds` | explicit per-rig knob (when > 0) |
| `merge_queue.pr_review_loop_max` | shared refinery cap (when > 0) |
| built-in default (3) | neither set |

## 4. Perspectives

Each perspective is a plain-markdown prompt injected verbatim as a review pass.
Resolution is **rig → town → built-in**:

```
<rig>/settings/review/perspectives/<name>.md     # rig-specific
<town>/settings/review/perspectives/<name>.md     # town-shared fallback
(embedded defaults: adversarial, security)         # ship with the binary
```

`adversarial` and `security` are embedded, so a rig with an empty `review`
block still gets a useful review. Add your own (e.g. `go-idioms.md`,
`simplify.md`) and list them in `review.perspectives`.

Inspect what resolves for the current rig:

```bash
gt reviewer perspectives                  # lists enabled perspectives + source (rig/town/builtin) + path
gt reviewer perspectives --show security  # prints a perspective's resolved prompt content
```

### Generated per-perspective prompts (deterministic execution contract)

The perspective markdown is only the **lens** ("what to look for"). The
deterministic **execution contract** ("how to review, what to return") lives in
one place — a shared, templated instruction block — and is injected into every
generated prompt. The Reviewer never hand-writes a pass prompt; it generates one
per perspective:

```bash
gt reviewer prompt <name> --pr <N> --sha <head-sha> --round <r> \
  [--prior-threads prior-threads.txt] [--instructions extra.txt]
```

The generated prompt embeds the lens, the exact SHA/diff to review, the codegraph
tooling expectations, the evidence standard, the per-pass finding cap
(`review.max_findings_per_perspective`), and the required output JSON schema —
leaving no part of the procedure to agent interpretation. `--instructions` is the
slot for injecting any additional execution instructions verbatim.

A perspective named in config but missing at every tier is a **config error**
(unless `fail_silent_perspectives: true`), so a typo surfaces loudly. Path
separators and `..` in a perspective name are rejected (no directory traversal).

## 5. (Optional) Model / provider override

The Reviewer defaults to Claude. To run it on another agent (Gemini, Codex, …),
drop a role override that replaces `start_command` — uses the existing
`agents.json` presets, no new plumbing:

```
<town>/roles/reviewer.toml   or   <rig>/roles/reviewer.toml
```
```toml
[session]
start_command = "exec gemini --approval-mode yolo"
```

## 6. (Optional) Codegraph

For graph-aware review (callers/impact/explore), the reviewer worktree should be
codegraph-indexed: run `codegraph init` in `<rig>/reviewer/rig` and register the
codegraph MCP server via a per-worktree `.mcp.json` pointed at that path. This is
the Reviewer's defining advantage over a diff-only reviewer.

---

## Operating

### What happens automatically

With `reviewer_local: true` on a `merge_strategy=pr` rig, the refinery patrol
dispatches the Reviewer for you — no operator action between MERGE_READY and the
human approval gate. Per review round the refinery runs `gt reviewer request
<pr> --mr <mr-id>` and then polls with `gt refinery pr await-review
--no-trigger`. After a fix round lands, it re-dispatches so the Reviewer
re-reviews the new SHA.

### Manual dispatch (debugging / crew PRs)

```bash
# Request a review of PR #N for the current rig (omit --mr for a standalone/crew request)
gt reviewer request <N> [--mr <mr-bead-id>]

# Inside the reviewer session, the role checks out, reviews in parallel, and posts:
gt reviewer checkout <N> --sha <head-sha>          # detached checkout at the reviewed SHA
gt reviewer prompt adversarial --pr <N> --sha <head-sha> --round 1 > prompt-adversarial.txt
gt reviewer prompt security    --pr <N> --sha <head-sha> --round 1 > prompt-security.txt
#   → launch one subagent per prompt IN PARALLEL; each returns a PerspectiveResult JSON
#     saved to perspective-<name>.json
gt reviewer consolidate perspective-*.json --sha <head-sha> --out findings.json
gt reviewer post --pr <N> --findings findings.json # the ONLY sanctioned posting path
gt reviewer done                                    # clear state + self-terminate the session
```

`gt reviewer checkout` and `gt reviewer post` refuse to run outside a reviewer
worktree (`<rig>/reviewer/rig`). `gt reviewer prompt` and `gt reviewer
consolidate` are pure generators/transformers — no GitHub calls — and run
anywhere in the rig.

The Reviewer runs perspective passes **in parallel** (one subagent per enabled
perspective, each reviewing strictly from its generated prompt), then
deterministically deduplicates the collected findings with `gt reviewer
consolidate` before the single `post`. Dedup keys on `(path, line, title)`, keeps
the highest priority, and unions the perspective tags; the consolidated summary
carries a verdict line for **every** perspective (perspectives with no findings
are still accounted for).

### Findings JSON shape (consumed by `gt reviewer post`)

```json
{
  "summary": "per-perspective verdicts + counts",
  "reviewed_sha": "abc123…",
  "findings": [
    {
      "path": "internal/foo.go", "line": 42,
      "priority": "high", "perspective": "adversarial",
      "title": "one-line summary",
      "body": "explanation with codegraph evidence",
      "suggestion": "concrete change"
    }
  ]
}
```

`priority` is `high|medium|low`. The post wrapper emits each finding with a
neutral shields.io priority badge and a `[perspective]` tag — the exact format
the refinery's `parseThreadPriority` understands, so findings flow into the
review-fix loop with priorities intact. A review with inline findings is always
anchored to a commit SHA (`--sha`, else the payload's `reviewed_sha`, else the
PR head); GitHub rejects an inline review without one.

---

## Verifying / observing

```bash
gt session peek gt-<rig>-reviewer       # watch the reviewer session live
gt session list | grep reviewer          # is a reviewer session running?
bd list --label review-request           # outstanding/closed review-request beads
gt mq list <rig>                          # the MR moving through the loop
```

On GitHub, a successful review appears as a **COMMENTED** review by the
`pr_reviewer` login, with one inline thread per finding and a top-level summary
listing per-perspective verdicts, a finding count, and the reviewed SHA.

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `reviewer_token_env … is not set` at dispatch | token var missing from the daemon's env | add `export GT_REVIEWER_GITHUB_TOKEN=…` to `settings/daemon.env`, `gt daemon restart` |
| Refinery posts an external trigger comment instead of dispatching | `reviewer_local` not `true`, or `merge_strategy` ≠ `pr` | fix rig `merge_queue`; settings validation requires `pr_reviewer` when `reviewer_local` |
| `await-review` times out (exit 3) → escalation | Reviewer never engaged within `pr_review_timeout` | check `gt session peek gt-<rig>-reviewer`; verify the token is valid; check the review-request bead |
| Review posted but threads have no priority | malformed badge | the post wrapper emits the badge; priority is advisory — the fix loop still carries the thread |
| `must run inside the reviewer worktree` | ran `post`/`checkout` outside `<rig>/reviewer/rig` | run from the reviewer worktree |
| `gt reviewer checkout` fails fetching `pull/<n>/head` | non-GitHub provider | v1 is GitHub-only by design (Bitbucket `SubmitReview` returns `ErrUnsupported`) |
| Reviewer session hung / never finished | hung review | `await-review`'s 30m timeout escalates (exit 3) so a hung Reviewer never blocks a merge — the functional safety net; the session self-terminates on `gt reviewer done`, and a zombie (tmux alive, agent dead) is recreated on the next dispatch |
| Perspective named in config not applied | missing file at every tier | `gt reviewer perspectives` shows what resolves; add the file or fix the name (or set `fail_silent_perspectives`) |

A hung or crashed Reviewer **never blocks a merge silently**: `await-review`'s
timeout escalates (exit 3) and the daemon reaps the stale session.

---

## Security posture

- The Reviewer's only write surfaces are its review bead, its own worktree
  (checkout only), and PR review comments via `gt reviewer post`. A tap-guard
  blocks `gh pr review`, `gh pr merge`, raw `gh api .../pulls/*/reviews`,
  `git push`, `gt refinery pr *`, and review-thread resolution.
- The machine user must not be `pr_approver` and must not have protected-branch
  merge rights — branch protection backstops the tap-guard.
- PR diffs and commit messages are **attacker-influenced data**, not
  instructions. The review-request bead carries only town-generated metadata
  (PR number, SHA, branch); raw PR body text never enters it. The worst
  realistic injection outcome is a bad review comment, which the human approval
  gate absorbs.
- The token value never touches disk (outside the 0600 `daemon.env`) or logs;
  config stores only the env var name, and an unset var fails fast at dispatch.

---

## Reference: config surface

| Setting | Location | Purpose |
|---|---|---|
| `merge_queue.pr_reviewer` | rig settings | Reviewer bot's GitHub login (drives the engagement gate) |
| `merge_queue.reviewer_local` | rig settings | `true` → dispatch the in-town Reviewer |
| `merge_queue.reviewer_token_env` | rig settings | env var **name** for the machine-user token (default `GT_REVIEWER_GITHUB_TOKEN`) |
| `merge_queue.pr_trigger_comment` | rig settings | set `""` for local dispatch |
| `merge_queue.pr_review_loop_max` | rig settings | review-iteration cap (fallback for `review.max_rounds`) |
| `review.perspectives` / caps / `max_rounds` / `parallel` | rig settings | perspective enablement, order, caps, per-rig iterations |
| `<rig|town>/settings/review/perspectives/*.md` | rig/town | perspective prompt files |
| `<town|rig>/roles/reviewer.toml` | role override | model/provider (`start_command`), health thresholds |
| `GT_REVIEWER_GITHUB_TOKEN` | `settings/daemon.env` | machine-user token **value** (durable across reboots) |
