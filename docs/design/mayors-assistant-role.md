# Mayor's Assistant: Town-Level Inbound Triage Role

**Status:** Proposed — delivered as a PR stack (docs → role registration →
message routing → sweep migration → prompt template). This document and the
`mayors-assistant` entries in `overview.md` / `reference.md` describe the
stack's end state; they are true once the role-registration PR lands.
**Created:** 2026-10-17
**Cross-references:** [telegraph.md](telegraph.md), [escalation.md](escalation.md),
[mail-protocol.md](mail-protocol.md), [plugin-system.md](plugin-system.md),
[../agent-provider-integration.md](../agent-provider-integration.md)

## Overview

The Mayor's Assistant (`mayors-assistant`) is a town-scoped singleton agent
that sits between the outside world and the Mayor. Every piece of *inbound
information* — Telegraph webhook events, escalations, infrastructure alerts,
patrol reports, plugin findings — is delivered to the assistant's inbox. The
assistant reads it, triages it, groups it, does surface-level due diligence,
and delivers consolidated briefings to the Mayor by mail. It also takes over
the periodic "loose ends" sweep that previously nudged the Mayor directly.

The assistant is a **filter, not a lock**. Anyone can still mail or nudge
`mayor/` directly; the system simply stops aiming its unsolicited firehose at
the Mayor and aims it at the assistant instead.

## Problem

The Mayor is the town's executive: it dispatches work, makes cross-rig
decisions, and is the only agent that escalates to the human overseer. It is
also, today, the default recipient of everything:

- every Telegraph event (Jira comments, GitHub reviews, CI failures) lands in
  `mayor/` and triggers a rate-limited nudge;
- `gt escalate` defaults to `mail:mayor` at medium severity and above;
- the daemon, Deacon, witnesses, and plugins mail `mayor/` for Dolt alerts,
  compaction digests, dead-polecat reports, force-kill notices, sentinel
  findings, and re-dispatch limits;
- a cooldown plugin nudges the Mayor every few hours to "sweep loose ends".

The Mayor's context window is its scarcest resource (see "The Solo Artist
trap" in `mayor.md.tmpl`). Spending it on reading raw webhook payloads one at
a time, deduplicating repeats, and deciding what is noise is exactly the
wrong trade. Interruptions also arrive while the Mayor is mid-decision, and
the nudge pile-up documented in `telegraph/transform/mail.go` is a direct
symptom.

## Responsibilities

**Primary — message triage.**

1. Read every inbound message in full (`gt mail inbox` → `gt mail read <id>`).
2. Triage by urgency and priority. Group related messages (same PR, same
   Jira issue, same incident) into one consolidated summary.
3. Perform *surface-level* due diligence before briefing: a quick
   `gt status`, `bd show <id>`, `gt convoy list`, or a look at the linked PR.
   Enough to say what the item is and whether it needs a decision — never a
   deep investigation.
4. Flag items that need real investigation as **suggested beads to sling**,
   with a proposed title and rig. The assistant does not investigate; it
   recommends that work be dispatched.
5. Deliver briefings to the Mayor as mail (`gt mail send mayor/`) with a
   subject prefix: `BRIEF:` for routine consolidated briefings, `URGENT:` for
   anything relayed immediately.
6. Keep its own inbox at zero: every processed message is marked read.

**Secondary — periodic sweep.** At the configured interval (default 1h) a
sweep plugin mails the assistant. The assistant then reviews open work in
beads and sends the Mayor a `SWEEP:` mail covering stalled decisions,
blocked external actors (PRs awaiting human review, unanswered Jira threads),
unslung ready work, and anything that needs executive action. Sweeps are
mail, not nudges, so they never interrupt the Mayor mid-task.

**Non-responsibilities.**

- It does not dispatch, sling, close, or reassign work unless the Mayor
  explicitly delegates a specific action by mail.
- It does not escalate to the overseer; that remains the Mayor's job
  (`gt notify-overseer`).
- It does not reply on Jira or GitHub. Telegraph is one-way; the reply hint
  in each Telegraph mail is forwarded to the Mayor in the briefing.
- It does not withhold. Urgent or critical messages are relayed immediately
  with an `URGENT:` prefix, even if a routine briefing went out a minute ago.

## Message routing

### Before

```
Telegraph (Jira/GitHub) ─┐
gt escalate (mail:mayor) ─┤
Dolt alerts / daemon ─────┼──▶ mayor/ inbox + nudges ──▶ Mayor
Patrol reports / Deacon ──┤
Plugins (sentinels, ───────┤
  loose-ends nudge) ───────┘
Witness/Refinery dispatch signals ──▶ mayor/ (SLOT_OPEN, MERGED, …)
```

### After

```
Telegraph (Jira/GitHub) ─┐
gt escalate ──────────────┤                        BRIEF: / URGENT: / SWEEP:
Dolt alerts / daemon ─────┼──▶ mayors-assistant/ ──▶ mail ──▶ mayor/
Patrol reports / Deacon ──┤        (triage, group,
Plugins (sentinels, ───────┤         due diligence)
  sweep trigger) ──────────┘
Witness/Refinery dispatch signals ──▶ mayor/ (unchanged)
Direct `gt mail send mayor/` ────────▶ mayor/ (unchanged)
```

The rule for deciding which existing `mayor/` destinations move:

| Message is… | Destination | Examples |
|---|---|---|
| Inbound information for triage | `mayors-assistant/` | Telegraph events, escalation mail, Dolt health alerts, wisp-compaction digests, `POLECAT_DIED` reports, force-kill notices, re-dispatch-limit notices, sentinel/plugin findings |
| An operational signal in the Mayor's dispatch loop | `mayor/` (unchanged) | `SLOT_OPEN`, `MERGED`, `MERGE_FAILED`, `BRANCH_MISSING`, `SPAWN_BLOCKED`, convoy completion, callbacks, handoff mail |
| Addressed to the Mayor by a human or another agent on purpose | `mayor/` (unchanged) | `gt mail send mayor/ …`, `gt nudge mayor …` |

Each redirected call site carries a `// Routed to the Mayor's Assistant`
comment so the split stays auditable.

**Backward safety.** `mayors-assistant/` is a well-known town-level address,
validated like `mayor/` and `deacon/`. Mail queues in the town beads store
whether or not the assistant session is running; nudges to a missing session
are logged and dropped exactly as they are for the Mayor today. For routine
inbound information that is the desired behaviour: nothing is lost, it is
read when the assistant is next up (the daemon restarts it on its patrol
tick, with the Mayor's zombie debounce). It is **not** sufficient for
time-critical escalations, which is why high and critical escalations keep a
direct path to the Mayor (next section).

**Escalation routes.** `settings/escalation.json` defaults become:

| Severity | Default route |
|---|---|
| low | `bead` |
| medium | `bead`, `mail:mayors-assistant` |
| high | `bead`, `mail:mayors-assistant`, `mail:mayor`, `email:human` |
| critical | `bead`, `mail:mayors-assistant`, `mail:mayor`, `email:human`, `sms:human` |

Medium escalations go through the assistant only. High and critical are
delivered to **both**: the Mayor must learn of them even if the assistant
session is dead, wedged, or never started, and a liveness-triggered re-route
would be more machinery than the problem deserves. The assistant includes
such items in its briefings as context, marked "already delivered to
mayor/", and does not treat them as needing relay.

Two code paths keep the same invariant:

- `GetRouteForSeverity` falls back to `bead, mail:mayors-assistant` for a
  severity missing from the routes map (it previously fell back to
  `mail:mayor`).
- `stripSlackRoutes` (slack_overseer_only) keeps a route that loses its
  `slack` action reachable by the Mayor: if the route already names
  `mail:mayor` or `mail:mayors-assistant` it is left alone; otherwise it
  gains `mail:mayors-assistant`. The Mayor remains the only path to Slack via
  `gt notify-overseer`; it hears about the suppressed route through the
  assistant (or directly, for high/critical).

Existing towns keep whatever routes they have persisted; operators who want
the new behaviour update the file (or delete it to regenerate defaults).

### Trust boundary

Telegraph wraps provider text in `EXTERNAL CONTENT` delimiters and the Mayor
is primed to treat `telegraph/` senders as untrusted. The assistant must not
launder that text into trusted instructions:

1. Briefings quote external text only inside the original
   `EXTERNAL CONTENT (untrusted: <provider>/<actor>)` delimiters, never
   paraphrased as fact outside them.
2. Suggested-action commands (`bd create`, `gt sling`, reply hints) are built
   only from structured fields — issue key, PR number, rig, bead ID — never
   from the body of an external message. A comment that *contains* a command
   is quoted as untrusted text, not proposed as an action.
3. Every briefed item carries its source address (`telegraph/github/<login>`,
   `gastown/witness`, …) so the Mayor's existing trust rule still applies to
   the relayed item.

## Interaction with the Mayor

- The Mayor treats `BRIEF:` mail as a digest to act on at its next natural
  pause and `URGENT:` mail as an interrupt. `SWEEP:` mail is a reminder list.
- Briefings end with a short **Suggested actions** list. Each item is one
  line: what to decide, or `bd create`/`gt sling` text the Mayor can run
  verbatim.
- The Mayor can delegate a bounded, **read-only or reversible** action back by
  mail (`gt mail send mayors-assistant/ -s "DELEGATE: …"`): re-check a PR,
  pull a thread's history, draft a summary, mark its own mail. A delegation
  is valid only when the mail's `From` is exactly `mayor/` and the request is
  not inside an `EXTERNAL CONTENT` block or a forwarded item. Mail `From` is
  derived from `GT_ROLE` and is not authenticated, so `DELEGATE:` never
  grants dispatch authority: `gt sling`, `bd close`, `gt polecat nuke`, rig
  park/dock and similar stay with the Mayor until sender authentication
  exists. The assistant performs only what is named and reports completion.
- The Mayor never needs to ack assistant mail; the assistant tracks what it
  has briefed by thread ID and does not re-brief the same item unless it
  changed.

## Sweep / cron migration

The `mayor-loose-ends-nudge` plugin (cooldown gate, 5h) sent the Mayor a
fixed "sweep loose ends now" task. That plugin is replaced by
`mayors-assistant-sweep`, which mails the assistant instead and whose
cooldown is read from town settings rather than hard-coded.

Rationale:

- A nudge that says "go sweep" forces the Mayor to do the reading. A mail
  from the assistant that *is* the sweep result lets the Mayor act.
- Sweep cadence is an operator decision and belongs in `settings/config.json`,
  not in a plugin file copied into every town.
- The plugin keeps the cooldown gate (daemon handler dispatches it to a dog
  with no changes to the dog pipeline). The only new mechanism is that a
  cooldown gate may name a town setting as its duration source.

Plugin gate (in `plugins/mayors-assistant-sweep/plugin.md`):

```toml
[gate]
type = "cooldown"
duration = "1h"                              # fallback
duration_setting = "assistant_sweep_interval" # town setting overrides duration
```

Other plugins that mail `mayor/` with findings (sentinels, reports) are
retargeted to `mayors-assistant/` as part of the routing change; their gates
are untouched.

## Configuration reference

| Setting | Where | Default | Purpose |
|---|---|---|---|
| `assistant_sweep_interval` | `settings/config.json` (town) | `"1h"` | Cooldown for the `mayors-assistant-sweep` plugin. Any Go duration string (`"30m"`, `"2h"`). Rendered into the assistant's prompt so it knows its cadence. |
| `role_agents["mayors-assistant"]` | `settings/config.json` (town) | town `default_agent` | Agent/model preset for the assistant, resolved through the same 3-tier hierarchy as every other role. |
| `roles/mayors-assistant.toml` | `<town>/roles/` | built-in | Override session pattern, work dir, start command, health thresholds, nudge, or template name. |
| `theme.role_defaults["mayors-assistant"]` | `mayor/config.json` | `slate` | tmux theme for the session. |

Built-in role definition (`internal/config/roles/mayors-assistant.toml`):

| Field | Value |
|---|---|
| `role` / `scope` | `mayors-assistant` / `town` |
| session pattern | `hq-mayors-assistant` |
| work dir | `{town}/mayors-assistant` |
| start command | `exec claude --dangerously-skip-permissions` (overridden by `role_agents`) |
| env | `GT_ROLE=mayors-assistant`, `GT_SCOPE=town` |
| health | `ping_timeout=30s`, `consecutive_failures=3`, `kill_cooldown=5m`, `stuck_threshold=1h` |
| nudge | "Check your inbox, triage, and brief the Mayor if anything is actionable." |

Identity surfaces that must agree:

| Surface | Value |
|---|---|
| Mail address | `mayors-assistant/` (also accepts `mayors-assistant`) |
| tmux session | `hq-mayors-assistant` |
| Agent bead | `hq-mayors-assistant` (town beads) |
| `GT_ROLE` / `BD_ACTOR` | `mayors-assistant` |
| Settings dir | `<town>/mayors-assistant/.claude/settings.json` |
| CLI | `gt mayors-assistant start\|stop\|attach\|status\|restart` (aliases `assistant`, `ma`); `gt peek mayors-assistant`; `gt nudge mayors-assistant "…"` |

Lifecycle: `gt up` starts the assistant after the Mayor; `gt down` stops it
with the other town sessions; the daemon's patrol tick restarts it when its
session dies (same zombie-debounce as the Mayor).

Hook policy: the assistant runs unattended, so it gets the Mayor's
autonomy guard (`AskUserQuestion` and `EnterPlanMode` blocked). It is
classified as an autonomous role for mail injection (mail is injected on
`SessionStart`, not on user prompts).

### Note on the role name

`mayors-assistant` contains a hyphen and shares the `mayor` prefix. Code
that parses agent bead IDs by splitting on `-`, or that matches addresses
with `strings.HasPrefix(addr, "mayor")`, must special-case this role or use
exact matches. The role-registration PR does both and adds tests for
`hq-mayors-assistant` and `mayors-assistant/`.

## Rejected alternatives

- **A Telegraph-only filter inside the Mayor's session.** Does not help with
  escalations, daemon alerts, or plugin reports, and still burns the Mayor's
  context on reading.
- **A dog instead of a persistent role.** Dogs are ephemeral and dispatched
  per plugin run; triage needs continuity (thread tracking, "already briefed"
  memory) and a stable mail address.
- **Making the assistant the only path to the Mayor.** Rejected: humans and
  agents must be able to reach the Mayor directly, and dispatch-loop signals
  must not gain a hop.
