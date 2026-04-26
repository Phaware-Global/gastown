# Telegraph: Configurable Per-Event-Type Operator Prompts

**Status:** Draft proposal
**Companions:** [docs/design/telegraph.md](telegraph.md) · [docs/runbooks/telegraph.md](../runbooks/telegraph.md)

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
| `{timestamp}` | `Timestamp.UTC().Format(RFC3339)` | "" |
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

`prompt_cap` (default 2048 bytes) bounds the resolved prompt text after variable substitution. Prompts that exceed the cap are truncated with `\n[… prompt truncated]` and a warning is logged at L3 (parallel to the existing `body_cap` behavior for external content). The cap is per-mail, not per-config-entry — relevant when a `{canonical_url}` or `{subject}` substitution unexpectedly inflates the rendered length.

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
2. Validates each key matches `^[a-z]+:[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]+)+$` — provider segment, colon, then a dotted event-type with at least two segments. Allows multi-segment vocabularies (`jira:issue.field.changed`, `github:pull_request.review.submitted`) that the previous draft's single-dot regex would have rejected. Underscores allowed within segments to match existing event names like `pull_request`. Invalid keys → exit at startup with a readable error.
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
