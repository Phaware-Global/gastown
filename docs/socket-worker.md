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
| `gt`, `bd` | created by `gt worker service install` (symlinks to `gt-proxy-client`) | the agent's control-plane calls; see [Binaries](#binaries) |
| `gt-proxy-client` | `make install` on that machine, then kept current by `push_binaries` | it *is* `gt` and `bd` there |
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
`gt-proxy-client` under those names, forwarding every call to the control plane
(`docs/proxy-server.md`). That is what lets a remote agent run `gt done` or
`bd update` with no town on disk and no credentials of its own.

`gt worker service install` sets this up: it copies `gt-worker-client` and
`gt-proxy-client` into `<state-dir>/bin`, points `gt` and `bd` there as relative
symlinks, and puts that directory **first** on the supervised worker's PATH. The
worker runs out of that directory too. Two consequences worth knowing:

- The bin dir is deliberately **not** the `gt` install dir. On a machine that is
  both orchestrator and worker, the real `gt` lives there and must keep living
  there — the agent gets the shim, your shell keeps the real binary.
- They are copies, because that directory is what the orchestrator refreshes.

The agent reaches the control plane in **relay mode**: it holds no certificate
of its own — the worker's local relay holds the session identity and terminates
mTLS upstream — so `gt` needs only `GT_PROXY_URL`, which the worker sets from
the relay it bound. (`gt-proxy-client` also still supports direct mTLS when
`GT_PROXY_CERT`/`KEY`/`CA` are supplied.)

### Keeping them fresh

You do not re-copy binaries after upgrading the orchestrator. The handshake
carries each side's version, and a worker whose version differs is refreshed
before the next session opens (§4.1 `push_binaries`): `gt-proxy-client` is
installed immediately, and `gt-worker-client` — the running service — is always
staged, then applied the first time that worker is genuinely idle (a session
ending, or the orchestrator's connection closing). A restart would abandon a
polecat mid-work, so the worker never takes one while it is being talked to.

`gt worker push-binaries <rig>` does it on demand. Use it when you want the
transfer to happen before someone starts a polecat, or when you need to see the
reason it is being skipped: the automatic path logs failures and carries on
(a version bump must never fail a polecat start), while the command reports them.

A worker on a different OS/architecture than the orchestrator is **refused**
rather than pushed to — this orchestrator only has its own platform's binaries,
and installing one the worker cannot execute is worse than leaving it stale.
Upgrade such a worker in place with `make install` there.

### Container mode

A work container is a **Linux** container even when the worker is a Mac, so its
`gt`/`bd` cannot be the worker's own binaries. The worker reports what its docker
daemon runs, the orchestrator pushes that platform's `gt-proxy-client` alongside
the worker's own, and container preparation injects it into the mounted `/opt/gt`
as `gt` and `bd`, links them onto `PATH` inside the container, and sets
`GT_PROXY_RELAY=1` (a container's relay is the bridge gateway, not loopback).

That means the orchestrator needs Linux artifacts even in an all-Mac setup:

```bash
make dist    # darwin/arm64, darwin/amd64, linux/amd64, linux/arm64
```

`make install` keeps this machine's own platform current on its own; `make dist`
is what you run when a worker — or a work container — is a different platform
from the orchestrator. A platform with no artifacts is refused with a message
naming it, rather than shipping a binary that cannot execute there.

The agent CLI is different in kind and stays operator-installed: its version,
licensing and auth are the operator's decision.

## Restarts and orphans

A worker restart does not preserve running sessions. The orchestrator reports
them orphaned, reaps them, and re-provisions — a polecat's work survives in its
checkpoint ref, but the agent is restarted. Session re-adoption across a worker
restart is a later phase, so prefer restarting a worker while no polecat is
attached.
