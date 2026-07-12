# Remote Polecat Execution — Provider: Local Network (Socket)

> **Date:** 2026-07-11
> **Author:** crew
> **Status:** Proposal
> **Core:** [remote-polecat-execution.md](remote-polecat-execution.md) — read it first; this spec assumes its architecture, interface, invariants, and lifecycle protocol.
> **Sibling:** [AWS EC2 provider](remote-polecat-execution-ec2.md)

This spec defines the **socket execution provider**: running polecats on a
**pre-provisioned machine** reachable over TCP (or a Unix socket). No cloud, no
provisioning API — the machine already exists and runs a persistent
**`gt-worker-client`** service that the orchestrator connects to.

**Use cases:** a GPU workstation down the hall; a spare Mac mini for iOS-adjacent
work; an air-gapped or on-prem environment where cloud execution is prohibited;
any scenario where a *specific physical machine* must do the work but the
orchestrator is a different host.

**Where EC2 creates and destroys machines, this provider opens and closes
*sessions* on a machine that persists.** Everything else — the mTLS proxy
control plane, the checkpoint protocol, exec modes, the image contract — is the
core, unchanged.

---

## 1. Model

```
Orchestrator host                             Worker machine (pre-provisioned)
┌─────────────────────────────┐               ┌─────────────────────────────────────┐
│ GasTown daemon              │               │ gt-worker-client  (persistent svc)  │
│  SocketBackend              │   control     │  • listens on tcp addr / unix sock  │
│   Provision ────────────────┼── conn (mTLS)►│  • authenticates the orchestrator   │
│   WrapCommand → attach argv─┼── exec stream►│  • per-session: CSR over the conn,  │
│   Teardown / signals ───────┼── messages ──►│    local relay, worktree, container │
│  gt-proxy-server ◄───mTLS───┼───────────────│  • checkpoint loop · watchdog       │
│  proxy CA · worker CA       │               │  • sessions survive its own restart │
└─────────────────────────────┘               └─────────────────────────────────────┘
```

`gt-worker-client` is this provider's `gt-worker-agent` (core §3) — same
responsibilities (cert acquisition, local relay, work-process management,
checkpoint loop, shutdown handling), packaged as a long-lived service instead of
a boot-injected program. The **provider channel** (core §3) is the socket
connection itself; the **provider interruption signal** (core §9.3) is an
explicit `shutdown` message on that connection (plus local OS signals).

Differences from an ephemeral cloud worker, all downstream of persistence:

- **`Provision` creates no machine** — it opens (or verifies) the connection and
  starts a *session* (§4).
- **`Teardown` destroys no machine** — it ends the session: stop the work
  process, clean the worktree, discard the session key, close per-session state
  (§4).
- **Binary freshness** is handled over the connection: the orchestrator pushes
  matching `gt`/`bd`/proxy-client binaries at session open if the worker's
  versions differ (core §6.1's delivery mechanism, socket form).
- **The offline checkpoint spool** (core §9.2) is simply the worker's own disk —
  the machine outlives the session, so a local spool directory is durable; it is
  drained to the proxy on reconnect.
- **No preemption** — no spot-style reclamation; the only interruptions are
  orchestrator-sent `shutdown` messages and local signals (operator reboot).

## 2. The `gt-worker-client` binary

A single static binary, installed and enrolled once per machine by the operator
(`gt worker install` / systemd unit on Linux, launchd on macOS). Responsibilities:

1. **Listen** on a configurable TCP `host:port` or Unix socket path.
2. **Authenticate** inbound orchestrator connections (§3) — mTLS for TCP; a
   pre-shared token is acceptable only on a Unix socket (§3.3).
3. **Per-session cert acquisition:** generate the session's private key locally
   and exchange CSR → signed cert **over the established connection** (§4.2) —
   the socket-provider form of core §7.2; the key never leaves the machine.
4. **Run the local relay** (`127.0.0.1:9899` or per core §6.1.1 for bridge
   containers), terminating mTLS to the host proxy with the session cert.
5. **Manage the work process:** prepare the worktree, pull the image, `docker
   run` the idle container (container mode) or prepare a native env, then
   `docker exec` / exec the agent argv on request (§5).
6. **Run the checkpoint loop** (core §9.2) and the local spool (§7).
7. **Handle shutdown:** graceful `shutdown`/`teardown` messages from the
   orchestrator, local SIGTERM (flush all sessions before exit), and the core
   §9.5 watchdog (end sessions on `max_runtime` or lost orchestrator contact —
   the machine itself always survives).
8. **Persist session state** (`/var/lib/gt-worker/sessions.json` + worktrees
   under `/var/lib/gt-worker/worktrees/<rig>/<polecat>`) so a `gt-worker-client`
   restart can re-adopt running containers and answer `discover` correctly.

## 3. Authentication (orchestrator ↔ worker)

Two identities exist on this channel, deliberately separate (they mirror the
core §3 two-channel split):

- **Machine identity** — mutual TLS on the socket connection: the worker proves
  it is the enrolled machine; the orchestrator proves it is the town's daemon.
  This authenticates the *provider channel*.
- **Polecat identity** — the per-session proxy cert (§4.2), used only against
  `gt-proxy-server`. This authenticates the *proxy channel* and is invisible to
  the socket protocol beyond the CSR exchange.

### 3.1 Worker CA and enrollment

The orchestrator maintains a dedicated **worker CA** (distinct from the proxy
CA — compromise of a machine cert must not allow minting polecat identities).
Enrollment is a one-time, operator-driven exchange:

1. Operator, on the worker: `gt-worker-client enroll --listen <addr>
   --join-token <token>` — generates the worker's machine keypair (key never
   leaves the machine), starts listening in **enrollment mode**.
2. Operator, on the orchestrator: `gt worker enroll <name> --address <addr>
   --join-token <token>` — the daemon connects over TLS with verification
   deferred, and both sides run a token-authenticated exchange (the join token
   is single-use, expiring, and operator-carried out-of-band): the worker sends
   its machine CSR; the daemon signs it with the worker CA and returns the
   machine cert + the worker CA certificate + the daemon's own client-cert CA.
3. Both sides persist their material; the token is invalidated; the worker exits
   enrollment mode. From now on the listener accepts **only** TLS 1.3 with
   client certs chaining to the orchestrator CA, and the orchestrator verifies
   the worker cert against the worker CA (name-pinned to `<name>`).

Re-enrollment (new token) rotates a machine cert; the daemon can revoke a
machine cert serial to cut a worker off.

### 3.2 Connection handshake

Every connection after enrollment:

1. TLS 1.3 mutual auth as above. Either side aborts on verification failure.
2. Orchestrator sends `hello`; worker replies `hello_ack` with its capabilities
   and active sessions (§4.1). Version negotiation happens here: `hello` carries
   the protocol version and the orchestrator's `gt` version; a worker that
   cannot speak the protocol version refuses with `error`.

### 3.3 Unix socket / pre-shared token mode

For a Unix socket (same host, or a socket forwarded through an
operator-managed secure tunnel), TLS is optional: filesystem permissions gate
the socket, and a **pre-shared token** (first message: `auth {token}`) replaces
the client cert. This mode is **refused on TCP listeners** — plaintext TCP with
a bearer token fails the core §7 invariants (the CSR/cert exchange and exec
payloads would be readable and injectable on the wire).

## 4. Wire protocol

Two connection types, both under the §3 handshake:

- **Control connection** — one persistent connection per worker, carrying
  newline-delimited JSON messages (one object per line, UTF-8). The daemon dials
  it at `Provision` and keeps it open; either side may reconnect (idempotent
  `hello` + session re-adoption).
- **Exec stream connections** — one per launched agent process (§5), carrying a
  binary-framed byte stream after a one-line JSON `attach` preamble.

Every control message: `{"type": "...", "session": "<session-id>", ...}`
(`session` omitted on connection-scoped messages). Requests carry `"id"` (a
nonce); responses echo it. Errors: `{"type":"error","id":…,"code":…,"msg":…}`.

### 4.1 Connection-scoped messages

| Message | Direction | Payload | Purpose |
|---|---|---|---|
| `hello` | orch → worker | `proto_version`, `gt_version`, `orchestrator_id` | open/resume a connection |
| `hello_ack` | worker → orch | `proto_version`, `worker_id`, `os`, `arch`, `capabilities` (`docker: bool`, `exec_modes: []`), `sessions: [<session summaries>]` | capability + state report |
| `discover` | orch → worker | optional `rig`, `polecat` filters | list sessions by identity (backs `Discover`) |
| `sessions` | worker → orch | `[ {session, rig, polecat, state, started_at} ]` | reply to `discover` |
| `push_binaries` | orch → worker | streamed chunks (`name`, `sha256`, base64 `data`, `eof`) | update `gt`/`bd`/proxy-client to match the orchestrator release (core §6.1) |
| `ping` / `pong` | both | — | keepalive; feeds the worker watchdog (§7) |

### 4.2 Session lifecycle messages

| Message | Direction | Payload | Purpose |
|---|---|---|---|
| `session_open` | orch → worker | `session`, `rig`, `polecat`, `exec_mode`, `image`, `network_mode`, `proxy_url`, `checkpoint_interval`, `max_runtime`, non-secret env | begin `Provision`: create worktree, pull image, start relay bootstrap |
| `csr` | worker → orch | `csr_pem` (CN `gt-<rig>-<name>`, key generated in worker tmpfs) | core §7.2 step 2 over the socket |
| `cert` | orch → worker | `cert_pem`, `ca_pem`, `not_after` | signed session cert (public material) |
| `session_ready` | worker → orch | `relay_addr`, worker-side preflight results (agent on `PATH`, `/bin/sh` — core §6.3) | `Provision` returns |
| `session_error` | worker → orch | `code`, `msg` | `Provision` fails fast (bad image etc.) |
| `shutdown` | orch → worker | `reason`, `grace_seconds` | graceful stop: run the core §9.3 sequence (stop agent → flush checkpoint → ack) |
| `shutdown_complete` | worker → orch | final checkpoint ref/commit | flush confirmation |
| `teardown` | orch → worker | `clean_worktree` (default `true`) | end the session (§6) |
| `teardown_complete` | worker → orch | — | session fully released |

**Channel binding (core §7.2 step 3):** the CSR is accepted only on the mTLS
connection of the machine the daemon addressed, within a `session_open` it
initiated, and the CN must equal that session's expected identity — the daemon
signs nothing else. A compromised worker can therefore only obtain certs for
polecats the daemon explicitly opened on *that* machine.

### 4.3 Exec stream framing

After the JSON preamble line `{"type":"attach","session":…,"exec":…}` and a
one-line `attach_ack`, the connection switches to binary frames:

```
1 byte  frame type   0=stdin  1=stdout  2=stderr  3=resize  4=exit  5=signal
4 bytes payload length (big-endian uint32)
N bytes payload
```

`resize` carries `{cols, rows}` JSON; `exit` carries the process's real exit
code (1-byte payload); `signal` (orch → worker) forwards e.g. SIGINT to the
agent. The stream closes after `exit`.

## 5. Interface mapping

| Core method | Socket implementation |
|---|---|
| `Provision` | Dial + handshake (or reuse the live control connection); `push_binaries` if versions differ; `session_open` → CSR/cert exchange → `session_ready`. If `hello_ack`/`discover` shows the session already live (daemon restart), **reattach** — no new session (core §9.4). Returns `Endpoint{address, session}`. |
| `WrapCommand` | Returns argv for a thin local launcher: `gt-worker-attach --address <addr> --session <id> -- <agent argv…>`. The launcher opens an exec stream (§4.3) sending `exec {argv, env}`; `gt-worker-client` execs it worker-side — container mode: `docker exec -e … <container> sh -c "<quoted argv>"`; native mode: direct exec as the session user — and pipes stdio. This is the blocking-pane process, same model as local/EC2. Non-secret session env rides the `exec` payload per core §7.4; command tokens follow the core §6.1.2 quoting discipline. |
| `Teardown` | `shutdown` (graceful, if the agent is still running) then `teardown`. The machine persists. |
| `Discover` | Dial the configured address, `discover {rig, polecat}` → `sessions`. No cloud tag queries; the worker's persisted session state (§2.8) is the source. |

> **Exit codes.** Unlike some cloud exec channels, the exec stream *does* carry
> the real remote exit code (`exit` frame), and `gt-worker-attach` exits with
> it. Per core §5 this is still used only for diagnostics — success remains
> `gt done` + heartbeats.

## 6. What "teardown" means on a persistent machine

`Teardown` must leave the machine as if the session never ran:

1. Stop the work container (`docker stop` + `rm`) or native process tree.
2. Flush a final checkpoint if the agent did not exit via `gt done` (the
   `shutdown` step already did this in the graceful path).
3. Remove the worktree (`clean_worktree: true`, the default — the checkpoint
   ref and polecat branch on the host are the durable artifacts; core §9.1). An
   operator may set `clean_worktree: false` per teardown for post-mortem
   debugging; the reaper's next sweep finishes the cleanup.
4. Shred the session key/cert from tmpfs, stop the session relay, delete the
   session from persisted state.
5. Optionally `docker image prune` per worker-local policy (not
   orchestrator-controlled).

## 7. Lifecycle details

- **Checkpoint loop** — exactly core §9.2, run by `gt-worker-client`.
- **Offline spool** — core §9.2's spool is a local directory
  (`/var/lib/gt-worker/spool/`): when the proxy is unreachable, checkpoint
  bundles land there and are drained (pushed, then deleted) on reconnect. No
  extra infrastructure; the machine's own disk is durable.
- **Interruption** — no preemption exists. The `shutdown` message (§4.2) is the
  interruption signal; local SIGTERM to `gt-worker-client` (machine reboot)
  triggers the same flush across all sessions, best-effort within the systemd
  stop timeout.
- **Watchdog (core §9.5, socket form)** — per session, `gt-worker-client`
  enforces `max_runtime` and a dead-man's switch (no orchestrator contact —
  control-connection pings *and* proxy pushes both failing — for a few ×
  `checkpoint_interval`): stop the agent, flush/spool a checkpoint, mark the
  session `orphaned`, **keep the machine running**. An orphaned session is
  cheap (no per-hour billing), so unlike EC2 the worker never self-destructs;
  the daemon reaps orphaned sessions on next contact.
- **Reattach** — daemon restart: `Discover`/`hello_ack` reports live sessions
  and `Provision` reattaches (core §9.4). Worker-service restart:
  `gt-worker-client` re-adopts sessions from persisted state; agents in
  containers keep running across the restart (the relay reconnects), and the
  next orchestrator connection resynchronizes.

## 8. Configuration schema extension

Socket-specific keys live under the `socket` key of the core `execution` block
(core §4). Annotated (JSONC — the real `settings/config.json` must be strict
JSON):

```jsonc
"execution": {
  // ── core shared fields (core §4) ──
  "backend": "socket",
  "exec_mode": "container",            // "container" | "native"
  "image": "ghcr.io/example/ios-dev-env:latest",
  "requires_docker": true,             // preflight checks the worker's capability handshake
  "network": { "mode": "open" },       // see §9 — egress is largely operator-owned
  "checkpoint_interval": "5m",
  "cooldown": "10m",
  "max_runtime": "4h",

  // ── socket provider extension ──
  "socket": {
    // TCP "host:port", or "unix:///path/to.sock" (§3.3)
    "address": "10.0.1.42:9878",

    // TLS material. "auto" (default) = managed by `gt worker enroll` under
    // ~/.gt/worker-ca/ — orchestrator client cert/key, worker CA to verify the
    // machine, pinned to the enrolled worker name. Explicit paths override.
    "tls": {
      "mode": "auto",                  // "auto" | "manual" | "none" (unix only)
      "worker_name": "gpu-box-1",      // pin: enrolled machine identity
      "ca_file": null,                 // manual mode: worker CA cert
      "cert_file": null,               // manual mode: orchestrator client cert
      "key_file": null                 // manual mode: orchestrator client key
    }
  }
}
```

The same rig as strict, comment-free JSON:

```json
{
  "execution": {
    "backend": "socket",
    "exec_mode": "container",
    "image": "ghcr.io/example/ios-dev-env:latest",
    "requires_docker": true,
    "network": { "mode": "open" },
    "checkpoint_interval": "5m",
    "cooldown": "10m",
    "max_runtime": "4h",
    "socket": {
      "address": "10.0.1.42:9878",
      "tls": { "mode": "auto", "worker_name": "gpu-box-1" }
    }
  }
}
```

Worker-side configuration (`/etc/gt-worker/config.json`, operator-managed, never
transmitted): listen address, state/worktree/spool directories, TLS material
from enrollment, allowed exec modes, and an optional **agent env file**
(`agent_env_file`) supplying worker-local secrets like `ANTHROPIC_API_KEY` —
this provider's `agent_auth` mechanism (core §7.1): the operator provisions LLM
credentials on the machine once; they are injected into the work process
worker-side and never cross the socket.

## 9. Network egress posture (socket implementation)

The core §7.3 planes hold: the control plane always flows through the proxy.
The work-egress plane, however, is **largely operator-owned** — the machine's
network is whatever the LAN provides, and gastown does not manage the LAN:

- **`open`** — the default and the honest description of most LAN workers: the
  work process uses the machine's normal egress.
- **`gateway`** — supported when the *operator* has installed a policy gateway
  (a Zero Trust client, a filtering proxy) on the machine; `gt-worker-client`
  verifies it is up before starting work, but does not install or configure it.
- **`sandboxed`** — container mode only: the work container is attached to an
  internal (no-egress) Docker network with only the relay reachable via the
  bridge gateway (core §6.1.1 option 2). Native mode cannot honor `sandboxed`
  on a machine gastown doesn't otherwise firewall, so preflight **rejects**
  `sandboxed` + `native` on this provider (core §7.3: reject rather than
  silently degrade).

## 10. Security model summary

- **Wire security:** TLS 1.3 mutual auth on every TCP connection (§3); token
  auth only on permission-gated Unix sockets. The exec stream and CSR exchange
  never travel unauthenticated or in plaintext.
- **Key invariants (core §7.2):** the session private key is generated in
  worker tmpfs and never leaves the machine — the socket carries the CSR and
  the (public) cert only. The machine key likewise never leaves the worker
  (enrollment signs a CSR, §3.1).
- **Identity separation:** worker CA ≠ proxy CA; a stolen machine cert lets an
  attacker *be a worker* (accept sessions) but not mint polecat identities or
  call the proxy; a stolen session cert is short-lived (core §7.2 TTL) and
  revocable by serial.
- **Blast radius:** the standing risk this provider adds over EC2 is
  **persistence** — the machine and any operator-provisioned credentials on it
  (the agent env file, §8) outlive the session. Mitigations: the core §10
  Docker-socket rules apply unchanged (untrusted rigs: rootless dockerd or no
  socket — there is no cloud metadata service here, but the operator's
  credential files play the equivalent role); the agent runs as a dedicated
  non-root user in native mode; keep worker machines single-purpose.
- **Orchestrator-side trust:** the daemon only connects to explicitly enrolled,
  name-pinned workers; `gt worker list`/`revoke` manage the fleet.

## 11. Implementation phases (socket)

Assumes core Tiers 1–2 (config, CA primitive, interface, provider-neutral
`gt-worker-agent` internals) are in place; `gt-worker-client` wraps the same
internals in a service + protocol shell.

1. `gt-worker-client` skeleton: listener, enrollment (§3.1), handshake (§3.2),
   persisted session state; `gt worker enroll/list/revoke` on the daemon.
2. Session lifecycle: `session_open` / CSR-cert exchange / `session_ready`;
   relay + worktree + idle container; `SocketBackend.Provision/Discover`.
3. Exec streaming: `gt-worker-attach`, the §4.3 framing, `WrapCommand`;
   worker-side preflight reporting.
4. Lifecycle completion: `shutdown`/`teardown`, checkpoint loop + local spool,
   watchdog + orphaned-session reaping, `push_binaries` freshness.
5. Egress modes (§9) and macOS (launchd) worker support.

## 12. Open questions (socket)

1. **Concurrent sessions per worker.** v1 assumes a small fixed capacity
   (worker config `max_sessions`, default 1). Should the worker advertise
   capacity/load in `hello_ack` so the daemon can pick among several enrolled
   workers for one rig — and is that already too close to the scheduler
   non-goal (core §1)?
2. **Binary delivery trust.** `push_binaries` lets the orchestrator run
   arbitrary code on the worker (inherent to the model, and equivalent to EC2's
   SSM injection) — is mTLS + enrollment sufficient, or should binaries also be
   signature-verified against a release key so a compromised *daemon host*
   can't push tampered binaries?
3. **macOS workers.** Container mode on macOS means Docker Desktop/colima with
   different bind-mount and UID semantics; native mode is the realistic first
   target for iOS rigs. Which ships first?
4. **NAT / non-LAN workers.** The orchestrator dials the worker, so a worker
   behind NAT needs a tunnel (Tailscale already assumed for the proxy plane,
   core §9.6). Standardize on "the address must be reachable; use your mesh,"
   or add a worker-initiated (reverse) connection mode later?
