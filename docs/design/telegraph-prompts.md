# Telegraph: Operator-Side Event Handling — Prompts + Actor Filtering

**Status:** Draft proposal
**Companions:** [docs/design/telegraph.md](telegraph.md) · [docs/runbooks/telegraph.md](../runbooks/telegraph.md)

This doc specs two related operator-side controls layered over the existing Telegraph L1→L2→L3 pipeline:

1. **Per-event-type operator prompts** (sections below up to the prompts acceptance criteria) — operator-supplied trusted text attached to Mayor mail to frame how the receiving agent should interpret each event class.
2. **Actor filtering for self-echo prevention** (companion section after the prompts spec) — operator-supplied list of event actors whose webhooks Telegraph silently drops at L2, preventing the feedback loop where Mayor's own outbound Jira comments fire webhooks that come back to Mayor's mailbox.

Both features share a deployment surface (config in `telegraph.toml`, opt-in via explicit operator action, no behavior change for existing deployments without config edits) and a trust class (operator-controlled config, sanitized against webhook-payload-derived inputs). They're spec'd in one PR because reviewing them as a unit lets reviewers catch any interaction risks early — e.g., does an actor-filtered event produce a misleading `prompt_key` log line? (No — actor filtering happens before prompt resolution, the filtered event never reaches L3.)

## Problem

Today, Telegraph normalizes external webhook events into Mayor mail with the structure:

```
Telegraph-Transport: http-webhook
Telegraph-Provider: jira
Telegraph-Event-Type: comment.added
Telegraph-Subject: PROJ-1234
... (more metadata headers)

--- EXTERNAL CONTENT (untrusted: jira/Artie) ---
<the actual comment body, issue summary, etc.>
--- END EXTERNAL CONTENT ---
```

The receiving LLM (Mayor, or any agent further downstream) gets the *facts* of what happened — actor, subject, event type, body — but has to derive its own interpretation of "what does the operator want me to do about this kind of event?" The receiving agent is left to infer intent from event type alone.

This works adequately for the simple cases ("an issue was created, acknowledge it"), but the moment routing becomes nuanced — "if this comment is a question directed at the assignee, draft a reply via the Atlassian MCP; if it's a directive about a code change, open a bead and dispatch a polecat; if it's purely conversational, drop it" — every receiver has to encode the same operator policy in its own prompt. That policy is operator-supplied configuration, not LLM training data, and it changes faster than receiver prompts get updated.

We want a single place where the operator can write **per-event-type framing prompts** that flow into the mail body alongside the external content, telling the receiving agent how to interpret events of that shape and what response actions are reasonable.

## Goals

1. Operator can configure a prompt per `<provider>:<event-type>` pair (e.g. `jira:comment.added`, `jira:issue.created`).
2. The prompt is delivered with every mail produced from that event type, sitting between the structured Telegraph headers and the external-content block — i.e. visible to the receiving agent without having to look elsewhere.
3. The trust boundary between operator-supplied prompt (trusted) and webhook-supplied content (untrusted) remains explicit and structurally enforced via delimiter blocks.
4. Operators can edit the prompt config and pick up changes by restarting Telegraph (no hot reload in v1).
5. Variable substitution from NormalizedEvent fields (`{actor}`, `{subject}`, `{canonical_url}`, etc.) so prompts can name the specific entity in question, not just describe the event class.
6. A `default` prompt covers event types without a specific entry; both omission and the absence of a default are valid (no OPERATOR PROMPT block emitted).

## Non-goals

- **Conditional logic / templating engine inside prompts.** Plain string substitution only. If you need branching, write multiple sentences or move the logic into the receiving agent.
- **Per-recipient prompts.** Telegraph mails go to a single hardcoded mailbox (`mayor/`) regardless of audience; recipient-specific framing is Mayor's job downstream, not Telegraph's.
- **Per-rig prompts.** Same reason — rig-aware routing is Mayor's responsibility. Mayor can layer rig-specific framing on top when forwarding mail.
- **Cross-provider inheritance.** No "every comment-ish event in any provider gets this fragment." Prompts are scoped per `<provider>:<event-type>` plus a single global `default`.
- **Hot reload / SIGHUP.** Defer until operators are actively iterating; v1 requires a Telegraph restart.
- **Per-event metrics tagging the resolved prompt key.** Would be useful for A/B analysis later; out of scope for v1.

## Design

### Mail body shape (with a configured prompt)

```
Telegraph-Transport: http-webhook
Telegraph-Provider: jira
Telegraph-Event-Type: comment.added
Telegraph-Event-ID: 22360
Telegraph-Timestamp: 2026-04-26T23:16:54Z
Telegraph-Actor: Artie
Telegraph-Subject: PROJ-1234
Telegraph-URL: https://phaware.atlassian.net/browse/PROJ-1234
Telegraph-Labels: ...

--- OPERATOR PROMPT (trusted) ---
A new comment was added to PROJ-1234 by Artie.
URL: https://phaware.atlassian.net/browse/PROJ-1234

If the comment is a direct question or @-mention, respond inline via
the addCommentToJiraIssue MCP tool. If it's a directive about a code
change, open a bead and dispatch a polecat. If it's purely
conversational, no action is needed.
--- END OPERATOR PROMPT ---

--- EXTERNAL CONTENT (untrusted: jira/Artie) ---
<the actual comment body>
--- END EXTERNAL CONTENT ---
```

Two structurally-distinct delimited blocks:

- **OPERATOR PROMPT (trusted)** — operator-supplied text from `telegraph.toml`. The receiving LLM can be told to follow these instructions.
- **EXTERNAL CONTENT (untrusted: \<provider\>/\<actor\>)** — webhook payload text. The receiving LLM should interpret this as data, never as instructions.

If no prompt is configured for the event type and no `default` is set, the OPERATOR PROMPT block is **omitted entirely** — the mail body collapses back to today's shape. Existing deployments see no behavior change until the operator opts in.

### Configuration

Two equivalent surfaces, in lookup order:

**Inline in `telegraph.toml`:**

```toml
[telegraph]
listen_addr  = "127.0.0.1:8765"
buffer_size  = 256
nudge_window = "30s"
body_cap     = 4096
prompt_cap   = 2048   # NEW: max bytes for a single resolved prompt; truncate over

[telegraph.prompts]
default = """
A telegraph event arrived from {provider} ({event_type}) involving {subject}.
Read the external content and decide whether action is required.
"""

"jira:comment.added" = """
A new comment was added to {subject} by {actor}.
URL: {canonical_url}

If the comment is a direct question or @-mention, respond inline via
the addCommentToJiraIssue MCP tool. If it's a directive about a code
change, open a bead and dispatch a polecat. If it's purely
conversational, no action is needed.
"""

"jira:issue.created" = """..."""
"jira:issue.updated" = """..."""
"jira:comment.updated" = """..."""
```

**Or in a separate file `~/gt/settings/telegraph.prompts.toml`:**

Same `[telegraph.prompts]` table at the top level. Loaded only if the file exists. **If both surfaces define the same key, the separate file wins** — this lets operators iterate on prompts in a smaller, more diff-friendly file without churning the main config.

#### Why event-type names are post-translation

Keys use `jira:comment.added` (the NormalizedEvent EventType, dotted), not `jira:comment_added` or `comment_created` (the wire-format strings). This decouples prompt config from provider webhook quirks like the bare-name comment events Jira emits — operators write prompts against Telegraph's stable internal vocabulary, not Atlassian's wire format.

The full set for v1, matching what the Jira translator currently emits:

| Key | Fires on |
|---|---|
| `jira:issue.created` | Issue creation |
| `jira:issue.updated` | Issue field change (status, assignee, summary, etc.) |
| `jira:comment.added` | New comment |
| `jira:comment.updated` | Comment edited |

### Variable substitution

Tokens replaced verbatim before the prompt is written into the mail body:

| Token | NormalizedEvent field | Empty-field behavior |
|---|---|---|
| `{provider}` | `Provider` | "" |
| `{event_type}` | `EventType` | "" |
| `{event_id}` | `EventID` | "" |
| `{actor}` | `Actor` | "" |
| `{subject}` | `Subject` | "" |
| `{canonical_url}` | `CanonicalURL` | "" |
| `{timestamp}` | `Timestamp.UTC().Format(time.RFC3339)` (Go's `time.RFC3339` constant, e.g. `2026-04-26T23:16:54Z`) | "" |
| `{labels}` | `Labels` joined with `, ` (each element CR/LF-stripped) | "" |

**Non-string fields require a defined serialization.** `{labels}` is the only multi-valued field exposed in v1; it renders as a comma-space-joined string (e.g. `bug, critical, security`). Each element passes through CR/LF stripping individually before joining so a maliciously-crafted label can't break out of the prompt block. If a future field needs serialization (e.g. an array of users), the spec for that field must define its rendering before being added to the substitution table.

Substitution is plain string replacement — no escaping, no recursion, no expressions. Unknown tokens (e.g. `{foo}`) are left as literal text; this lets operators include literal braces in prose without escaping. Empty fields collapse to empty strings rather than leaving the literal token.

**Known limitation: literal known-token text.** Because substitution is unconditional, a prompt cannot include the literal string `{actor}` (or any other defined token) as prose — it will always substitute. v1 does not provide an escape mechanism (no `\{actor\}` or `{{actor}}` syntax). Operators who need to talk *about* a token rather than substitute it should reword the prompt ("the comment author" instead of "the {actor} placeholder"). If this becomes a real need, a future revision can introduce an escape syntax; the cost of leaving it out of v1 is small because operator-authored prompts rarely need to discuss the substitution mechanism itself.

**Substituted values pass through value-sanitization** before being inserted into the prompt block:

1. CR/LF characters are stripped (same as existing `sanitizeHeaderValue` for Telegraph metadata headers) — prevents a crafted issue title from injecting newline-delimited fake delimiter lines.
2. The full sanitized value is then checked against the literal start and end delimiters of the OPERATOR PROMPT block. If a sanitized value matches `--- OPERATOR PROMPT (trusted) ---` or `--- END OPERATOR PROMPT ---` exactly (after trimming surrounding whitespace), substitution replaces it with the literal string `[redacted: delimiter spoof]` and a WARN is logged. CR/LF stripping alone is insufficient — an attacker who controls a Jira label or display name could craft the exact delimiter string on a single line, and substitution would otherwise drop it verbatim into the trusted block.

### Resolution order

For a NormalizedEvent with `Provider="jira"`, `EventType="comment.added"`:

1. Look up exact key `"jira:comment.added"` → use that template if present.
2. Fall back to `default` key → use that template if present.
3. Otherwise → emit no OPERATOR PROMPT block (mail body shape collapses to current behavior).

### Length cap

`prompt_cap` (default 2048 bytes) bounds the **substituted prompt text before the truncation marker is appended**. Prompts whose post-substitution length exceeds the cap are sliced to `prompt_cap` bytes (rune-bounded, see below), then the literal marker `\n[… prompt truncated]` is appended on top, and a warning is logged at L3. This matches the existing `body_cap` convention in `internal/telegraph/transform/mail.go` (line 207: `text[:t.bodyCap] + "\n[… truncated]"`) so operators have a single mental model for how Telegraph's size limits compose. The actual emitted prompt-block size is therefore `min(len(prompt), prompt_cap) + len(marker)` — operators sizing mail-body budgets should account for the marker length (~24 bytes) being added on top of `prompt_cap` when truncation fires.

The cap is per-mail, not per-config-entry — relevant when a `{canonical_url}` or `{subject}` substitution unexpectedly inflates the rendered length.

**Truncation is rune-bounded, not byte-bounded.** A naive byte-slice at `prompt_cap` could split a multi-byte UTF-8 sequence (any non-ASCII actor name, label, or comment URL slug) and emit invalid UTF-8 into the mail body. The implementation must scan back from the cap to the nearest rune boundary before truncating — same convention already used by `safeTitle` in `internal/telegraph/transform/mail.go` for the subject line. The cap is documented in bytes (not runes) because operators reason about mail body size in bytes, but the slicing operation respects rune boundaries.

### Code organization

New package `internal/telegraph/prompts`:

```go
package prompts

type Config struct {
    Default string
    ByKey   map[string]string  // "jira:comment.added" → template
    Cap     int                // 0 = no cap
}

type Resolver struct { /* ... */ }

func NewResolver(c Config) (*Resolver, error)

// Resolve returns the rendered prompt for an event, or "" if no prompt
// is configured (neither exact match nor default). The returned string
// is post-substitution, post-sanitization, post-truncation.
func (r *Resolver) Resolve(event *telegraph.NormalizedEvent) string
```

Wired into the existing L3 `Transformer`:

```go
func NewProduction(
    townRoot string,
    bodyCap int,
    nudgeWindow time.Duration,
    resolver *prompts.Resolver,  // NEW; nil disables prompt blocks
    logger *tlog.Logger,
) *Transformer
```

`buildBody` (`internal/telegraph/transform/mail.go`) emits the OPERATOR PROMPT block between the existing metadata-header section and the EXTERNAL CONTENT block when `t.resolver != nil` and `t.resolver.Resolve(event) != ""`.

### Startup behavior

Telegraph at startup:

1. Parses `[telegraph.prompts]` from the main config and `~/gt/settings/telegraph.prompts.toml` (if present), merging with the separate-file taking precedence on key collision.
2. Validates each key (**other than the literal string `default`**, which is a special-cased fallback key and is exempt from the regex) matches `^[a-z]+:[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]+)+$` — provider segment, colon, then a dotted event-type with at least two segments. Allows multi-segment vocabularies (`jira:issue.field.changed`, `github:pull_request.review.submitted`) that the previous draft's single-dot regex would have rejected. Underscores allowed within segments to match existing event names like `pull_request`. Invalid keys → exit at startup with a readable error. The `default` exemption is structural: that key is a sentinel for the resolver's fallback path, not a `<provider>:<event-type>` mapping, and the validation regex was never meant to apply to it.
3. Validates each prompt value is non-empty after trimming. Empty strings → exit at startup.
4. Validates each prompt template **does not contain either delimiter** of the OPERATOR PROMPT block — neither the literal start `--- OPERATOR PROMPT (trusted) ---` nor the literal end `--- END OPERATOR PROMPT ---`. The end-delimiter check is the obvious case (it would close the trust boundary mid-prompt); the start-delimiter check is the symmetrical paranoia case (a substituted value or hand-edited template containing the start delimiter could let a future template-aware tool re-open a closed trust boundary if the receiver does string-based parsing). Both checks are cheap. If either is present, refuse to start.
5. Logs at INFO the count of registered prompt keys plus whether a `default` was configured: `[Telegraph] prompts loaded: 4 specific, default=true`.

### Trust model

- **Prompts are operator-controlled.** They live in operator-managed config files (`telegraph.toml`, `telegraph.prompts.toml`), same trust class as the rest of Telegraph's configuration. No user input, webhook payload, or LLM output can write to them.
- **Substituted variables are untrusted.** They come from NormalizedEvent fields, which themselves derive from webhook payloads. We sanitize them on the way into the prompt block (CR/LF stripping) so a crafted issue title can't fake a delimiter line. The receiving LLM should still treat substituted values as data, even within the prompt block — same caution that already applies to anything in the Telegraph-* metadata headers.
- **The trust delimiter is structural.** The receiving LLM is told (in its own system prompt) to treat content between `--- OPERATOR PROMPT (trusted) ---` and `--- END OPERATOR PROMPT ---` as instructions, and content between `--- EXTERNAL CONTENT (untrusted: …) ---` and `--- END EXTERNAL CONTENT ---` as data. Spoofing those delimiters is the attack surface; closing it requires three layers in concert: (a) startup-time rejection of templates containing either delimiter (Startup Behavior step 4), (b) CR/LF stripping on substituted values (so a multi-line label can't smuggle a fake delimiter line), and (c) exact-match rejection of substituted values that equal a delimiter literal even on a single line (so a one-line label of exactly `--- END OPERATOR PROMPT ---` is replaced with `[redacted: delimiter spoof]`). The combination handles both the multi-line and the single-line attack shapes.

## Failure modes

| Condition | Telegraph behavior | Receiver behavior |
|---|---|---|
| No `[telegraph.prompts]` configured | Resolver returns "" for every event | Mail body unchanged from today |
| Prompt for this event type, no `default` | Resolved prompt rendered into block | Receives operator framing |
| No prompt for this event type, `default` set | Default template rendered into block | Receives generic framing |
| Prompt template longer than `prompt_cap` | Truncated with `[… prompt truncated]` marker, WARN logged | Receives clipped prompt |
| Substituted variable contains CR/LF | Stripped via sanitizeHeaderValue | Receives single-line value |
| Substituted variable equals a delimiter literal | Replaced with `[redacted: delimiter spoof]`, WARN logged | Sees the redaction marker, not the spoofed delimiter |
| Substituted UTF-8 string would be split mid-rune by truncation | Truncation backs up to nearest rune boundary | Receives valid UTF-8 |
| Config file has invalid prompt key syntax | Telegraph exits at startup with error | n/a (telegraph never started) |
| Config file has start- or end-delimiter inside prompt | Telegraph exits at startup with error | n/a |

## Migration / rollout

1. Ship v1 with the resolver + delimiters in place. **No default prompt and no per-event prompts in stock config.** Existing deployments behave exactly as today (no OPERATOR PROMPT block emitted) until the operator opts in by editing config.
2. Operators opt in by adding `[telegraph.prompts]` to `telegraph.toml` (or creating `~/gt/settings/telegraph.prompts.toml`) and restarting Telegraph.
3. The `gt down` / `gt up` cycle is the supported way to pick up prompt changes — same lifecycle as any other Telegraph config edit.
4. Add a starter `telegraph.prompts.toml.example` to the repo with commented templates for each Jira event type, modeling the prompt patterns we expect operators to want.

## Open questions

1. **Mayor-side override of Telegraph's prompt.** Should Mayor be able to *replace* or *append to* Telegraph's prompt on a per-rig basis when forwarding? Probably yes eventually — Mayor knows the destination rig and can add rig-specific framing. Defer to a follow-up; the v1 mail format already lets Mayor inspect the OPERATOR PROMPT block and rewrite it before re-mailing.
2. **Hot reload.** Adding a `gt telegraph reload` subcommand that re-parses prompts without bouncing the listener would be useful once operators are iterating on prompts. Requires care around in-flight events.
3. **Conditional / multi-shot prompts.** If "the right framing depends on whether the issue is in project X or Y" becomes a real need, the answer is probably to introduce a thin `WhenLabels` or `WhenSubject` matcher above the template — but that's well beyond v1.
4. **Prompt sanity-check tooling.** A `gt telegraph render-prompt --event-type=jira:comment.added --subject=TEST-1 ...` subcommand that prints what the resolved prompt would look like, without firing a real event, would speed up authoring. Probably worth shipping in v1 if cheap.
5. **Escape syntax for literal token text.** Currently a prompt cannot include literal `{actor}` (or any other defined token) as prose because substitution is unconditional. If operators report this as a real friction, a follow-up can add `{{actor}}` → literal `{actor}` semantics; not in v1.

## Acceptance criteria for v1

- `[telegraph.prompts]` parsed from main config + optional separate file, separate file wins on collision.
- Resolver returns the right template for exact match, falls back to `default`, returns `""` otherwise.
- `buildBody` emits the OPERATOR PROMPT block iff resolver returns non-empty.
- Variables (`{provider}`, `{event_type}`, `{event_id}`, `{actor}`, `{subject}`, `{canonical_url}`, `{timestamp}`, `{labels}`) substituted from NormalizedEvent; `{labels}` rendered as comma-space-joined with each element CR/LF-stripped; unknown tokens left literal; empty fields collapse to empty strings.
- Substituted values pass three-stage sanitization: CR/LF strip → exact-match rejection of OPERATOR PROMPT delimiter literals → final value inserted.
- Prompt cap enforced post-substitution with **rune-boundary truncation** (no mid-rune slicing).
- Startup-time validation rejects malformed keys (regex allows multi-segment dotted event-types), empty values, and templates containing **either** the OPERATOR PROMPT start or end delimiter.
- **Delivery log includes `prompt_key`.** The existing `event=deliver` structured log adds a `prompt_key` field naming which template resolved (`"jira:comment.added"`, `"default"`, or `""` if no prompt block was emitted). Promoted from "future addition" to v1 because debugging LLM responses without knowing which prompt fired forces operators to re-derive the resolution from event-type after the fact, and the field is two lines of code at the call site that already has the resolved prompt in scope.
- New tests covering: exact-key resolve, default fallback, empty-fallback, cap truncation at rune boundary (including a multi-byte UTF-8 input that would corrupt under naive byte-slicing), start-delimiter rejection, end-delimiter rejection, multi-segment key acceptance, variable substitution including empty fields, `{labels}` rendering with multi-element and empty inputs, CR/LF stripping in substituted values, exact-delimiter-match redaction in substituted values, `prompt_key` field present in delivery log.
- End-to-end smoke test (manual): trigger a real Jira comment, confirm the OPERATOR PROMPT block appears between metadata and external content with the expected substituted values, and confirm the `event=deliver` log line carries `prompt_key=jira:comment.added`.
- Runbook update at `docs/runbooks/telegraph.md` documenting the prompts config + restart-to-apply flow.

---

# Companion feature: Actor filtering for self-echo prevention

## Problem

When Mayor (or any agent operating as a configured "self" persona) posts a comment to Jira via the Atlassian MCP, the resulting webhook fires back to Telegraph and lands in Mayor's mailbox as if from an external user — even though Mayor's own action originated it. Worse, every Mayor outbound comment produces **two** webhooks: a `comment.added` event for the comment itself, plus an `issue.updated` event because the issue's lastUpdated timestamp moved. Both arrive with `actor=<operator persona>`. Mayor sees its own actions echoed back as new mail, processes them, and enters a feedback loop.

Filtering this at Jira's webhook config (JQL filter) is **not possible**: JQL is an issue-level query language, and the comment-author / event-actor field is not exposed to JQL. Even with a separate bot account for Mayor's outbound API calls, the JQL filter (e.g. `assignee = currentUser()`) still matches the issue when Mayor edits it, because the issue's assignee is unchanged. The filter must live downstream of Jira.

Telegraph already has the actor in `NormalizedEvent.Actor`. It's the natural place to drop self-echo events — sooner than Mayor's mail handler, with structured-log visibility, and without requiring every receiver to encode the same dedup logic.

## Goals

1. Operator can name one or more actor display-names whose events Telegraph silently drops at L2, before mail is produced and before Mayor is nudged.
2. Drops produce a structured audit log entry naming the filtered actor and event type, so it's clear what was silenced and why.
3. The filter is per-provider (Jira-specific in v1), defined alongside the existing provider config in `telegraph.toml`.
4. Existing deployments without `ignore_actors` configured see no behavior change.

## Non-goals

- **Pattern matching on actor names** (regex, glob, prefix). Single-string exact-match is enough for the dogfood case; if multiple operators share a town, list them explicitly. Pattern matching can be added later without breaking the v1 schema.
- **Auto-derivation of "self" from MCP credentials or environment.** Operator names the actor explicitly. "Telegraph automatically knows it's me" couples Telegraph to outbound infrastructure it shouldn't know about, and would mis-fire if the operator persona's display name differs from the API caller's identity.
- **Subject-based filtering.** JQL handles "which issues fire" upstream of Telegraph; that's the right layer for issue-scope filtering. This feature is exclusively about the actor field.
- **Cross-provider actor filtering.** Each provider's actor field has different semantics; the GitHub equivalent (when added) gets its own `ignore_actors` under `[telegraph.providers.github]`.

## Configuration

Extend the existing `[telegraph.providers.jira]` block with a new `ignore_actors` field:

```toml
[telegraph.providers.jira]
enabled       = true
secret_env    = "GT_TELEGRAPH_JIRA_SECRET"
events        = [
    "jira:issue_created",
    "jira:issue_updated",
    "jira:comment_added",
    "jira:comment_updated",
]
ignore_actors = ["Artie"]   # NEW — drop events whose actor exactly matches any entry
```

`ignore_actors` accepts a list of strings. Empty list (or absent) means no actor filtering — current behavior. Strings are compared **case-sensitively** against `NormalizedEvent.Actor` for exact equality. No partial matches, no regex, no whitespace trimming during comparison (config-load can trim entries; runtime comparison is exact against the post-translation Actor field).

## Where the check happens

After successful authentication and translation, but before the event is enqueued to L3:

```
L1 receives request
  → Authenticate (existing — drops with reason=hmac_invalid on failure)
  → Translate to NormalizedEvent (existing — drops with reason=unknown_event_type on unknown webhookEvent)
  → if event.Actor matches an entry in providerConfig.IgnoreActors:
        log Drop(reason="actor_filtered", event_type, event_id, actor)
        return  // silently dropped, no L3 mail produced, no Mayor nudge
  → enqueue to L3 (existing)
```

The check sits in the same place as the existing `unknown_event_type` drop, just one rung down. The semantic distinction is intentional: `unknown_event_type` is a Telegraph gap (we don't know how to translate this), while `actor_filtered` is an operator decision (we know how, but the operator said don't deliver). Both produce `event=drop` lines, but with different `reason` codes so dashboards can distinguish "Telegraph needs a code update" from "Telegraph is doing what the operator asked."

## New reason code

Add `ReasonActorFiltered = "actor_filtered"` to the existing constants in `internal/telegraph/tlog/logger.go`:

```go
const (
    ReasonHMACInvalid       = "hmac_invalid"
    ReasonUnknownEventType  = "unknown_event_type"
    ReasonParseError        = "parse_error"
    ReasonBackpressure      = "backpressure"
    ReasonProviderDisabled  = "provider_disabled"
    ReasonActorFiltered     = "actor_filtered"   // NEW
)
```

The structured log line on a filtered event:

```json
{
  "ts": "2026-04-26T23:30:14Z",
  "component": "telegraph",
  "event": "drop",
  "provider": "jira",
  "event_type": "comment.added",
  "event_id": "22360",
  "actor": "Artie",
  "reason": "actor_filtered"
}
```

The `actor` field is included in this drop log — even though it's not part of the generic `Drop()` signature today — because audit trail of "who got filtered" is the entire value of the feature. The `Drop()` helper in `tlog/logger.go` gains an `actor` parameter (passed empty string for non-actor-filtered drop reasons to preserve current log shape for those reasons).

## Code organization

In `internal/telegraph/providers/jira/translator.go`, the Translator gains an `ignoreActors map[string]struct{}` set populated at construction:

```go
func New(secret string, ignoreActors []string, logger *slog.Logger) *Translator {
    set := make(map[string]struct{}, len(ignoreActors))
    for _, a := range ignoreActors {
        set[a] = struct{}{}
    }
    return &Translator{
        secret:       []byte(secret),
        ignoreActors: set,
        logger:       logger,
    }
}
```

After translation succeeds, before returning the NormalizedEvent:

```go
evt, err := translateXxx(&p)
if err != nil {
    return nil, err
}
if _, filtered := t.ignoreActors[evt.Actor]; filtered {
    // Return the event alongside the sentinel — the L1 handler needs
    // evt.Actor / evt.EventType / evt.EventID to populate the audit-log
    // line. The L1 handler treats (evt != nil, err == ErrActorFiltered)
    // as "drop this, don't enqueue, but log with full event metadata."
    return evt, ErrActorFiltered
}
return evt, nil
```

**Why return `(evt, ErrActorFiltered)` rather than `(nil, ErrActorFiltered)`:** the L1 handler is the single source of truth for `event=drop` log lines (it's where every other drop reason is logged today, including `unknown_event_type`, `parse_error`, `backpressure`). To keep that locality of audit-logging while still routing the actor field into the structured log, the translator must hand the event back to L1 alongside the sentinel. L1 reads the err first; on `ErrActorFiltered` it logs `Drop(reason="actor_filtered", event_type=evt.EventType, event_id=evt.EventID, actor=evt.Actor)` and short-circuits before enqueue. On any other error (or no error), the existing logic applies. Tests covering this contract: (a) translator returns non-nil event when filtered, (b) L1 handler logs the actor field on filtered drop, (c) L1 handler does not enqueue the filtered event to L3.

New sentinel error `ErrActorFiltered` defined in `internal/telegraph/types.go` alongside `ErrUnknownEventType`:

```go
var ErrActorFiltered = errors.New("actor filtered by provider config")
```

The L1 HTTP handler (`internal/telegraph/transport/http.go`) maps `ErrActorFiltered` to `Drop(reason="actor_filtered", actor=evt.Actor, event_type=evt.EventType, event_id=evt.EventID)` — same shape as the existing `ErrUnknownEventType` mapping but populated from the event the translator returned. **HTTP response remains 200 OK** to the caller (Jira), same convention as `unknown_event_type` — non-200 would trigger Jira's webhook-retry logic and produce repeated drops for events the operator already said to ignore.

## Failure modes / interactions

| Condition | Telegraph behavior | Receiver behavior |
|---|---|---|
| Actor not in `ignore_actors` list | Event flows normally to L3 | Receives mail as today |
| Actor in `ignore_actors` list | `event=drop, reason=actor_filtered` logged with actor field | No Mayor mail, no nudge |
| `ignore_actors` empty or absent | No filtering applied | No behavior change from today |
| Actor field empty (e.g. translator couldn't extract) | Treated as not-matching (empty string never matches a non-empty list entry) | Event flows through |
| `ignore_actors` contains an empty string `""` | Config-load rejects with error: empty entries are meaningless | Telegraph never started |
| Same comment fires both `comment.added` AND `issue.updated` from same actor | Both events are filtered (both events have actor=<persona>) | Both echoes silenced |
| Operator persona renamed in Jira (display name change) | Old name no longer matches; events flow through until config updated | Echoes resume until operator updates `ignore_actors` |

## Trust model

- `ignore_actors` is operator-controlled config, same trust class as the rest of `[telegraph.providers.jira]`.
- The filter trusts `NormalizedEvent.Actor`, which derives from webhook payload. An attacker who can spoof the actor field (i.e., compromise Jira) could either bypass the filter (events from a different actor) or grief by impersonating the operator persona (events disappear). HMAC verification gates payload integrity at L1; if HMAC is broken, this filter is the least of your concerns.
- Filtering is **silent at the receiver** — no Mayor mail, no nudge. The audit trail lives only in `~/gt/logs/telegraph.log`. Operators who need to detect "is my filter masking real events I should have seen?" should sample the log periodically. Adding a counter for actor-filtered events is a v2 polish item if it becomes worth tracking.
- The filter does NOT interact with the prompts feature: actor filtering happens at L2, prompt resolution happens at L3 inside `buildBody`. A filtered event never reaches `buildBody`, so the `prompt_key` log field is naturally absent (the entire `event=deliver` line is absent — there's only an `event=drop` line). Reviewers should confirm this ordering when implementing both features in tandem.

## Acceptance criteria for v1

- `ignore_actors` parsed from `[telegraph.providers.jira]` as a list of strings; empty/absent means no filtering.
- Config-load rejects empty-string entries in the list (`ignore_actors = ["Artie", ""]` → error at startup).
- Translator's `New(...)` constructor accepts the list, builds an internal set for O(1) lookup.
- After successful translation, translator checks `evt.Actor` against the set and returns new `ErrActorFiltered` sentinel on match.
- L1 HTTP handler maps `ErrActorFiltered` to `Drop(reason="actor_filtered", actor=evt.Actor)` log line; HTTP response stays 200.
- New `ReasonActorFiltered` constant added to `tlog/logger.go`; `Drop()` helper gains `actor` parameter (passed empty string for existing reasons to preserve current log shape).
- New tests covering: empty `ignore_actors` list (no drop), set with non-matching actor (no drop), set with matching actor (drop on `comment.added`), drop on `issue.updated` from same actor (verifies both echo paths are closed), case-sensitivity (mixed-case actor with lowercase entry → no drop), HTTP response stays 200 on filtered drop, structured log line includes the `actor` field.
- End-to-end smoke test (manual): set `ignore_actors = ["Artie"]` in `~/gt/settings/telegraph.toml`, restart Telegraph, post a comment to a Jira issue assigned to Artie via the Atlassian MCP, confirm `~/gt/logs/telegraph.log` shows two `event=drop, reason=actor_filtered` entries (one for `comment.added`, one for `issue.updated`) and Mayor mailbox is unchanged.
- Runbook update at `docs/runbooks/telegraph.md` documenting the filter, why it exists (self-echo prevention from Mayor's outbound Jira API calls), and the audit-log expectation.

## Open questions (actor filtering)

1. **Filter on `Actor` (display name) vs on the underlying `user.name` (Atlassian login).** Display names can be edited in Atlassian profiles; `user.name` (account ID or login) is more stable. v1 uses display name to match the existing `Telegraph-Actor` header convention and the dogfood instance's data. If operators report drift, an `ignore_actor_ids` companion field is a clean future addition that lookup-checks the same set against the more stable identifier.
2. **Counters for filtered events.** `tlog.Counters` already has slots for reject reasons; a `Counters.ActorFiltered` increment is two lines and would let operators quickly answer "how many events am I dropping per hour?" without grep. Not in v1 but flagged as cheap polish.
3. **Cross-provider unification.** When the GitHub provider lands, the same pattern (`[telegraph.providers.github]` with its own `ignore_actors`) is the right shape. Worth pinning the convention now via this Jira implementation rather than retrofitting later.
