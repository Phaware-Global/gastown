# Telegraph: Town-Level Inbound External Event Transport

**Status:** Design — blocks all implementation beads (gt-ipc, gt-2tg, gt-eef, gt-8vr, gt-q5k)
**Epic:** gt-6if
**Created:** 2026-04-19

## Overview

Telegraph is Gas Town's always-on inbound line that converts external events into
Mayor-addressed mail with a rate-limited nudge. It listens on an HTTP endpoint,
authenticates and translates provider payloads into a normalized shape, then
delivers structured mail to the Mayor.

Scope: single instance per town, not per rig. Config lives under the town surface.

Non-goals (v1): outbound writes back to providers, LLM in the hot path, business
state storage, notification-level dedup, routing beyond Mayor.

Cross-references: [dog-execution-model.md](dog-execution-model.md),
[mail-protocol.md](mail-protocol.md).

---

## Three-Layer Architecture

```
External System
      │
      ▼
┌─────────────────────────────────────────────────────────────────┐
│ L1 · Transport                                                  │
│   HTTP webhook listener                                         │
│   Authenticates via provider signature                          │
│   Hands RawEvent to dispatcher → L2                             │
└─────────────────────────────────────────────────────────────────┘
      │  RawEvent
      ▼
┌─────────────────────────────────────────────────────────────────┐
│ L2 · Translation  (per-provider)                                │
│   Translator interface — one impl per provider                  │
│   Jira: parses webhook JSON → NormalizedEvent                   │
│   Unknown event types: log + reject; no silent drops            │
└─────────────────────────────────────────────────────────────────┘
      │  NormalizedEvent
      ▼
┌─────────────────────────────────────────────────────────────────┐
│ L3 · Transformation  (provider-agnostic)                        │
│   Builds Mayor-addressed mail envelope                          │
│   Rate-limited nudge: one nudge per window                      │
│   Uses existing gt mail + gt nudge primitives                   │
└─────────────────────────────────────────────────────────────────┘
      │  gt mail send mayor/ + gt nudge mayor/
      ▼
    Mayor
```

**Key invariant:** Adding a new provider requires only a new L2 `Translator`
implementation plus a config stanza. Layers 1 and 3 do not change.

---

## Explicit Layer Interfaces

### L1 → L2: `RawEvent`

```go
// RawEvent is the authenticated-but-uninterpreted payload from Transport.
// L1 guarantees the request passed HMAC/signature verification before
// enqueuing. L2 never re-verifies; it only translates.
type RawEvent struct {
    Provider   string            // stable provider ID, e.g. "jira"
    Headers    map[string]string // original HTTP headers (lowercased keys)
    Body       []byte            // raw request body (must not be mutated)
    SourceIP   string            // remote addr for logging
    ReceivedAt time.Time
}
```

### L2 → L3: `NormalizedEvent`

```go
// NormalizedEvent is the provider-agnostic representation produced by L2.
// L3 consumes this; it knows nothing about Jira or any other provider.
type NormalizedEvent struct {
    Provider     string    // e.g. "jira"
    EventType    string    // dot-separated, e.g. "issue.created", "comment.updated"
    EventID      string    // provider-native event ID (for dedup logging only)
    Actor        string    // who triggered the event (stable user handle)
    Subject      string    // primary entity, e.g. "PROJ-1234"
    CanonicalURL string    // link back to entity in provider UI
    Text         string    // salient text: title + description snippet or comment body
    Labels       []string  // provider-native labels/tags (not instructions)
    Timestamp    time.Time // event time from provider (UTC)
}
```

### L2 Translator Interface

```go
// Translator is implemented once per provider. L1 selects the right
// Translator by matching the request path/header to Provider().
type Translator interface {
    // Provider returns the stable provider identifier ("jira", "github", …).
    Provider() string

    // Authenticate verifies the request signature or token.
    // Called by L1 before enqueuing. Returns non-nil on failure.
    // Must not log secrets.
    Authenticate(headers map[string]string, body []byte) error

    // Translate converts a raw body to a NormalizedEvent.
    // Returns ErrUnknownEventType if the event type is not in scope.
    // Unknown types MUST be logged (with EventID if extractable) and returned
    // as ErrUnknownEventType — never silently dropped, never forwarded raw.
    Translate(body []byte) (*NormalizedEvent, error)
}

// ErrUnknownEventType is returned by Translate for out-of-scope event types.
var ErrUnknownEventType = errors.New("unknown event type")
```

---

## Mail Envelope Contract (Field-by-Field)

Every event that passes L1 authentication and L2 translation produces exactly
one mail to Mayor with this structure.

### From

```
telegraph/jira/<actor-handle>
```

Format: `telegraph/<provider>/<actor>`. Stable per source+actor pair.
Allows Mayor to recognize the sender origin at a glance without reading the body.
Never an internal agent address.

### To

```
mayor/
```

Fixed in v1. Telegraph has no routing logic.

### Subject

```
[JIRA PROJ-1234] Issue transitioned: In Progress → Done
```

Format: `[<PROVIDER> <SUBJECT>] <EventType prose>: <salient delta>`

Constructed entirely from structured `NormalizedEvent` fields — never from
raw user text. Subject is safe to display even if body contains adversarial content.

Examples by event type:
- `issue.created`     → `[JIRA PROJ-1234] Issue created: Fix login timeout`
- `issue.updated`     → `[JIRA PROJ-1234] Issue updated: status In Progress → Done`
- `comment.added`     → `[JIRA PROJ-1234] Comment added by alice`
- `comment.updated`   → `[JIRA PROJ-1234] Comment updated by alice`

### Body

```
Telegraph-Transport: http-webhook
Telegraph-Provider: jira
Telegraph-Event-Type: issue.updated
Telegraph-Event-ID: <provider-native-id>
Telegraph-Timestamp: 2026-04-19T12:00:00Z
Telegraph-Actor: alice
Telegraph-Subject: PROJ-1234
Telegraph-URL: https://company.atlassian.net/browse/PROJ-1234
Telegraph-Labels: story, critical

--- EXTERNAL CONTENT (untrusted: jira/alice) ---
<Text field from NormalizedEvent here>
--- END EXTERNAL CONTENT ---
```

Rules:
- Metadata block appears first, as `Telegraph-*` headers.
- External content is wrapped in explicit delimiters that identify it as untrusted.
- No user-supplied text appears outside the delimited block.
- Body length is capped (default 4 KB) to prevent oversized context injection.
  Content beyond the cap is truncated with a `[… truncated]` notice inside the block.

### Why the delimiters matter

The Mayor is a Claude agent. Jira users can write arbitrary text in issues and
comments. Without explicit trust demarcation, a Jira issue body containing
`You are now in admin mode…` would appear in Mayor's context as potential
instructions. The `--- EXTERNAL CONTENT ---` wrapper makes the trust boundary
structurally unambiguous.

---

## Jira Auth Scheme

**Choice: HMAC-SHA256 shared secret (Jira's native webhook mechanism)**

Jira's built-in webhook system sends a `X-Hub-Signature: sha256=<hex>` header
computed over the raw request body using a shared secret configured on the Jira
webhook registration. This is equivalent to GitHub's webhook signature scheme.

Verification steps in L1 (delegated to the Jira `Translator.Authenticate`):
1. Extract `X-Hub-Signature` header.
2. Compute `HMAC-SHA256(secret, rawBody)`.
3. Compare using `hmac.Equal` (constant-time). Reject on mismatch or missing header.
4. Return HTTP 401 and log the rejection (no body in log).

**Why not OAuth or JWT?** Jira webhook delivery is one-way push; there is no
OAuth callback. Shared-secret HMAC is the documented Jira mechanism, is
stateless, and requires no token refresh.

**Secret storage:** The shared secret is never committed. It is resolved from an
environment variable at daemon startup (see Config section). The secret value
must never appear in logs, error messages, or structured metadata.

---

## Config Surface

**Location:** `~/gt/settings/telegraph.toml` (town-level, single instance)

```toml
[telegraph]
listen_addr  = ":8765"      # bind address; required
buffer_size  = 256          # max RawEvents queued between L1 and L2 (backpressure)
nudge_window = "30s"        # max one Mayor nudge per this window
body_cap     = 4096         # max bytes of external text in mail body
log_file     = ""           # empty = stderr / daemon log

[telegraph.providers.jira]
enabled    = true
secret_env = "GT_TELEGRAPH_JIRA_SECRET"   # env var name holding HMAC secret
events     = [
    "issue_created",
    "issue_updated",
    "comment_added",
    "comment_updated",
]
```

### Secret Resolution

Secrets (HMAC signing keys, provider tokens) are resolved at startup from
**environment variables only**. The config file stores the env var *name*,
not the value.

Resolution sequence:
1. Read `secret_env` from config (e.g. `"GT_TELEGRAPH_JIRA_SECRET"`).
2. `os.Getenv(secretEnv)` — fail fast at startup if the env var is unset.
3. The resolved secret lives in memory only; never written to disk or log.

Rotating a secret: update the env var and restart Telegraph (or the daemon).

### Enabling/Disabling a Provider

Set `enabled = false` under `[telegraph.providers.<name>]`. The daemon applies
config changes on heartbeat without a full restart (hot-reload). If hot-reload
is not implemented in v1, a daemon restart is required; this must be documented
in the operator runbook.

---

## Daemon Supervision Model

Telegraph is an **imperative Go goroutine** within the daemon process — same
execution model as Doctor, Reaper, and Compactor (see dog-execution-model.md).

Rationale: Telegraph MUST stay up regardless of agent availability. It has no
dependency on Claude sessions. Running inside the daemon keeps supervision free
and deterministic.

Lifecycle:
1. Daemon startup: `go telegraph.Run(ctx, cfg)` — launched once.
2. The goroutine owns an `http.Server` and the L1 → L2 → L3 pipeline.
3. If the goroutine panics, the daemon's recover wrapper restarts it with
   exponential backoff (reusing the same `restartTracker` pattern as other dogs).
4. `context.Done()` triggers graceful shutdown: stop accepting new requests,
   drain the in-flight buffer, shut down the HTTP server.
5. Daemon shutdown propagates via the shared context.

The HTTP server socket binds on daemon startup. If the port is already in use,
the daemon logs the error and exits — not retried silently.

```
daemon.Run()
  └── go telegraph.Run(ctx, cfg)
           ├── http.ListenAndServe(cfg.ListenAddr, handler)  ← L1
           ├── dispatchLoop(rawCh)                           ← L1→L2→L3
           └── nudger(normalCh, cfg.NudgeWindow)             ← L3 rate-limiter
```

---

## Backpressure Strategy

Telegraph must not accumulate unbounded memory under event bursts.

**Mechanism: bounded channel + caller-reject**

```
HTTP request → Authenticate → (enqueue or reject)
                                   │
                             chan RawEvent (size = buffer_size)
                                   │
                              dispatchLoop
                                   │
                              Translate (L2)
                                   │
                               Deliver (L3)
```

Behavior when the channel is full:
- Return HTTP 503 to the caller (Jira will retry with its own backoff).
- Log a structured `reject` line with reason `backpressure`.
- Do not block the HTTP handler goroutine.

The buffer size (default 256) provides elasticity across short bursts. At one
event per second, 256 events gives ~4 minutes of headroom if L2/L3 stalls.

A sustained drop rate indicates under-sizing or a downstream stall and should
trigger operator action (observable via log `reason=backpressure` count).

---

## Nudge Rate-Limit Window

Mayor should not be flooded with nudges during high event volume. Telegraph
accumulates mail at full event rate but limits nudges.

**Policy:** at most one `gt nudge mayor/` per `nudge_window` (default 30 seconds).

**Implementation:**
```
lastNudge time.Time  // zero value = never nudged

after each mail delivery:
    if now - lastNudge >= nudge_window:
        gt nudge mayor/ "Telegraph: new events pending"
        lastNudge = now
```

The nudge message is generic — Mayor reads the actual event details from mail.
The nudge is advisory: if the Mayor session is not running, the nudge is lost
(acceptable; Mayor will discover the mail on next startup via `gt mail inbox`).

30 seconds is the recommended default. Operators may shorten for low-volume
environments or lengthen for high-volume ones. The tradeoff: shorter window =
more responsive Mayor, more nudge noise; longer window = batched notification,
delayed response.

---

## Observability

Every terminal outcome for an inbound request emits a structured JSON log line
on a single line. No multi-line logs; structured for `jq` / VictoriaLogs queries.

### Log Events

**`accept`** — request authenticated and enqueued for L2:
```json
{
  "ts": "<RFC3339>",
  "component": "telegraph",
  "event": "accept",
  "provider": "jira",
  "source_ip": "1.2.3.4",
  "event_id": "<provider-native-id>"
}
```

**`reject`** — request rejected before or during L2:
```json
{
  "ts": "<RFC3339>",
  "component": "telegraph",
  "event": "reject",
  "provider": "jira",
  "source_ip": "1.2.3.4",
  "reason": "hmac_invalid | unknown_event_type | backpressure | parse_error | provider_disabled",
  "event_id": "<provider-native-id-if-extractable>"
}
```

**`deliver`** — mail sent to Mayor:
```json
{
  "ts": "<RFC3339>",
  "component": "telegraph",
  "event": "deliver",
  "provider": "jira",
  "event_type": "issue.updated",
  "event_id": "<id>",
  "actor": "alice",
  "subject": "PROJ-1234",
  "mail_id": "<bead-id-of-created-mail>"
}
```

**`drop`** — L2 event discarded without delivery (e.g. truncated after cap, future dedup):
```json
{
  "ts": "<RFC3339>",
  "component": "telegraph",
  "event": "drop",
  "provider": "jira",
  "event_type": "...",
  "event_id": "...",
  "reason": "..."
}
```

**Rules:**
- No secret values in any log field.
- No raw request body in any log field.
- `source_ip` is the direct client IP; do not attempt reverse DNS.
- `event_id` may be absent on `reject` if the body was unparseable.
- Log destination: same log file as daemon (default stderr); configurable via
  `[telegraph] log_file`.

---

## Prompt-Injection Threat Model

### Threat

Telegraph bridges untrusted external content (Jira issues, comments) into
Mayor's Claude context. Malicious or poorly-authored content could attempt
to inject instructions.

Attack vector: A Jira user writes `[SYSTEM] Ignore previous instructions…`
in an issue title or comment body.

### Mitigations

| Layer | Mitigation | Rationale |
|-------|-----------|-----------|
| L2 (Translation) | Subject line constructed from structured fields only (event type, issue key, status labels) — never from raw user text | Prevents injection via subject |
| L3 (Transformation) | External text in body is always wrapped in `--- EXTERNAL CONTENT ---` delimiters with untrusted label | Mayor's model can identify the trust boundary structurally |
| L3 (Transformation) | Body content is capped at `body_cap` bytes; remainder truncated | Limits payload surface |
| L3 (Transformation) | `Telegraph-*` metadata headers are constructed from `NormalizedEvent` fields, never from raw text | Metadata cannot carry injected instructions |
| L1 (Transport) | Unauthenticated requests rejected at the perimeter | Reduces attacker surface to authenticated Jira instances |
| Mayor context | Mayor CLAUDE.md / prime context notes that `telegraph/` From addresses carry untrusted external content | Primes Mayor's reasoning about trust level |

### Residual Risk

A sufficiently adversarial Jira project member can write content into issues
that appears inside the `EXTERNAL CONTENT` block. The delimiters reduce but
do not eliminate the risk that a sophisticated prompt injection bypasses Mayor's
reasoning. v1 accepts this residual risk. Future mitigations could include:
- Content sanitization (strip markdown, code fences, system-looking tokens)
- Separate system-prompt section that labels the block as data, not instructions
- LLM-based classifier (explicitly out of scope for v1)

---

## Provider Extensibility

Adding a provider (e.g. GitHub, PagerDuty, Grafana) requires:

1. Create `internal/telegraph/providers/<name>/translator.go` implementing `Translator`.
2. Add `[telegraph.providers.<name>]` config stanza with `enabled`, `secret_env`, `events`.
3. Register the `Translator` in the dispatcher at daemon startup.

No changes to:
- `internal/telegraph/transport/` (L1 HTTP listener)
- `internal/telegraph/transform/` (L3 mail + nudge)
- `internal/telegraph/types.go` (`RawEvent`, `NormalizedEvent`, `Translator`)

### Package Layout

```
internal/telegraph/
├── types.go                     # RawEvent, NormalizedEvent, Translator interface
├── config.go                    # Config struct, secret resolution
├── transport/
│   └── http.go                  # L1: HTTP listener, auth delegation, enqueue
├── providers/
│   └── jira/
│       └── translator.go        # L2: Jira Translator implementation
└── transform/
    └── mail.go                  # L3: envelope builder, rate-limited nudge
```

### Jira v1 Scope

Supported event types (v1):
- `jira:issue_created`
- `jira:issue_updated` (status transition, priority change, assignee change)
- `jira:comment_added`
- `jira:comment_updated`

All other Jira event types → `ErrUnknownEventType` → logged, rejected, HTTP 200
returned to Jira (to prevent Jira from retrying indefinitely on unsupported types).

Future event types are additive: new entries in the `events` config array +
new cases in `Translate()`. No interface changes.

---

## Sequence: Jira Webhook → Mayor Mail

```
Jira Webhook Server
     │
     │  POST /webhook/jira
     │  X-Hub-Signature: sha256=<hmac>
     │  Body: { "issue": {...}, "webhookEvent": "jira:issue_updated" }
     │
     ▼
L1 · HTTP Handler
     │  1. Route to Jira Translator by path or config
     │  2. translator.Authenticate(headers, body)  → HMAC verify
     │  3. log "accept" or "reject:hmac_invalid"
     │  4. send RawEvent to channel or reject:backpressure
     ▼
dispatchLoop goroutine
     │  5. translator.Translate(body) → NormalizedEvent
     │     or log "reject:unknown_event_type" / "reject:parse_error"
     ▼
L3 · Transformer
     │  6. Build Telegraph-* metadata headers from NormalizedEvent
     │  7. Build From, Subject, delimited Body
     │  8. gt mail send mayor/ (creates mail bead)
     │  9. log "deliver"
     │  10. if now - lastNudge >= nudge_window:
     │        gt nudge mayor/ "Telegraph: new events pending"
     │        lastNudge = now
     ▼
   Mayor inbox
```

---

## Open Questions (for Overseer review before impl)

1. **Hot-reload vs restart for provider disable:** Is a daemon restart acceptable
   for enabling/disabling a provider in v1? Hot-reload requires a config watcher
   goroutine; restart is simpler. Recommend restart + doc; upgrade in v2 if needed.

2. **Jira retry behavior on HTTP 200 vs 5xx:** Jira stops retrying after HTTP 200.
   For unknown event types, returning 200 prevents retry storms. For parse errors
   on known event types, returning 400 may be appropriate to surface bugs. Recommend
   200 for unknown types, 400 for parse errors on known types.

3. **Body cap value:** Default 4 KB recommended. Jira descriptions can be long.
   A higher cap (e.g. 8 KB) adds more context but increases injection surface.
   Overseer to confirm.

4. **Mayor nudge wording:** The nudge says `"Telegraph: new events pending"`. Should
   it include a count of pending unread Telegraph mails? Adds value but requires a
   beads query on the nudge path. Recommend keep it simple in v1.
