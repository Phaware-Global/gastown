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
| `hello_ack` | worker → orch | `proto_version`, `gt_version`, `worker_id`, `os`, `arch`, `capabilities` (`docker: bool`, `exec_modes: []`, `container_platform`), `sessions: [<session summaries>]` | capability + state report; `gt_version` is what the freshness check compares, `container_platform` is what its docker daemon runs |
| `discover` | orch → worker | optional `rig`, `polecat` filters | list sessions by identity (backs `Discover`) |
| `sessions` | worker → orch | `[ {session, rig, polecat, state, started_at} ]` | reply to `discover` |
| `push_binaries` | orch → worker | streamed chunks (`name`, `sha256`, base64 `data`, `eof`) | update `gt`/`bd`/proxy-client to match the orchestrator release (core §6.1) |
| `push_binaries_ack` | worker → orch | `name`, `applied` (`installed` \| `staged`) | reply to the terminal chunk |
| `ping` / `pong` | both | — | keepalive; feeds the worker watchdog (§7) |

**Binary freshness rules.** `name` is an allowlist (`gt-proxy-client`,
`gt-worker-client`), never a path: it arrives from the wire and is joined to a
directory. The worker verifies the whole-file `sha256` **before** anything is
installed, writes through a staging file, and installs by rename — a half-written
`gt` on a worker is an agent that cannot reach the control plane.

- The worker runs out of its **own bin dir** (`<state-dir>/bin`), and `gt`/`bd`
  are relative symlinks to `gt-proxy-client` there. A push therefore updates the
  agent's CLI by replacing one file, and needs no write access to a system path.
- `gt-worker-client` is the running service: applying it means exiting for the
  supervisor, so a push **always stages** it — never applies inline. The
  orchestrator is still holding that connection (Provision reuses it for
  `session_open`), so exiting there would kill the ack and fail the very
  provision the refresh is meant to be invisible to. It is applied at a
  genuinely idle moment instead: a teardown that empties the worker, or a
  control connection closing with nothing live. While an apply is in flight the
  worker refuses `session_open` with `restarting`, and **`Provision` retries it**
  (bounded, inside the caller's deadline) — along with a connection that dies
  mid-bringup, which is the same event seen from the other side. Nothing upstream
  retries a provision, so without this the refresh would not be invisible: a
  polecat start that happened to race an upgrade would fail once.
- Binaries are served **per platform** from the tree `make dist` builds
  (`<root>/<goos>-<goarch>/`), selected by the worker's reported `os`/`arch`.
  The two pushables are pure Go and cross-compile with `CGO_ENABLED=0`, so a
  macOS orchestrator can keep a Linux worker current. A platform with no
  artifacts is **refused**, naming it — installing a binary the worker cannot
  execute is strictly worse than leaving it stale. A same-platform worker falls
  back to the orchestrator's own install dir, so the single-platform case works
  with no extra step.
- The container's binaries are pushed when the worker says it **lacks** them,
  not only when versions differ: a worker can be exactly up to date and have
  never received them (fresh enrollment, a wiped state dir), and gating on
  version alone left every container session running an agent with no `gt`/`bd`
  at all. `hello_ack` reports `container_binaries` for this. A container session
  that still cannot resolve one **fails** rather than starting a mute agent.
- A push may carry a `platform` tag, and then it is **for the work container,
  not the worker**: on a macOS worker the container is still a Linux container,
  so its `gt`/`bd` are a different build from the ones the worker runs. Tagged
  binaries are stored separately and never installed as the worker's own. When
  the container platform EQUALS the worker's, no tagged copy is sent — the
  worker's own binary runs unmodified in the container and injection uses it.
- Platform tags are validated by shape (`<goos>-<goarch>`) on **both** sides —
  `sockproto.ValidPlatformTag`. Each end joins the other's value to a local
  path (the worker's reported `os`/`arch` and `container_platform` on the
  orchestrator, the push tag on the worker), so validating on one side only
  would leave the other open to a traversal.
- Either side reporting version `dev` (an unversioned build) opts out, rather
  than pushing on every provision forever.
- `Provision` pushes best-effort and logs failures — `proto_version`, not
  `gt_version`, is the compatibility gate, so a version bump must never fail a
  polecat start. `gt worker push-binaries <rig>` is the operator path and does
  report errors.

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
agent, by **canonical name** (`SIGINT`, `SIGTERM`, `SIGHUP`, `SIGQUIT`). The
stream closes after `exit`.

Stream rules that follow from the framing:

- **One agent per session.** A second `attach` to a session that already has a
  running exec is refused (exit `125`): two agents on one worktree would
  double-write the tree the checkpoint loop is committing.
- **Half-close ≠ disconnect.** A launcher may half-close its write side to hand
  the agent stdin EOF; the worker closes the agent's stdin and keeps the agent
  running. Only a hard read error is treated as a lost pane.
- **Liveness.** The worker periodically writes a zero-length `stdout` frame — a
  no-op for the launcher, a real write on the socket — so a killed pane is
  detected even for an agent that produces no output. On a failed write (probe
  or output pump) the worker cancels the exec: `SIGTERM` to the agent's process
  group, then a hard kill after a short grace, so a dead launcher can never
  leave an agent pinning the session.
- **Session lifecycle wins.** `shutdown` stops an attached agent *before* the
  final flush (so the checkpoint captures a quiesced tree), and `teardown`
  stops it before the worktree is removed.

Session env in the `attach` payload is an **allowlist** (`GT_*`, `BD_*`,
`ANTHROPIC_DEFAULT_*`, plus specific model-*selection* keys, none
credential-shaped), enforced identically on both sides. The worker re-validates
rather than trusting the launcher's filter: in native mode the agent runs on the
worker host, where a wire `LD_PRELOAD` or `PATH` would be code execution.

**Endpoints never cross the wire.** Any key naming a destination (`*_URL`,
`*_HOST`, `*_ADDR`, `*_PORT`, `*_ENDPOINT`, …) is refused by shape, so a var
added later cannot quietly reopen the class. An endpoint is a worker-local fact:
the agent's control-plane URL is the worker's **own session relay** — the only
endpoint carrying that polecat's identity to the proxy — so `gt-worker-client`
sets `GT_PROXY_URL` itself from the relay it bound (in container mode the
container already has it from creation). An orchestrator-supplied endpoint would
be at best unreachable from the worker and at worst a redirect: a wire
`GT_PROXY_URL` points the agent's `gt`/`bd` RPC at an attacker, whose injected
responses arrive as mail and beads — prompt injection with extra steps.

Agent credentials come from the worker's own agent env file (§8), never from the
orchestrator — **and so does the endpoint a credential is sent to**.
`ANTHROPIC_BASE_URL` is barred from the wire: it is not itself a secret, but a
compromised or confused orchestrator that could set it would exfiltrate the
worker's own API key to an endpoint of its choosing. Ordering alone is not
enough — the env file wins only for keys it also sets, and a file holding the key
but not the base URL is an ordinary config — so an alternate-provider polecat
pairs base URL and credential together in the worker's env file. (Local gastown
already applies the same pairing rule: `config/env.go` excludes
`ANTHROPIC_BASE_URL` from parent-shell passthrough.)

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
    },

    // Proxy admin base URL used to sign session CSRs (§4.2). Default
    // http://127.0.0.1:9877. LOOPBACK ONLY: the admin API is unauthenticated —
    // its protection is that it binds the orchestrator's loopback — so a remote
    // value is refused at backend construction rather than dialed.
    "admin_url": null
  }
}
```

> **Who signs.** The worker never touches the CA: it generates its key locally
> and sends a CSR over the control connection, and the orchestrator signs it
> through the proxy's admin API on the worker's behalf. The admin endpoint
> **refuses** a CSR whose CN is not `gt-<rig>-<name>` — that refusal is the §4.2
> channel binding — and the backend re-checks the issued cert's CN before
> installing it as the session's identity.

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
this provider's form of the externalized agent-auth contract (core §7.1): the
operator provisions credentials on the machine once; they are injected into the
work process worker-side and never cross the socket.

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
   watchdog + orphaned-session reaping. (`push_binaries` freshness landed with
   the worker-deployment work — §4.1.)
5. Egress modes (§9) and macOS (launchd) worker support.

## 12. Decisions (socket)

All resolved 2026-07-11:

1. **Concurrent sessions per worker.** Worker-side **`max_sessions`** in the
   worker config (default **1**), advertised in `hello_ack`; the worker
   refuses `session_open` beyond it. One enrolled worker per rig in v1 —
   choosing among several workers is placement, which stays out of scope
   (core §1 non-goal). No load advertisement beyond the session count.
2. **Binary delivery trust.** v1 relies on **mTLS + enrollment** plus the
   per-binary `sha256` already carried in `push_binaries` — threat-parity
   with the EC2 provider's boot-time injection, and a compromised
   orchestrator host already owns the town it orchestrates.
   Release-key signature verification is future hardening, not v1.
3. **macOS workers.** **Linux, container mode ships first** — it shares the
   container/relay/bind-mount code paths with EC2. macOS native mode follows
   for iOS-adjacent rigs (launchd packaging, no-Docker native path).
4. **NAT / non-LAN workers.** v1 requires the worker `address` to be
   **directly reachable** from the orchestrator — the same mesh/VPN
   assumption the proxy plane already makes (core §9.6). A worker-initiated
   reverse-connection mode is deferred until a real deployment needs it.
