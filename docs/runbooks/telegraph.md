# Telegraph Operator Runbook

**Production path:** `<JIRA_BASE_URL>` → `<TELEGRAPH_PUBLIC_URL>/webhook/jira` (via tunnel/proxy) → Telegraph on `localhost:8765`

Design reference: [docs/design/telegraph.md](../design/telegraph.md)

---

## Config: `~/gt/settings/telegraph.toml`

Create this file before first run. Telegraph refuses to start without it.

```toml
[telegraph]
listen_addr  = ":8765"
buffer_size  = 256
nudge_window = "30s"
body_cap     = 4096
log_file     = "/path/to/gt/logs/telegraph.log"

[telegraph.providers.jira]
enabled    = true
secret_env = "GT_TELEGRAPH_JIRA_SECRET"
events     = [
    "jira:issue_created",
    "jira:issue_updated",
    "jira:comment_added",
    "jira:comment_updated",
]
```

**Field reference:**

| Field | Default | Notes |
|-------|---------|-------|
| `listen_addr` | `:8765` | TCP address for the HTTP webhook listener |
| `buffer_size` | `256` | Max queued events between L1 and L2; full → HTTP 503 to caller |
| `nudge_window` | `30s` | Max one Mayor nudge per this window regardless of event volume |
| `body_cap` | `4096` | Max bytes of Jira content in mail body; excess is truncated |
| `log_file` | `""` (stderr) | Path to log file; empty → logs go to stderr |

---

## Secret: Generating and Rotating the Jira HMAC Secret

Telegraph authenticates Jira webhooks using HMAC-SHA256. The secret is a
32-byte random value stored in an environment variable — never committed to version
control. `~/.zshrc` is a local disk file; treat it accordingly (restrict permissions,
exclude from backups if sensitive).

**Generate a new secret:**

```bash
openssl rand -hex 32
# Produces 64 hex characters (32 bytes), e.g.: a3f9e2c1d4b8...
```

**Set it in your shell environment** (add to `~/.zshrc` or equivalent):

```bash
export GT_TELEGRAPH_JIRA_SECRET="<the hex string from above>"
```

Telegraph reads this variable at startup via `ProviderConfig.ResolveSecret()` (called during `Config.ResolveProviders()`). If the
variable is unset or empty, the process exits immediately with an error.

**Rotate the secret:**

1. Generate a new value with `openssl rand -hex 32`.
2. Update `GT_TELEGRAPH_JIRA_SECRET` in your shell environment.
3. Update the Jira webhook registration with the new secret (see [Jira Configuration](#jira-webhook-configuration)).
4. Restart Telegraph (see [Restart Procedure](#restart-procedure)).

The old secret is invalidated as soon as Jira is updated. Plan rotations during
low-traffic periods — there is no overlap window.

---

## Starting Telegraph

Telegraph runs as a foreground process. Use `nohup` to detach from the terminal:

```bash
# Ensure the log and run directories exist
mkdir -p ~/gt/logs ~/gt/run

# Start in background
nohup gt telegraph start > ~/gt/logs/telegraph-boot.log 2>&1 &
echo $! > ~/gt/run/telegraph.pid
```

On successful startup, Telegraph prints:

```
[Telegraph] listening on :8765, providers=[jira]
```

This confirms the port is bound and the Jira provider is active. After this line,
all output goes to `log_file` (or stderr if unset).

**CLI flags:**

```bash
gt telegraph start --config /path/to/telegraph.toml   # override config path
gt telegraph start --town-root /path/to/gt            # override town root
```

---

## Exposing Telegraph via Tunnel or Proxy

Telegraph listens on `:8765` by default (all interfaces). If your firewall does
not restrict inbound access to port 8765, Telegraph will accept connections from
any source — not just your tunnel or proxy. Restrict the bind address to
`127.0.0.1:8765` in `listen_addr` if you want loopback-only binding, or rely on
OS-level firewall rules.

Route the public webhook URL to the local listener using whichever ingress approach
fits your infrastructure:

**General pattern:**
```
<TELEGRAPH_PUBLIC_URL>/webhook  →  http://127.0.0.1:8765/webhook
```

**Options (choose one):**

- **Cloudflare Zero Trust tunnel** — add a public hostname route:
  - Hostname: `telegraph.example.com`, path `/webhook`
  - Service: `http://127.0.0.1:8765`

- **ngrok** (development/testing):
  ```bash
  ngrok http 8765
  # Use the printed https://*.ngrok.io URL as <TELEGRAPH_PUBLIC_URL>
  ```

- **nginx reverse proxy** (self-hosted with TLS):
  ```nginx
  location /webhook {
      proxy_pass http://127.0.0.1:8765/webhook;
  }
  ```

- **Any other TCP/HTTP tunnel** that terminates TLS and forwards to `:8765`.

**Verify the tunnel is reachable:**

```bash
curl -s -o /dev/null -w "%{http_code}" https://<TELEGRAPH_PUBLIC_URL>/webhook/jira
# 405 = tunnel live, Telegraph running, Jira provider enabled (GET rejected; POST required)
# 404 = tunnel live, Telegraph running, but Jira provider not enabled or unknown path
# 502/504 = tunnel cannot reach :8765 (Telegraph not running)
```

---

## Jira Webhook Configuration

In your Jira instance (requires admin access):

1. Go to **Settings → System → WebHooks**.
2. Click **Create a WebHook**.
3. Fill in:
   - **Name:** `telegraph` (or any label you prefer)
   - **URL:** `https://<TELEGRAPH_PUBLIC_URL>/webhook/jira`
   - **Secret:** paste the value of `GT_TELEGRAPH_JIRA_SECRET`
   - **Events:** check the four supported types:
     - Issue: Created, Updated
     - Comment: Created, Updated
4. Leave **JQL Filter** empty to receive all projects, or restrict as needed.
5. Click **Create**.

Jira will send `X-Hub-Signature: sha256=<hmac>` on every delivery. Telegraph
verifies it using constant-time comparison. Mismatched signatures are rejected
with HTTP 401 and logged as `reason=hmac_invalid`.

---

## Verifying Receipt

### Tail the log file

```bash
LOG_FILE="$(grep -m1 'log_file' ~/gt/settings/telegraph.toml | awk -F'"' '{print $2}')"
LOG_FILE="${LOG_FILE:-$HOME/gt/logs/telegraph.log}"  # fallback if log_file not set (stderr mode)
tail -f "$LOG_FILE" | jq .
```

A successful round-trip produces two log lines:

```json
{"ts":"2026-04-26T08:00:00Z","component":"telegraph","event":"accept","provider":"jira","source_ip":"1.2.3.4","bytes_len":512,"latency_ms":2}
{"ts":"2026-04-26T08:00:00Z","component":"telegraph","event":"deliver","provider":"jira","event_type":"issue.created","event_id":"TEST-1-1714118400000","actor":"testuser","subject":"TEST-1","mail_id":""}
```

> Note: `event_id` is absent from `accept` lines — it is only extracted during L2
> translation and appears in `deliver`/`drop` lines.

### Send a signed test POST

Use this recipe to verify the full pipeline without triggering a real Jira event:

```bash
SECRET="${GT_TELEGRAPH_JIRA_SECRET}"

BODY='{"webhookEvent":"jira:issue_created","timestamp":1714118400000,"user":{"name":"testuser","displayName":"Test User"},"issue":{"key":"TEST-1","self":"https://jira.example.com/rest/api/2/issue/TEST-1","fields":{"summary":"Runbook verification","description":"","labels":[]}}}'

SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $2}')

# Send to local listener (bypasses tunnel — safe for testing)
curl -s -X POST http://127.0.0.1:8765/webhook/jira \
  -H "Content-Type: application/json" \
  -H "X-Hub-Signature: sha256=${SIG}" \
  -d "$BODY"
# Expected: HTTP 200, empty body
```

Then check Mayor's inbox to confirm the mail arrived:

```bash
gt mail inbox
# Should show a new message from telegraph/jira/testuser
```

---

## Reading Logs and tlog.Counters

Every log line is a single JSON object. Key fields:

| Field | Description |
|-------|-------------|
| `ts` | RFC3339 timestamp |
| `component` | Always `"telegraph"` |
| `event` | `accept`, `reject`, `deliver`, `drop`, `nudge_sent`, `nudge_suppressed` |
| `provider` | e.g. `"jira"` |
| `reason` | On `reject`: `hmac_invalid`, `backpressure`, `parse_error`, `provider_disabled`; on `drop`: `no_translator`, `translate_error`, `transform_error` |
| `source_ip` | Caller IP (tunnel edge IP in production) |
| `actor` | Who triggered the event |
| `subject` | Issue key, e.g. `PROJ-1234` |

**`tlog.Counters`** are in-process atomic counters that mirror the log events.
They reset on restart. The counters are:

| Counter | Incremented on |
|---------|---------------|
| `Accept` | Request authenticated and enqueued |
| `RejectHMACInvalid` | Bad or missing `X-Hub-Signature` |
| `RejectUnknownType` | (Unused in v1) — unknown event types are logged as `drop` with `reason="translate_error"` |
| `RejectBackpressure` | Internal buffer full (HTTP 503 returned) |
| `RejectParseError` | HTTP body read failure or request body exceeds size limit |
| `RejectProviderDis` | Provider set to `enabled = false` |
| `Deliver` | Mail successfully sent to Mayor |
| `Drop` | Event discarded post-L2 without delivery |
| `NudgeSent` | Mayor nudge sent (within rate-limit window) |
| `NudgeSuppressed` | Mayor nudge skipped (rate-limit window active) |

**Useful `jq` queries:**

```bash
# Count by event class
jq -s 'group_by(.event) | map({event: .[0].event, count: length})' "$LOG_FILE"

# Show only rejections
jq 'select(.event == "reject")' "$LOG_FILE"

# Watch backpressure in real time (buffer full)
tail -f "$LOG_FILE" | jq 'select(.reason == "backpressure")'

# All delivers for a specific issue key
jq 'select(.event == "deliver" and .subject == "PROJ-1234")' "$LOG_FILE"
```

---

## Restart Procedure

Telegraph has no hot-reload in v1 — config or secret changes require a restart.

> **Note:** Daemon-managed restart (automatic recovery on crash) is planned for
> gt-mwy.3 and not yet available. For now, restart means stopping and re-launching
> the process manually.

```bash
# Graceful stop — SIGTERM triggers a 5-second drain window then clean shutdown
kill -TERM $(cat ~/gt/run/telegraph.pid)
# Wait for the log line: [Telegraph] shutdown complete

# Start again (ensure dirs exist)
mkdir -p ~/gt/logs ~/gt/run
nohup gt telegraph start > ~/gt/logs/telegraph-boot.log 2>&1 &
echo $! > ~/gt/run/telegraph.pid
```

If `telegraph.pid` is missing, find the process:

```bash
pgrep -f "gt telegraph start"
kill -TERM <pid>
```

SIGINT has the same effect as SIGTERM (clean drain). SIGKILL skips the drain
window; use only if the process is unresponsive.

During the restart window (~1–2 seconds), Jira receives connection errors and
retries automatically with its built-in exponential backoff. No events are lost
if the restart is brief.
