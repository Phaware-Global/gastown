# Telegraph Operator Runbook

**Production path:** `phaware.atlassian.net` → `telegraph.phaware.care/webhook/jira` (Cloudflare Zero Trust tunnel) → Telegraph on `localhost:8765`

Design reference: [docs/design/telegraph.md](../design/telegraph.md)

---

## Config: `~/gt/settings/telegraph.toml`

Create this file before first run. Telegraph will refuse to start without it.

```toml
[telegraph]
listen_addr  = ":8765"
buffer_size  = 256
nudge_window = "30s"
body_cap     = 4096
log_file     = "/Users/agent/gt/logs/telegraph.log"

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

**Fields:**

| Field | Default | Notes |
|-------|---------|-------|
| `listen_addr` | `:8765` | TCP address for the webhook HTTP listener |
| `buffer_size` | `256` | Max queued events between L1 and L2; full → HTTP 503 to caller |
| `nudge_window` | `30s` | Max one Mayor nudge per this window regardless of event volume |
| `body_cap` | `4096` | Max bytes of Jira content in mail body; excess is truncated |
| `log_file` | `""` (stderr) | Path to log file; leave empty to log to stderr |

---

## Secret: Generating and Rotating the Jira HMAC Secret

Telegraph uses HMAC-SHA256 to verify Jira webhook signatures. The secret is a
32-byte random hex string stored in an environment variable — never committed.

**Generate a new secret:**

```bash
openssl rand -hex 32
# Example output: a3f9e2c1d4b8...  (64 hex chars = 32 bytes)
```

**Set it in your shell environment** (add to `~/.zshrc` or equivalent):

```bash
export GT_TELEGRAPH_JIRA_SECRET="<the hex string from above>"
```

**Rotate the secret:**

1. Generate a new hex-32 value with `openssl rand -hex 32`.
2. Update `GT_TELEGRAPH_JIRA_SECRET` in your shell environment.
3. Update the Jira webhook registration with the new secret (see [Jira Configuration](#jira-webhook-configuration) below).
4. Restart Telegraph (see [Restart Procedure](#restart-procedure)).

The old secret becomes invalid the moment Jira is updated. There is no overlap window; plan rotation during low-traffic periods.

---

## Starting Telegraph

Telegraph runs as a foreground process. Use `nohup` to detach from the terminal:

```bash
# Start in background, log to file
nohup gt telegraph start > ~/gt/logs/telegraph-boot.log 2>&1 &
echo $! > ~/gt/run/telegraph.pid
```

Or if `log_file` is set in `telegraph.toml`, stdout/stderr from the process itself is minimal:

```bash
nohup gt telegraph start &
echo $! > ~/gt/run/telegraph.pid
```

On startup Telegraph prints:

```
[Telegraph] listening on :8765, providers=[jira]
```

That line confirms the port is bound and the Jira provider is active.

**Flags:**

```bash
gt telegraph start --config /path/to/telegraph.toml   # override config path
gt telegraph start --town-root /path/to/gt            # override town root
```

---

## Cloudflare Zero Trust Tunnel

The public endpoint `telegraph.phaware.care/webhook` proxies to `http://127.0.0.1:8765/webhook` via a Cloudflare Zero Trust tunnel.

### Tunnel config (in your Cloudflare dashboard or `config.yml`)

```yaml
ingress:
  - hostname: telegraph.phaware.care
    path: /webhook
    service: http://127.0.0.1:8765
  - service: http_status:404
```

Or in the Cloudflare Zero Trust dashboard:

1. Go to **Networks → Tunnels** → select your tunnel.
2. Under **Public Hostnames**, add a route:
   - **Subdomain:** `telegraph`
   - **Domain:** `phaware.care`
   - **Path:** `/webhook`
   - **Service:** `HTTP` → `127.0.0.1:8765`
3. Save. No restart of `cloudflared` needed for hostname changes.

**Verify the tunnel is live:**

```bash
curl -s -o /dev/null -w "%{http_code}" https://telegraph.phaware.care/webhook/jira
# Expected: 405 (Method Not Allowed — GET is rejected; POST is the correct method)
# 502/504 means the tunnel cannot reach :8765 (Telegraph not running)
```

---

## Jira Webhook Configuration

Configure in Jira at `phaware.atlassian.net`:

1. **Settings → System → WebHooks** (requires Jira admin).
2. Click **Create a WebHook**.
3. Fill in:
   - **Name:** `telegraph-gastown`
   - **URL:** `https://telegraph.phaware.care/webhook/jira`
   - **Secret:** paste the value of `GT_TELEGRAPH_JIRA_SECRET`
   - **Events to send:** check all four:
     - Issue: Created, Updated
     - Comment: Created, Updated
4. Leave **JQL Filter** empty to receive all projects, or restrict as needed.
5. Click **Create**.

Jira sends a `X-Hub-Signature: sha256=<hmac>` header on every delivery.
Telegraph verifies it against `GT_TELEGRAPH_JIRA_SECRET` using constant-time comparison.
Mismatched signatures are rejected with HTTP 401 and logged as `reason=hmac_invalid`.

---

## Verifying Receipt

### Tail the log file

```bash
tail -f ~/gt/logs/telegraph.log | jq .
```

A successful delivery looks like:

```json
{"ts":"2026-04-26T08:00:00Z","component":"telegraph","event":"accept","provider":"jira","source_ip":"...","event_id":"PROJ-1234-1714118400000","bytes_len":512,"latency_ms":2}
{"ts":"2026-04-26T08:00:00Z","component":"telegraph","event":"deliver","provider":"jira","event_type":"issue.updated","event_id":"PROJ-1234-1714118400000","actor":"alice","subject":"PROJ-1234","mail_id":""}
```

### Send a signed test POST

Use this recipe to verify the full path without touching Jira:

```bash
# Set your secret
SECRET="${GT_TELEGRAPH_JIRA_SECRET}"

# Minimal Jira issue_created payload
BODY='{"webhookEvent":"jira:issue_created","timestamp":1714118400000,"user":{"name":"testuser","displayName":"Test User"},"issue":{"key":"TEST-1","self":"https://phaware.atlassian.net/rest/api/2/issue/TEST-1","fields":{"summary":"Runbook verification","description":"","labels":[]}}}'

# Compute HMAC-SHA256 signature
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $2}')

# Send to local listener (bypass tunnel for local testing)
curl -s -X POST http://127.0.0.1:8765/webhook/jira \
  -H "Content-Type: application/json" \
  -H "X-Hub-Signature: sha256=${SIG}" \
  -d "$BODY"
# Expected response: HTTP 200, empty body
```

Then check the log — you should see `event=accept` followed by `event=deliver`.

Check Mayor's inbox to confirm the mail arrived:

```bash
gt mail inbox
# Should show a new message from telegraph/jira/testuser
```

---

## Reading Logs and `tlog.Counters`

Every log line is a single JSON object with these fields:

| Field | Description |
|-------|-------------|
| `ts` | RFC3339 timestamp |
| `component` | Always `"telegraph"` |
| `event` | `accept`, `reject`, `deliver`, `drop`, `nudge_sent`, `nudge_suppressed` |
| `provider` | e.g. `"jira"` |
| `reason` | On `reject`/`drop`: `hmac_invalid`, `unknown_event_type`, `backpressure`, `parse_error`, `provider_disabled` |
| `source_ip` | Caller IP (Cloudflare edge IP in production) |
| `actor` | Who triggered the event |
| `subject` | Issue key (e.g. `PROJ-1234`) |

**Useful `jq` queries:**

```bash
# Count deliveries vs rejections
jq -s 'group_by(.event) | map({event: .[0].event, count: length})' ~/gt/logs/telegraph.log

# Show only rejections with reasons
jq 'select(.event == "reject")' ~/gt/logs/telegraph.log

# Watch backpressure events (buffer full)
tail -f ~/gt/logs/telegraph.log | jq 'select(.reason == "backpressure")'

# All delivers for a specific issue
jq 'select(.event == "deliver" and .subject == "PROJ-1234")' ~/gt/logs/telegraph.log
```

**`tlog.Counters`** are in-process atomic counters for the same event classes. They reset on restart. Query them in tests via `logger.Counters.Deliver.Load()`, `logger.Counters.RejectHMACInvalid.Load()`, etc.

---

## Restart Procedure

Telegraph has no hot-reload in v1. Any config or secret change requires a restart.

```bash
# Graceful stop (SIGTERM → 5s drain window, then shutdown)
kill -TERM $(cat ~/gt/run/telegraph.pid)
# Wait for: [Telegraph] shutdown complete

# Start again
nohup gt telegraph start > ~/gt/logs/telegraph-boot.log 2>&1 &
echo $! > ~/gt/run/telegraph.pid
```

If `telegraph.pid` is missing, find the process:

```bash
pgrep -f "gt telegraph start"
kill -TERM <pid>
```

SIGINT has the same effect as SIGTERM (clean 5-second drain). SIGKILL skips drain; use only if the process is unresponsive.

During the restart window (~1–2 seconds), Jira will receive HTTP connection errors and retry automatically with its built-in backoff. No events are lost if the restart is brief.

---

## Log File Location

`log_file` in `telegraph.toml` controls where structured JSON logs go.

- Empty string → stderr (captured by `nohup` redirect or journald).
- Absolute path → written directly. Telegraph opens the file with `O_APPEND` so log rotation via `logrotate` with `copytruncate` is safe.

Suggested path: `~/gt/logs/telegraph.log`

```bash
# Create the log directory if it doesn't exist
mkdir -p ~/gt/logs
```
