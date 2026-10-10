# Gas Town Escalation Protocol

> Reference for the unified escalation system in Gas Town.

## Overview

Gas Town agents escalate issues when automated resolution is not possible.
Escalations are severity-routed, tracked as beads, and support stale detection
with automatic re-escalation.

## Severity Levels

| Level | Priority | Description | Default Route |
|-------|----------|-------------|---------------|
| **CRITICAL** | P0 (urgent) | System-threatening, immediate attention | bead + mail + email + SMS |
| **HIGH** | P1 (high) | Important blocker, needs human soon | bead + mail + email |
| **MEDIUM** | P2 (normal) | Standard escalation, human at convenience | bead + mail mayor |

## Tiered Escalation Flow

```
Agent -> gt escalate -s <SEVERITY> "description"
           |
           v
     [Deacon receives]
           |
           +-- resolves --> updates issue, re-slings work
           +-- cannot  --> forwards to Mayor
                              +-- resolves --> updates issue, re-slings
                              +-- cannot  --> forwards to Overseer --> resolves
```

Each tier can resolve OR forward. The chain is tracked via bead comments.

## Configuration

Config file: `~/gt/settings/escalation.json`

### Default Configuration

```json
{
  "type": "escalation",
  "version": 1,
  "routes": {
    "medium": ["bead", "mail:mayor"],
    "high": ["bead", "mail:mayor", "email:human"],
    "critical": ["bead", "mail:mayor", "email:human", "sms:human"]
  },
  "contacts": {
    "human_email": "",
    "human_sms": "",
    "slack_webhook": "",
    "smtp_host": "",
    "smtp_port": "587",
    "smtp_from": "",
    "smtp_user": "",
    "smtp_pass": "",
    "sms_webhook": ""
  },
  "stale_threshold": "4h",
  "max_reescalations": 2,
  "slack_overseer_only": true
}
```

### Action Types

| Action | Format | Behavior |
|--------|--------|----------|
| `bead` | `bead` | Create escalation bead (always first, implicit) |
| `mail:<target>` | `mail:mayor` | Send gt mail to target |
| `email:human` | `email:human` | Send email to `contacts.human_email` |
| `sms:human` | `sms:human` | Send SMS to `contacts.human_sms` |
| `slack` | `slack` | Post to `contacts.slack_webhook` — **suppressed** unless `slack_overseer_only` is `false` (see below) |
| `log` | `log` | Write to escalation log file |

### Slack is overseer-only (`slack_overseer_only`)

Slack reaches a human's phone. Routing every escalation there produced a DM
flood of raw, unfiltered beads (dozens per hour during a dolt-archive outage).
Slack is therefore reserved for the mayor's judgment:

- `slack_overseer_only` defaults to `true` when the key is absent.
- While it is `true`, `LoadEscalationConfig` strips every `slack` action from
  `routes` and prints one warning per affected severity. A route that loses a
  `slack` action keeps (or gains) `mail:mayor`, so a suppressed route degrades
  to the gatekeeper rather than to silence. `gt escalate` also
  gates the action at execution time and records
  `slack route suppressed: use gt notify-overseer for Slack (slack_overseer_only=true)`
  in the delivery status. The suppression is not a missing-contact skip, so it
  never hard-fails a HIGH/CRITICAL escalation.
- `gt escalate stale` re-routes only `mail:` targets and never ran external
  actions; nothing in the automated re-escalation path can reach Slack.
- Set `"slack_overseer_only": false` to restore per-escalation Slack posts.

The only path to Slack is `gt notify-overseer` (below).

## Escalation Beads

Escalation beads use `type: escalation` with structured labels for tracking.

### Label Schema

| Label | Values | Purpose |
|-------|--------|---------|
| `severity:<level>` | MEDIUM, HIGH, CRITICAL | Current severity |
| `source:<type>:<name>` | plugin:rebuild-gt, patrol:deacon | What triggered it |
| `acknowledged:<bool>` | true, false | Has human acknowledged |
| `reescalated:<bool>` | true, false | Has been re-escalated |
| `reescalation_count:<n>` | 0, 1, 2, ... | Times re-escalated |
| `original_severity:<level>` | MEDIUM, HIGH | Initial severity |

## Category Routing (future)

Categories provide structured routing based on the nature of the escalation.
Not yet implemented as CLI flags; currently use `--to` for explicit routing.

| Category | Description | Default Route |
|----------|-------------|---------------|
| `decision` | Multiple valid paths, need choice | Deacon -> Mayor |
| `help` | Need guidance or expertise | Deacon -> Mayor |
| `blocked` | Waiting on unresolvable dependency | Mayor |
| `failed` | Unexpected error, can't proceed | Deacon |
| `emergency` | Security or data integrity issue | Overseer (direct) |
| `gate_timeout` | Gate didn't resolve in time | Deacon |
| `lifecycle` | Worker stuck or needs recycle | Witness |

## Commands

### gt escalate

Create a new escalation.

```bash
gt escalate -s <MEDIUM|HIGH|CRITICAL> "Short description" \
  [-m "Detailed explanation"] [--source="plugin:rebuild-gt"]
```

Flags: `-s` severity (required), `-m` body, `--source` origin identifier,
`--to` route to tier (deacon/mayor/overseer), `--dry-run`, `--json`.

For Dolt outages or GT behavior mismatches that involve Dolt-backed state, add
the RCA capture checklist from `docs/dolt-health-guide.md` to the escalation
body or the follow-up bead before restarting services.

### gt escalate ack

Acknowledge one or more escalations (prevents re-escalation).

```bash
gt escalate ack <bead-id> [<bead-id>...]
```

### gt notify-overseer

Mayor-only. Posts one composed, human-readable ops brief to
`contacts.slack_webhook`. Restricted to the mayor identity (a human at the
terminal with no agent identity is also allowed, for testing the webhook).

```bash
gt notify-overseer -s "<subject>" --category <infra|ops|security> \
  (-m "<composed summary>" | --stdin) [--refs <id1,id2,...>]
```

- `--category` is required and limited to `infra`, `ops`, `security`. Product
  direction, stakeholder asks, feature scoping, PR reviews, and Jira decisions
  are rejected by construction — they belong in GitHub comments or Jira.
- `--refs` resolves each bead (any prefix, routed to its owning database) and
  pulls its title into the brief; an unknown ID fails the send. Bead IDs and
  titles are agent-written, so they are mrkdwn-escaped (`&`, `<`, `>`) before
  posting; the mayor's subject and body are rendered as written. After a
  successful post each referenced bead gets a `slack-forwarded:<UTC timestamp>`
  label (in its own database) as the audit trail.
- The mayor's operating rules (role template) require batching related
  escalations into one brief, waiting at least one check cycle or 30 minutes,
  and sending at most one brief per incident per hour.

Payload format (Slack mrkdwn):

```
🚨 *Gastown Ops Brief – 2026-10-10 14:05 UTC*
`[INFRA]` *CI pipeline down*
GitHub Actions runners offline since 09:40 UTC; 4 PRs blocked. Retried twice.

*References:*
• hq-abc — dolt-archive: dolt push failed for 1 remote(s)
• hq-def — Agent host disk 96% full

_Ack with: `gt escalate ack hq-abc hq-def`_
```

### gt escalate list

```bash
gt escalate list [--severity=...] [--stale] [--unacked] [--all] [--json]
```

### gt escalate stale

Re-escalate stale (unacked past `stale_threshold`) escalations. Bumps severity
(MEDIUM->HIGH->CRITICAL), re-executes route, respects `max_reescalations`.

```bash
gt escalate stale [--dry-run]
```

### gt escalate close

```bash
gt escalate close <bead-id> [--reason="Fixed in commit abc123"]
```

## Integration Points

### Plugin System

Plugins use escalation for failure notification:

```bash
gt escalate -s MEDIUM "Plugin FAILED: rebuild-gt" \
  -m "$ERROR" --source="plugin:rebuild-gt"
```

### Deacon Patrol

Deacon uses escalation for health issues:

```bash
if [ $unresponsive_cycles -ge 5 ]; then
  gt escalate -s HIGH "Witness unresponsive: gastown" \
    -m "Witness has been unresponsive for $unresponsive_cycles cycles" \
    --source="patrol:deacon:health-scan"
fi
```

Deacon patrol also runs `gt escalate stale` periodically to catch unacked
escalations and re-escalate them.

## When to Escalate

### Agents SHOULD escalate when:

- **System errors**: Database corruption, disk full, network failures
- **Security issues**: Unauthorized access attempts, credential exposure
- **Unresolvable conflicts**: Merge conflicts that cannot be auto-resolved
- **Ambiguous requirements**: Spec is unclear, multiple valid interpretations
- **Design decisions**: Architectural choices that need human judgment
- **Stuck loops**: Agent is stuck and cannot make progress
- **Gate timeouts**: Async conditions did not resolve in expected time

### Agents should NOT escalate for:

- **Normal workflow**: Regular work that can proceed without human input
- **Recoverable errors**: Transient failures that will auto-retry
- **Information queries**: Questions that can be answered from context

## Mayor Startup Check

On `gt prime`, Mayor displays pending escalations grouped by severity.
Action: review with `bd list --tag=escalation`, close with `bd close <id> --reason "..."`.


## Viewing Escalations

```bash
# List all open escalations
bd list --status=open --tag=escalation

# Filter by category
bd list --tag=escalation --tag=decision

# View specific escalation
bd show <escalation-id>

# Close resolved escalation
bd close <id> --reason "Resolved by fixing X"
```
