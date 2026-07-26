# Running a socket-execution worker

A **worker** is a machine that runs polecat agents for a rig whose execution
backend is `socket` (design: `docs/design/remote-polecat-execution-socket.md`).
The orchestrator keeps everything else — witness, refinery, reviewer, deacon,
mayor, and the control plane — on its own host; only polecats move.

This is the operator runbook. macOS is the supported worker platform today; the
same binary runs under systemd on Linux, but `gt worker service` writes a
launchd job only.

## What has to be on the worker

| Piece | How it gets there | Why |
|---|---|---|
| `gt-worker-client` | `make install` on that machine | the worker service itself |
| `gt`, `bd` | **operator-provisioned for now** (`gt-proxy-client` installed under both names) | the agent's control-plane calls; see [Binaries](#binaries) |
| agent CLI (e.g. `claude`) | operator-provisioned | licensing and auth are the operator's, not gastown's |
| `git` | operator-provisioned | the worktree is cloned through the session relay |
| Docker | only for `exec_mode: container` | Docker Desktop on macOS |

The orchestrator additionally needs `gt-worker-attach`, which `make install`
places next to `gt`. It is the process the tmux pane runs in place of the agent.

## 1. Enroll the machine

On the **orchestrator**:

```bash
gt worker enroll mac-mini-1 --generate-token
```

That prints the worker-side command with a single-use join token. Run it on the
**worker**:

```bash
gt-worker-client enroll \
    -listen 0.0.0.0:9899 \
    -tls-dir ~/Library/Application\ Support/gt-worker/tls \
    -join-token-file /path/to/token
```

The worker generates its own key — only a CSR crosses the wire — and writes
`machine.crt`, `machine.key`, `worker-ca.crt`, `client-ca.crt` and
`worker-name.txt` into `-tls-dir`.

## 2. Provide agent credentials

Create the agent env file on the worker, mode 0600:

```bash
install -m 600 /dev/null ~/Library/Application\ Support/gt-worker/agent.env
cat >> ~/Library/Application\ Support/gt-worker/agent.env <<'EOF'
ANTHROPIC_API_KEY=sk-…
EOF
```

This file is the **only** sanctioned source of agent credentials: the
orchestrator cannot send them, and it cannot send an endpoint either — a wire
`ANTHROPIC_BASE_URL` is refused, because it would redirect this key. If the
agent needs a non-default endpoint, pin it here alongside the key.

## 3. Install the service

On the worker:

```bash
gt worker service install \
    --listen 0.0.0.0:9899 \
    --proxy-url https://orchestrator.local:9876 \
    --worker-name mac-mini-1 \
    --tls-dir ~/Library/Application\ Support/gt-worker/tls \
    --agent-env-file ~/Library/Application\ Support/gt-worker/agent.env
```

`--proxy-url` is the orchestrator's `gt-proxy-server` **as this machine reaches
it** — not a loopback address, unless the worker and orchestrator are the same
box. The proxy's certificate must carry that hostname or IP: set
`extra_san_hosts` / `extra_san_ips` in `~/gt/.runtime/proxy/config.json` and
restart the proxy, because the cert is issued at startup.

Then `gt worker service status`, and `gt worker service restart` after any
config change. Logs land in `<state-dir>/worker.log`.

For a **unix-socket** worker (orchestrator and worker on one machine), pass
`--listen unix:///…/worker.sock` and put the pre-shared token in
`<state-dir>/worker.env` as `GT_WORKER_TOKEN=…` (0600). The job sources that
file, so the token never appears in `ps`.

## 4. Point a rig at it

In the rig's `settings/config.json`:

```json
{
  "execution": {
    "backend": "socket",
    "exec_mode": "native",
    "socket": {
      "address": "mac-mini-1.local:9899",
      "tls": { "mode": "auto", "worker_name": "mac-mini-1" }
    }
  }
}
```

`gt polecat start` in that rig now provisions a session on the worker, and the
pane runs `gt-worker-attach` against it.

## Binaries

`gt` and `bd` on a worker are **not** the real binaries: they are
`gt-proxy-client` installed under those names, forwarding every call to the
orchestrator's `gt-proxy-server` over mTLS with the session's own certificate
(`docs/proxy-server.md`). That is what lets a remote agent run `gt done` or
`bd update` with no town on disk and no credentials of its own.

They are gastown's own binaries and are coupled to the proxy protocol, so
keeping them in step with the orchestrator is gastown's job, not the operator's
— that is what `push_binaries` (design §11 phase 4) is for: the handshake
already reports the worker's version, and the orchestrator pushes matching
binaries when they differ. Until that lands, install `gt-proxy-client` under
both names by hand and re-copy it whenever the orchestrator is upgraded. In
container mode the same bits are injected into the work container from the
worker's `--gt-dir`, which is likewise operator-populated for now.

The agent CLI is different in kind and stays operator-installed: its version,
licensing and auth are the operator's decision.

## Restarts and orphans

A worker restart does not preserve running sessions. The orchestrator reports
them orphaned, reaps them, and re-provisions — a polecat's work survives in its
checkpoint ref, but the agent is restarted. Session re-adoption across a worker
restart is a later phase, so prefer restarting a worker while no polecat is
attached.
