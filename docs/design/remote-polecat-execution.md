# Remote Polecat Execution — Provider-Agnostic Core

> **Date:** 2026-06-21 (revised 2026-07-11: provider-agnostic refactor)
> **Author:** crew
> **Status:** Proposal
> **Related:** sandboxed-polecat-execution.md, persistent-polecat-pool.md, proxy-server.md, federation.md
> **Providers:** [AWS EC2](remote-polecat-execution-ec2.md) · [Local Network (Socket)](remote-polecat-execution-socket.md)

This document specifies the **provider-agnostic core** of remote polecat
execution: the architecture, configuration schema, `ExecutionBackend` interface,
execution model, credential invariants, and lifecycle protocol that every
execution provider implements. Provider-specific mechanics (provisioning APIs,
credential delivery channels, interruption signals, network plumbing) live in
the companion provider specifications:

- **[Provider: AWS EC2](remote-polecat-execution-ec2.md)** — ephemeral cloud
  instances (spot or on-demand) provisioned per task from a Packer-built image.
- **[Provider: Local Network (Socket)](remote-polecat-execution-socket.md)** —
  pre-provisioned machines on the local network (or any TCP-reachable host)
  running a persistent `gt-worker-client` service.

---

## 1. Problem Statement

Every polecat today runs on the orchestrator host: the daemon's `SessionManager`
execs the agent inside a tmux session under the user's UID, with direct loopback
access to Dolt, `.runtime/`, and mail. A single developer machine cannot sustain
10–20 simultaneous agent sessions without resource contention.

We want to **offload polecat execution to remote workers** — cloud instances
provisioned on demand, or existing machines elsewhere on the network — to
increase compute capacity, while keeping **certain rigs pinned to the
orchestrator host** (e.g. iOS development, which needs local provisioning
profiles and signing keys).

This document specifies a per-rig, pluggable **execution backend**. A backend
acquires a worker (by provisioning one, or by attaching to one that already
exists), launches a polecat on it, routes all control-plane and git traffic back
to the orchestrator over the existing mTLS proxy, preserves work across
interruptions, and releases the worker per a configured lifecycle.

### Goals

1. **Per-rig execution host config** — each rig declares where its polecats run.
2. **Pluggable execution providers** — ephemeral cloud sandboxes and
   pre-provisioned machines behind one interface; adding a provider never
   changes the core.
3. **mTLS control plane** — the remote polecat reaches `gt`/`bd` and git only
   through the host proxy; no direct Dolt auth, no GitHub access from the worker.
4. **Secure identity delivery** — each polecat gets a short-lived client cert;
   the private key is generated on the worker and never leaves it.
5. **Checkpoint + recovery** — work is continuously checkpointed and recoverable
   if a worker dies; a replacement resumes from the last checkpoint.
6. **Lifecycle management** — teardown after a cooldown, an absolute zombie cap,
   and a graceful flush on interruption or expiry.
7. **Configurable execution environment** — per-rig `native` vs. `container`
   execution and a per-rig work image; resource sizing where the provider
   supports it.
8. **Docker / nested-container support** — agents can use a real Docker daemon
   on providers that offer one (§10).

### Non-goals

- A cross-machine scheduler / placement engine. The orchestrator still owns
  dispatch; each rig statically selects a backend. Load-balancing polecats across
  a fleet is out of scope.
- Multi-town federation (see `federation.md`). This is single-town, single
  orchestrator, with remote *execution* only.
- Replacing the local execution path. `local` remains the default backend.

---

## 2. Background: what already exists

Two partial seams toward remote execution exist in the tree. This design builds
on the first and deliberately does **not** use the second.

### 2.1 Path A — `exec_wrapper` + `gt-proxy` (BUILT)

- **`exec_wrapper`** (`internal/config/types.go` `RuntimeConfig.ExecWrapper`,
  resolved per-rig by `resolveExecWrapper` in `internal/config/loader.go`) is a
  command prefix inserted between the env-export and the agent binary in
  `BuildStartupCommand`. It already wraps the *fully resolved* agent command.
- **`gt-proxy-server` / `gt-proxy-client`** (`internal/proxy/`,
  `cmd/gt-proxy-{server,client}`, documented in `proxy-server.md`) implement an
  mTLS CLI relay: the remote runs `gt-proxy-client` as `gt`/`bd`, which forwards
  argv to the host, where the real `gt`/`bd` execute against the host's Dolt. Git
  fetch/push are relayed to `~/gt/<rig>/.repo.git`. Identity is the client cert CN
  (`gt-<rig>-<name>`); an explicit **allowlist** gates permitted `gt`/`bd`
  subcommands (anything not listed is rejected with 403). (A separate denylist
  exists only for revoked cert serials, not subcommands.)

This is exactly the control-plane transport this design needs. **Gap:** the proxy
is a standalone binary — it is *not* wired into the spawn path. Nothing issues
per-polecat certs or injects `GT_PROXY_*` env automatically today.

### 2.2 Path B — `Connection` / `MachineRegistry` / SSH (SCAFFOLDED, UNUSED)

`internal/connection/` defines a `Connection` interface, `LocalConnection`, a
`MachineRegistry` (`{name, type: local|ssh, host, key_path, town_path}`), and an
address parser for `[machine:]rig[/polecat]`. The SSH implementation is a stub
(`"ssh connections not yet implemented"`) and **nothing in dispatch reads it**.

We do **not** build on Path B. It models named long-lived hosts with raw SSH and
has no transport story for beads. The *use case* it gestured at — a specific,
pre-existing machine as an execution target — **is** in scope, but it is served
by the [socket provider](remote-polecat-execution-socket.md), which pairs the
named-host model with a worker-side daemon and the proxy control plane instead
of raw shell access. Path B's scaffolding remains unused.

### 2.3 Persistent-pool tension

`persistent-polecat-pool.md` deliberately *reuses* polecats (identity + worktree
survive across assignments; "no nuke in the happy path"). The ephemeral-sandbox
cost model wants the opposite: sandboxes torn down per task. We resolve this in
§9: persistent **identity** (host-side), ephemeral **sandbox** (the worker
environment), with the polecat branch in `.repo.git` as the durable artifact.

---

## 3. Architecture

Four actors, in every deployment:

- **Orchestrator** — the GasTown daemon on the host. Owns dispatch, identity,
  beads, `.repo.git`, and the proxy server. Decides *when* a polecat runs and
  *which* backend runs it.
- **ExecutionBackend** — the provider driver inside the daemon (§5). Translates
  the generic lifecycle (`Provision` / `WrapCommand` / `Teardown` / `Discover`)
  into provider API calls.
- **`gt-worker-agent`** — the worker-side daemon (generic name; a provider may
  ship it under a provider-specific binary name). It runs on the worker machine
  and is responsible for: acquiring the polecat's proxy cert via the provider's
  delivery mechanism (§7.2), running the local mTLS relay, preparing and
  supervising the work process (`docker run` / native), running the checkpoint
  loop (§9.2), and handling interruption/shutdown signals and the local watchdog
  (§9.3, §9.5). On ephemeral providers it is injected at provision time; on
  pre-provisioned machines it runs as a persistent service.
- **`gt-proxy-server`** — the existing mTLS relay on the orchestrator (§2.1).
  Terminates all control-plane (`gt`/`bd`) and git traffic from workers.

```
Orchestrator host                            Worker (provider-supplied machine)
┌─────────────────────────────┐              ┌──────────────────────────────────────┐
│ GasTown daemon              │              │ gt-worker-agent                       │
│  SpawnPolecatForSling       │  Provision   │  • keygen local; CSR signed via the   │
│   └─ ExecutionBackend ──────┼──(provider──►│    provider's delivery channel (§7.2) │
│   └─ WrapCommand →          │    API)      │  • LOCAL plaintext relay :9899        │
│       launcher argv ────────┼──(provider──►│  • checkpoint + push loop (§9.2)      │
│                             │  exec chan.) │  • interruption handler + watchdog    │
│  gt-proxy-server ◄──mTLS────┼──────────────│    (§9.3, §9.5)                       │
│   /v1/exec  (gt/bd)         │              │                                       │
│   /v1/git/<rig> (.repo.git) │              │  work process (container | native)    │
│        │ async push         │              │   • gt/bd + /opt/gt injected          │
│        ▼                    │              │   • worktree mounted in               │
│  GitHub (host-only)         │              │   • GT_PROXY_URL + git origin →       │
│                             │              │     the local relay (§6.1.1)          │
└─────────────────────────────┘              │   • no direct Dolt / GitHub /         │
                                             │     control plane (egress per §7.3)   │
                                             └──────────────────────────────────────┘
```

The remote polecat never contacts Dolt, GitHub, or the gastown control plane
directly — all of that flows worker ↔ proxy, and the host pushes to GitHub. The
polecat's *own* outbound internet (package installs, APIs) is governed by a
per-rig egress posture (`sandboxed` / `gateway` / `open`, §7.3), implemented by
each provider with its own network primitives.

Because gastown controls the worker host, the gastown binaries, relay,
checkpointing, and interrupt handling all live in the single `gt-worker-agent`
service — no sidecar container, no shared-volume copy, no cross-container
signalling. The agent runs either natively or in a Docker container so per-rig
custom images and Docker-in-workflow both work (§6, §10).

Two channels connect orchestrator and worker, and they are deliberately
distinct:

1. **The provider channel** — how the backend provisions the worker, delivers
   the cert-signing exchange, launches the agent process, and sends lifecycle
   signals. This is provider-specific (a cloud command channel, a persistent
   socket, …) and is authenticated by provider means, not by gastown certs.
2. **The proxy channel** — the ongoing mTLS control plane (`gt`/`bd`, git,
   heartbeats) from `gt-worker-agent`'s relay to `gt-proxy-server`. This is
   identical across providers and authenticated by the per-polecat client cert.

---

## 4. Per-rig configuration

A new optional `Execution` block on `RigSettings`
(`internal/config/types.go`), loaded from each rig's `settings/config.json`.
Absent or `backend: "local"` → today's behavior (this is how iOS rigs stay
pinned to the host).

The block below is annotated for readability. The on-disk `settings/config.json`
is parsed by Go's `encoding/json` (`internal/config/loader.go`), which rejects
comments — the **actual file must be strict, comment-free JSON**. Treat the `//`
notes here as documentation, not literal syntax.

```jsonc
// settings/config.json  (illustrative — strip comments in the real file)
"execution": {
  // Which provider runs this rig's polecats. "local" is the default.
  // Additional values are defined by provider specs (e.g. "ec2", "socket").
  "backend": "local",

  // Execution environment (§6). Container mode (default for remote backends)
  // runs the agent in a container from `image`; gastown injects gt/bd + the
  // worktree. Native runs the agent directly on the worker.
  "exec_mode": "container",            // "container" | "native"
  "image": "registry.example.com/my-dev-env:latest",

  // Capability gate (§10): the rig's workflows need a real Docker daemon.
  // Preflight rejects providers that cannot supply one. Optional.
  "requires_docker": true,

  // Work-egress posture (§7.3). How each mode is realized is provider-defined.
  "network": {
    "mode": "gateway"                  // "sandboxed" | "gateway" | "open"
  },

  // Lifecycle (§9)
  "checkpoint_interval": "5m",         // continuous work checkpointing
  "cooldown": "10m",                   // delay before teardown after DONE
  "max_runtime": "4h",                 // absolute zombie cap

  // Provider-specific extension: exactly one object keyed by the backend name,
  // with keys defined by that provider's spec. Ignored by the core.
  // "ec2":    { ... }   — see remote-polecat-execution-ec2.md §4
  // "socket": { ... }   — see remote-polecat-execution-socket.md §8
}
```

**Core schema (shared fields only):** `backend`, `exec_mode`, `image`,
`requires_docker`, `network.mode`, `checkpoint_interval`, `cooldown`,
`max_runtime`. Everything else — regions, instance sizing, purchasing models,
addresses, TLS material, auth modes, gateway credentials — is a
**provider-specific extension**, namespaced under a key matching the `backend`
value and documented in that provider's spec. The core loader validates the
shared fields and hands the extension object to the backend opaquely.

`RigSettings` is a versioned, pointer-block struct; adding `Execution
*ExecutionConfig` follows the established pattern (`MergeQueue`, `Review`,
`CodeGraph`, …) and bumps `CurrentRigSettingsVersion`.

---

## 5. The `ExecutionBackend` interface

```go
// Resolved per rig. local = no-op; remote providers acquire real workers.
type ExecutionBackend interface {
    // Provision acquires the execution environment and blocks until the agent
    // can be launched into it. "Acquire" is provider-defined: create an
    // ephemeral sandbox, or open/verify a session on a pre-provisioned worker.
    // Idempotent for resume (see §9.4): if a live worker already exists for
    // this identity, reattach instead of acquiring a new one. Returns the
    // handle the daemon uses for WrapCommand/Teardown.
    Provision(ctx context.Context, spec PolecatSpec) (Endpoint, error)

    // WrapCommand takes the fully-resolved agent command (argv) and session env
    // and returns the complete argv the daemon should exec on the orchestrator
    // host to launch the agent remotely. The backend controls the ENTIRE
    // structure — it does not merely prepend a prefix (see note below). The
    // returned argv is the blocking-pane process (the tmux pane runs it); the
    // backend is responsible for landing argv and env in the remote process by
    // whatever mechanism its launcher requires (§7.4).
    //   Local:  the argv unchanged (today's behavior).
    //   Remote: a launcher that drives the provider's exec channel.
    WrapCommand(ep Endpoint, agentArgv []string, env map[string]string) ([]string, error)

    // Teardown releases the environment. Called by the reaper after cooldown,
    // on max_runtime expiry, or on explicit nuke. "Release" is provider-defined:
    // destroy the sandbox, or end the session and clean up on a persistent
    // worker — either way the polecat's remote footprint is gone afterward.
    Teardown(ctx context.Context, ep Endpoint) error

    // Discover re-finds live endpoints from PROVIDER-SIDE state by polecat
    // identity, so the daemon can reattach after a restart and the reaper can
    // sweep orphans (§"Endpoint discovery"). How identity is recorded and
    // queried is necessarily provider-specific — which is why this lives on
    // the interface rather than in generic daemon code.
    Discover(ctx context.Context, filter IdentityTags) ([]Endpoint, error)
}
```

> **`BuildStartupCommand` must be refactored — the prefix model is insufficient.**
> Today `BuildStartupCommand` (`internal/config/loader.go`) builds the startup
> string by appending the agent command as **trailing positional args** to the
> `ExecWrapper` prefix (`exec env VAR=val … <prefix> <agent> <args>`). That works
> for local wrappers (`exitbox`, `sudo`) that take a trailing command, but **breaks
> for remote backends**: remote launchers typically need the command embedded in
> a launcher-specific slot (a session document, a `--command` flag, a protocol
> message), not as trailing argv. So the existing static `ExecWrapper []string`
> (a pure prefix) is replaced, for remote backends, by
> `ExecutionBackend.WrapCommand`, which receives the resolved agent argv + env
> and returns the full argv — letting each backend place the command (and env,
> §7.4) in the slot its launcher actually requires. Local rigs keep the prefix
> path unchanged.

> **Launcher exit codes are not a success signal.** Providers' exec channels are
> not required to propagate the remote process's exit status to the launcher argv
> (some cannot). This is fine because, as with the local tmux model, gastown does
> **not** derive success from the launcher's exit code: completion is the agent
> calling `gt done` (heartbeat → `exiting`), and crash/abnormal-exit is caught by
> stale-heartbeat + liveness detection (§8.1, §9.3) — not the launch command's
> return value. If a backend needs the true remote exit status (e.g. for
> diagnostics), it captures it out-of-band via a provider-specific mechanism;
> provider specs document whether and how their channel carries it.

- `LocalBackend` — `Provision`/`Teardown` no-ops; `WrapCommand` returns the agent
  argv unchanged (today's path, refactored behind the interface, no behavior
  change).
- **[EC2 provider](remote-polecat-execution-ec2.md)** — ephemeral cloud
  instances, spot or on-demand, provisioned per task. The first cloud provider.
- **[Socket provider](remote-polecat-execution-socket.md)** — pre-provisioned
  machines running a persistent `gt-worker-client`; `Provision` opens a session,
  it does not create a machine.

`PolecatSpec` carries the resolved per-rig config (the §4 shared fields plus the
opaque provider extension) and the polecat identity (`<rig>/<name>`), so
backends are config-driven, not hard-coded.

### Endpoint discovery — surviving a daemon restart

`Endpoint` MUST be reconstructable from provider-side state, not just from
daemon memory, because the daemon can crash or restart while remote workers are
still running (and, for billed providers, still costing money). Every backend
therefore **records the polecat identity on the provider side** (`gt:rig`,
`gt:polecat`, `gt:session` — as resource tags, session metadata, or whatever the
provider offers) at `Provision`. On startup the daemon (and `Teardown`)
re-discovers live endpoints by querying that identity, rather than persisting
endpoint handles locally. This is what makes `Provision` idempotent for resume
(§9.4) and prevents orphaned workers after a crash. The reaper additionally
sweeps for identity-labeled workers with no corresponding live agent bead and
tears them down.

### Wiring points

- **Provision hook:** inserted between `SpawnPolecatForSling` returning and the
  deferred `StartSession` call (`internal/cmd/polecat_spawn.go`) — a natural gap,
  since session start is already deferred. This is also where the daemon mints
  the per-polecat cert (CN `gt-<rig>-<name>`) and arranges its **secure
  delivery** to the worker via the provider's mechanism (§7.2 — never as
  plaintext that lingers where it can be read back).
- **WrapCommand:** replaces the static `ExecWrapper` prefix-append in
  `BuildStartupCommand` (`internal/config/loader.go`) for remote backends — the
  command builder is refactored to delegate final-argv construction (command +
  env placement) to the backend (see the note in §5 and §7.4). Local rigs keep
  the prefix path.
- **Teardown + cooldown:** `killIdlePolecat` (`internal/daemon/daemon.go`) gains a
  `backend.Teardown()` call; a cooldown timestamp in the heartbeat makes the
  reaper wait before tearing down.
- **Zombie cap:** `reapIdlePolecat` gains an absolute `max_runtime`
  (wall-clock-since-spawn) check, independent of heartbeat freshness — today's
  reaper is idle-based and would not catch a busy-but-looping polecat.

---

## 6. Execution model

Every remote provider runs the polecat in one of two modes:

- **`native` (simplest):** the agent runs directly on the worker; the worker's
  base image / installed toolchain *is* the dev environment. Lowest overhead,
  full Docker access where present, but the toolchain is fixed per worker.
- **`container` (default; per-rig toolchains):** the agent runs in a Docker
  container from the rig's `image`, with gastown bits and the worktree
  bind-mounted in from the worker host. This preserves custom images per rig
  *and* gives Docker (§10).

Because gastown controls the worker host, gastown is delivered by **host
bind-mounts**, not container gymnastics: the single `gt-worker-agent` service
holds the cert, runs the relay, and owns checkpointing/interrupts. Nothing about
the proxy, cert, or control plane depends on what the work image contains.

### 6.1 Host injection (the primary mechanism)

**The worker's base image is a stable base, decoupled from the `gt` release
cadence.** Rebuilding worker base images for every `gt` update would make active
development painfully slow. Instead the base carries only slow-moving
infrastructure — `dockerd`, `git`, a `gt-agent` system user, the provider's
management agent, and a thin bootstrapper — and the **version-sensitive
binaries** (`gt`/`bd`/proxy-client and the `gt-worker-agent` program) are
**delivered from the orchestrator** — which *is* the matching `gt` release — via
the provider channel: injected at boot on ephemeral providers, or updated over
the session connection on persistent workers. This guarantees the proxy client
matches the server protocol without rebuilding base images on a `gt` bump; base
rebuilds are reserved for OS/security updates. Each provider spec defines its
delivery mechanism.

The worker base carries **`git`** as well — host-side checkpointing (§9.2) runs
`git` in `gt-worker-agent`, so it is a worker requirement independent of whether
the *work image* ships git.

```
Worker host (base: dockerd · git · provider mgmt agent · gt-worker-agent · gt/bd · idle bin)
│
├── gt-worker-agent   (the worker-side gastown supervisor; service manager per provider)
│     • generates key locally; gets a CSR signed via the provider channel (§7.2)
│       → cert in worker tmpfs
│     • runs the LOCAL relay; terminates mTLS to the host proxy (:9876) upstream
│     • runs the checkpoint+push loop over the worktree (§9.2)
│     • runs the provider's interruption watcher + the local watchdog (§9.3, §9.5)
│     • prepares the env once relay + cert are up — container mode: `docker run`
│       the IDLE work container (idle entrypoint, no agent yet); native: nothing
│
└── agent PROCESS — launched on demand by the ORCHESTRATOR, not gt-worker-agent:
      WrapCommand → provider exec channel → (container: `docker exec`) -- <argv>
      • injected mounts (container): /opt/gt (gt/bd + idle) · the worktree
        · docker.sock where the rig uses Docker (§10)
      • env: GT_PROXY_URL + git origin → the local relay (address per §6.1.1)
      • holds NO cert/key — mTLS is gt-worker-agent's job
```

**mTLS termination lives entirely in `gt-worker-agent` on the worker host.** The
agent's `gt`/`bd`/git talk to a plaintext local relay; the worker service adds
the client cert and forwards over mTLS to the host proxy. **The private key
never enters the work container or its env.** The hop is worker-internal and
never leaves the machine. Two distinct ports avoid confusion: the local relay is
`…:9899`; the host proxy (mTLS upstream) is `:9876`.

**Worktree ownership & agent UID.** `gt-worker-agent` runs with the privileges
it needs to manage `dockerd` and read/commit the worktree for host-side
checkpointing (root on providers where it also manages the host). The agent's
UID then depends on the mode:

- **Container mode + bridge networking:** the container may run as `root` — it
  is namespaced, and container-reachable worker credentials (where the provider
  exposes any) are blocked at the bridge (§10; provider specs give the
  mechanics). Root inside the container is contained **for the
  no-raw-docker-socket case** only — a container *with* the host `docker.sock`
  is not contained by namespacing (§10).
- **`native` mode / host networking:** there is no namespace isolation, so
  providers that expose ambient credentials on the worker require the agent to
  run as a **dedicated non-root `gt-agent` UID** — distinct from
  `gt-worker-agent`'s UID — so host-level controls can tell the two apart. See
  the provider specs for the concrete requirement.

In every case the worktree is created **group-owned by a shared `gt` group**
(and group-writable), so `gt-worker-agent` and the agent (whatever UID) both
retain access and checkpointing is never blocked by an ownership mismatch.

#### 6.1.1 Container networking — how the agent reaches the relay

`127.0.0.1` inside a bridge-network container is the *container's* loopback,
**not** the host — so a relay bound to worker-host `127.0.0.1:9899` is
unreachable from a default bridge container. Two supported wirings (the backend
picks one and sets `GT_PROXY_URL` / git `origin` to match):

1. **Host networking** (`--network host`, default **for trusted rigs only**). The
   container shares the host network namespace, so `127.0.0.1:9899` *is* the
   relay. Simplest, but it **defeats network-level isolation of the container**:
   with no bridge and no routing hop, the container's traffic is
   indistinguishable from the host's, so bridge-level controls (including any
   provider credential-endpoint blocking, §10) cannot apply. **Acceptable only
   for trusted rigs.**
2. **Bridge + host-gateway** (**required for `sandboxed` / untrusted-code
   rigs**). Keep bridge isolation: bind the relay to the docker bridge gateway
   (or `0.0.0.0:9899`, firewalled to the bridge subnet) and start the container
   with `--add-host=host.docker.internal:host-gateway`; the agent reaches the
   relay at `http://host.docker.internal:9899`. This is the mode in which
   network-level hardening actually works, so untrusted rigs **must** use it
   (preflight enforces this pairing).

So the networking default is **trust-dependent**, not unconditional: host
networking for trusted rigs (simplicity), bridge for untrusted ones (so
network hardening is effective). `native` mode has no container; the agent uses
`127.0.0.1:9899`, and any credential-endpoint lockdown happens at the worker
level instead (§10, provider specs).

#### 6.1.2 Startup ordering & command construction

Two distinct lifecycle steps, by two different actors:

1. **`gt-worker-agent` prepares the environment** (service/Docker ordering, no
   health-check gymnastics): obtain its cert (CSR over the provider channel,
   §7.2), confirm the relay is listening (it probes the relay itself), and — in
   container mode — `docker run` the **idle** work container (running only the
   injected idle entrypoint; no agent yet).
2. **The orchestrator launches the agent process** on demand via `WrapCommand` →
   the provider exec channel → (container) `docker exec` into the prepared
   container. This is the same blocking-pane model as local; `gt-worker-agent`
   never starts the agent itself — it only readies the worker and supervises
   checkpoint/interrupt.

> **Command construction (avoid an injection footgun).** The remote command
> (`WrapCommand` → provider exec channel → `docker exec -- …`) is built from the
> tokenized agent argv (§5, §8) with the same `ShellQuote` discipline
> `BuildStartupCommand` already applies (`internal/config/loader.go`): every
> token individually shell-quoted, config-derived parts (model flags,
> custom-agent args, the free-form `InitialPrompt`) treated as **untrusted data
> to be quoted, never interpolated raw** (pass the prompt as a single quoted arg
> or via stdin/file). Session env is injected per §7.4, not via the
> orchestrator's local `exec env`.

### 6.2 Image contract (container mode only)

In `container` mode the image is the dev environment and **carries only the
toolchain and agent runtime; gastown injects the rest from the worker host.**
Concisely:

**MUST provide:** (1) the **agent runtime binary** the rig resolves to (`claude`,
`codex`, …) on `PATH`; (2) the **project toolchain** (language runtimes,
build/test tools — the reason for a custom image); (3) a **POSIX shell
(`/bin/sh`)** — not because `docker exec` itself uses one (it `execve`s the
binary directly), but because most providers' exec channels are **string**
interfaces: `WrapCommand` delivers the agent as a single shell-quoted command
line that we run as `sh -c "<argv>"`; that, plus interactive sessions, needs
`/bin/sh` (v1 requires it; distroless is a known limitation — open question 6);
(4) a **Docker client** *only if* the rig's workflows call `docker`/`docker
compose` (§10).

**MUST NOT be expected to carry:** gastown binaries (`gt`/`bd`/proxy client —
bind-mounted at `/opt/gt`); a specific entrypoint/`CMD`/init (gastown supplies an
injected idle entrypoint); any provider management agent (the provider channel
terminates on the worker host); any credentials/certs/Dolt config (injected per
§7; the proxy key never enters the container); or any assumption about direct
egress (control plane is proxied; the agent's own internet is governed by
`network.mode`, §7.3).

`git` is needed in the image only if the agent's *own* workflows call it;
gastown's checkpoint/push runs host-side in `gt-worker-agent`, independent of
the image.

**Default image:** when `image` is empty, the backend uses a gastown-published
default dev image satisfying the above for the `claude` agent plus a common
toolchain — same injection path, no special case.

### 6.3 Preflight

Preflight splits by *where it can run cheaply* — the orchestrator is often a
laptop, so it must **not** pull the (potentially large) work image just to
inspect it:

- **Orchestrator-side (config only, no image pull):** reject `requires_docker`
  on a provider that cannot supply a Docker daemon (§10); reject an
  untrusted/`sandboxed` rig configured for host networking (§6.1.1);
  sanity-check the resolved agent config and the provider extension block
  (sizing, addresses, auth references — per provider spec). These need no image
  and fail instantly.
- **Worker-side (image-content checks), in `gt-worker-agent` after the worker
  pulls the image:** verify the resolved agent runtime resolves on `PATH` and
  that `/bin/sh` exists. The worker has already pulled the image to run it, so
  this is free there and ruinous on the laptop. A failure is reported back over
  the control plane (and surfaced on the bead) **before** the agent is launched,
  so a bad image still fails fast with a clear error — just without dragging the
  image across the developer's link.

---

## 7. Credentials & identity

Four distinct credential concerns cross the orchestrator↔worker boundary. The
invariant is that **no secret material — a private key, an API key, a password,
the proxy client **private key** (the signed cert is public, not secret) — ever
appears in the work image, in provisioning metadata, in the remote command
string, in process args, or in any provider-side log of the exec channel.**
Secret **references** (identifiers naming a secret in a provider's secret store)
are expected and not sensitive; the provider resolves them worker-side without
exposing the value.

| Concern | Core invariant | Delivery |
|---|---|---|
| **Control-plane identity** (proxy cert) | Daemon mints a short-lived leaf cert (CN `gt-<rig>-<name>`) at provision; **the private key is generated on the worker and never transmitted** (§7.2). Identity is the cert, enforced by `gt-proxy-server`. | Provider-defined signing channel (§7.2). |
| **Image pull auth** | Never in the command string or image. | Provider-defined: ambient worker credentials or a worker-side secret store / operator-installed login. |
| **LLM auth** | Never in the command string, image, or provisioning metadata (§7.1). | Provider-defined worker-side injection; ambient cloud identity where available. |
| **Worker's own platform identity** | Whatever identity the worker holds (cloud role, machine cert) is scoped to that worker's needs, least-privilege. | Provider-defined. |

### 7.1 LLM auth detail

`internal/config/env.go` already defines the per-provider auth allowlist
(`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, the cloud-LLM platform var groups,
…) and emits them into the agent env. **But** it delivers them by reading the
orchestrator's shell env and baking them into the host `exec env` prefix — which
(a) does not propagate through a remote exec channel into the worker process,
and (b) would leak the secret via provider-side channel/session logs and process
args if inlined into the remote command. So for remote backends, the **names**
are accounted for but the **delivery** must change:

- Route auth vars through **worker-side env injection** (a secret store the
  worker can read, ambient cloud identity, or operator-provisioned worker
  config), never the command line.
- Prefer mechanisms with **no transmitted secret at all** (e.g. an ambient cloud
  identity that grants model access) where the provider offers one.

A useful security property falls out: the key lives worker-side (secret store or
machine config), not the orchestrator's shell — the remote path is *more*
isolated than local. Each provider spec defines its `agent_auth` mechanism.

### 7.2 Secure proxy-cert delivery

The per-polecat client cert and its **private key** are the most sensitive
material in the system: they grant `gt`/`bd` and git access as that identity.
The key MUST NOT be injected as lingering plaintext readable through provider
APIs **and should never travel over the network at all.** The core protocol,
which every provider implements over its own channel:

1. **Key generation is worker-local.** `gt-worker-agent` generates the private
   key in worker tmpfs. It never leaves the machine — not to the orchestrator,
   not to a secret store.
2. **CSR-signing exchange.** The worker emits a CSR (CN `gt-<rig>-<name>`) over
   the provider channel; the daemon validates the CSR's CN against the expected
   identity for the worker it is talking to, signs it with the CA, and returns
   the **cert** over the same channel. A signed certificate is public material,
   not a secret — it may transit channels that are logged.
3. **Channel binding replaces bootstrap tokens.** The provider channel is
   already mutually authenticated (by the provider's platform identity, mTLS,
   or equivalent) and
   addressed to the exact worker the daemon acquired — so no separate bootstrap
   secret is needed to bind the CSR to the worker. Providers whose channels lack
   this property must add an explicit binding step (see the socket spec's
   enrollment flow).

**New CA primitive required:** the proxy CA today (`/v1/admin/issue-cert`)
*generates a keypair and returns the key* — which would defeat the
no-key-transport goal. This design needs a **CSR-signing path** (`sign(csr) →
cert`, key never seen by the CA) added to the CA/proxy; do not reuse the
keypair-issuing endpoint for the remote flow.

The private key lives only in worker tmpfs (never in the work container, and
under the CSR flow never on the wire), and the cert is short-lived so exposure
is bounded even if a worker is compromised. This design wants a **shorter
`proxy_cert_ttl` (≈24h)** for remote workers than the proxy's current
keypair-issuance default (720h / 30d) — an intentional change; the authoritative
default should be set on the new CSR-signing path, not inherited. The CA can
revoke a leaked serial via the proxy denylist.

> **Ongoing relay traffic** (the `:9876` mTLS control plane) must not require an
> inbound port on the orchestrator opened per-worker, and must not pin a raw
> orchestrator IP (the orchestrator is often a laptop, §9.6). The worker dials a
> **stable host name** (Tailscale / VPN / dynamic DNS) for the proxy; the
> `:9876`/`RequireAndVerifyClientCert` config is unchanged. Cert delivery and
> lifecycle signalling ride the provider channel, not a listener on the laptop.

### 7.3 Network model & egress posture

Two **orthogonal** network planes, governed separately:

- **Control plane** (`gt`/`bd`, git, beads) → **always** the host proxy, in
  every mode. This is about identity, not isolation: it is how the polecat
  reaches Dolt without DB auth. It never changes with the egress posture.
  (Providers may additionally need a narrow allowlist of their own platform
  endpoints for the provider channel to function — see each provider spec.)
- **Work-egress plane** — the agent's *own* outbound internet (npm, PyPI,
  crates.io, the Go module proxy, apt mirrors, GitHub for dependencies,
  arbitrary HTTP APIs the task legitimately calls). This is what `network.mode`
  controls.

> **Why this matters:** a fully locked-down worker would break `npm install`,
> `pip install`, `go mod download`, `apt-get`, and most real build steps. Total
> isolation is correct for *untrusted* work but is the wrong default for
> ordinary development. So egress is a **per-rig spectrum**, not a binary.

#### `network.mode`

1. **`sandboxed`** — no general egress; only the proxy (plus any provider
   platform allowlist). Maximum isolation — the original goal of
   `sandboxed-polecat-execution.md` (prevent credential exfiltration /
   malicious-MCP reach-out). Dependencies must be **pre-baked into the
   image/worker base** or served from an **internal mirror / pull-through
   cache** the provider can reach without general egress. Use for
   high-sensitivity rigs or untrusted code.

2. **`gateway`** *(recommended default for dev work)* — full outbound internet,
   but **mediated by a policy-enforcing egress gateway** rather than raw NAT:
   legitimate package and API traffic gets through while the gateway **enforces
   destination policy, blocks known-bad endpoints, and logs every flow** for
   audit/DLP. The happy medium: a real security posture — exfiltration is
   policed and observable — without crippling the agent. Gateway product,
   credential handling, and bring-up are provider-defined; `gt-worker-agent`
   brings the tunnel up before the work process starts.

3. **`open`** — unrestricted egress (whatever the worker's network allows).
   Simplest, least safe; for fully trusted rigs where the gateway hop is
   unwanted. Allowed but never the default.

In **all** modes the control plane and git still flow through the gastown proxy
— only the *work-egress* plane differs. The shift from `sandboxed` to `gateway`
is a shift from **isolation-by-prevention** to **mediation-by-policy +
observability**: appropriate when the agent must reach real registries, but you
still want every byte of egress attributable and governed. A provider that
cannot implement a mode rejects it at preflight rather than silently degrading
(see the socket spec, where egress is largely operator-owned).

### 7.4 Session-env propagation

The agent needs gastown's session env to function. These fall into two groups:

- **Existing session vars** — `GT_ROLE`, `GT_SESSION`, `GT_ROOT`, `BD_ACTOR`,
  `GT_RIG`, `GT_POLECAT`, the Dolt host/port, etc. — produced today by `AgentEnv`
  (`internal/config/env.go`) and emitted as the `exec env VAR=val …` prefix in
  `BuildStartupCommand`.
- **New relay vars this design adds** — `GT_PROXY_URL` and the `GIT_SSL_*` group
  (and friends) that point gt/bd/git at the local relay (§6.1). These are **not**
  emitted by `AgentEnv` today; they are introduced by this work (set per the
  chosen container-networking mode, §6.1.1) and injected alongside the existing
  vars.

The problem is the same for both groups: the local `exec env` prefix **runs on
the orchestrator host and does not cross the boundary** — neither remote exec
channels nor `docker exec` forward the client's environment to the remote
process. So a naive remote launch would start the agent with **none** of these
set, and `gt`/`bd` would fail to resolve their role, rig, or workspace.

The backend therefore injects session env **remotely**, as part of `WrapCommand`
(§5) — never via the orchestrator's local `exec env`:

- **Container mode:** pass each var through `docker exec -e VAR=val …` (or an
  `--env-file` that `gt-worker-agent` writes to the bind-mounted `/opt/gt` and
  references), so they are set in the agent's process, not the worker host's.
- **Native mode:** `gt-worker-agent` writes them to an env file it `source`s (or
  prepends `env VAR=val …` to the launched argv) on the worker.

Split by sensitivity, consistent with §7.1:

- **Non-secret session env** (`GT_ROLE`, `GT_SESSION`, `GT_ROOT`, `BD_ACTOR`,
  `GT_PROXY_URL`, …) travels in the `WrapCommand` payload / env-file. These are
  not secrets; appearing in the exec-channel invocation is acceptable.
- **Secret env** (LLM API keys, registry creds, the proxy cert/key) is **never**
  in the command payload — it is injected via the provider's worker-side secret
  mechanism (§7.1–7.2) or, for the proxy cert, terminated entirely in
  `gt-worker-agent` so it never reaches the agent at all.

This is why `WrapCommand` takes the env map (§5): the backend, not
`BuildStartupCommand`, is responsible for landing the session env in the remote
process by whatever mechanism its launcher requires.

---

## 8. Model configuration carry-over

Rig model/agent config (`Agent` preset → `RoleAgents["polecat"]` → custom agent →
`Command`/`Args`/`Env`) resolves **host-side in `BuildStartupCommand`** before
the wrapper is applied. The remote needs none of gastown's agent config files;
only the resolved command + env cross. The config splits three ways:

| Surface | Examples | Crosses to the remote? |
|---|---|---|
| **Command + Args + prompt** | `claude --model claude-opus-4-8`, custom `Command: codex`, `--dangerously-skip-permissions`, `InitialPrompt` | **Free** — it *is* the wrapped command string (§6.1.2). |
| **Env-based model config** (`rc.Env`) | `ANTHROPIC_BASE_URL` (Groq/MiniMax), `ANTHROPIC_MODEL`, `ANTHROPIC_DEFAULT_*_MODEL` | **Inject** — same boundary as auth: non-secret → plain remote env; secret → the provider's secret mechanism (§7.4). |
| **Agent runtime binary** | the binary `Command` names | **Must be in the image / worker base** (§6.2). |

So custom models carry over: CLI config for free, env config via the §7
injection path, and the runtime via the §6.2 image contract.

### 8.1 Liveness consequence

`GT_PROCESS_NAMES` (used by Witness/reaper to match `pane_current_command`) is
meaningless for remote: the host pane runs the backend's *launcher* process, not
the agent. **Remote backends use heartbeat-based liveness only** (the
`.runtime/heartbeats/` mechanism via the proxy), not process-name matching. This
is required regardless of model config; custom-agent rigs just make it concrete.

---

## 9. Lifecycle, recovery, and interruption

### 9.1 Identity vs. sandbox (persistent-pool reconciliation)

For remote backends there is **no host-side worktree**. Persistent **identity**
(name, agent bead, CV) stays host-side; the **sandbox** is the ephemeral remote
execution environment (an instance, or a session on a persistent machine); the
durable artifact is the polecat branch in `~/gt/<rig>/.repo.git`. This diverges
from the reuse-the-worktree pool model and is carved out explicitly for
`backend != local`.

### 9.2 Recovery — git push via proxy (no storage snapshots)

There is no provider storage-snapshot lifecycle; durability is host-side git.
**Two refs with two roles** (this is the source-of-truth split):

- The **polecat branch** (`polecat/<name>/<issue>`) is the artifact for
  *completed/intentional* work — it advances only on the agent's own real
  commits and `gt done`, and is what becomes a PR. It stays clean.
- The **checkpoint ref** (`refs/checkpoints/polecat/<name>`) is the resume
  source-of-truth for *in-progress/interrupted* work — force-updated every
  interval with the latest worktree state.

To de-risk tight interruption windows, the polecat **checkpoints continuously**:
every `checkpoint_interval` (and on quiescence) `gt-worker-agent` commits +
pushes the **checkpoint ref** through the proxy, so the host is never more than
one interval stale. **Recovery resets the worktree from the checkpoint ref**
(not the branch), then the agent continues; on `gt done` the real work is
already on the clean branch. This applies to **non-preemptible** workers too (it
guards worker and host crashes, not just preemption).

Checkpointing must stay cheap and must not pollute the branch or bloat the repo:

- **Disposable, non-accumulating commits.** Each checkpoint is an **orphan
  commit** (no parent) — or a single commit always re-parented on the branch tip
  — and the ref is **force-moved**, never appended. Old checkpoint commits
  become unreferenced and are reclaimed by periodic `git gc --prune` on
  `.repo.git`, so a long session does not accumulate a 5-minute-granularity
  commit chain. (Unchanged blobs/trees are shared by content-addressing; only
  changed trees cost objects.)
- **Tracked-only, gitignore-respecting staging.** Stage with the repo's
  `.gitignore` honored and **do not** blindly `git add -A` over untracked trees
  — avoid committing `node_modules`, build/compiler caches, and logs (bandwidth
  + bloat through the proxy). Untracked-but-wanted files are the rare exception,
  handled explicitly.
- **Quiescence guard.** Trigger a checkpoint only when the worktree is
  momentarily settled (no in-flight writes for a short debounce), so a
  half-written file is not captured mid-flush. The interval is a ceiling, not a
  hard metronome.
- **Offline checkpoint spool (provider-defined fallback).** Normally checkpoints
  push to the host `.repo.git` via the proxy. But if the orchestrator is offline
  at exactly the wrong moment — an interruption while the laptop is asleep — the
  final, unpushed delta would die with an ephemeral worker. So when the proxy is
  unreachable, `gt-worker-agent` spools the checkpoint (a git bundle of the
  checkpoint ref) to a **provider-defined durable location** that outlives the
  worker. On resume, the replacement worker pulls the spooled bundle when the
  host `.repo.git` is behind it, **and immediately re-pushes that state to
  `.repo.git` via the proxy** (or the daemon ingests the bundle directly) so the
  host reconverges and the spool is not left as a second source-of-truth. This
  is optional per provider and only engages on a proxy outage. (EC2: an object
  store; socket: the worker's own persistent disk — see the provider specs.)

### 9.3 Interruption (provider-signalled preemption or shutdown)

Some providers can take the worker away (capacity reclamation, host shutdown); some
can only be asked to stop (an explicit shutdown message). Either way,
interruption is handled **on the worker** by `gt-worker-agent`, which watches
the **provider's interruption signal** — a metadata endpoint to poll, a process
signal, or a message on the session connection; each provider spec defines its
signal and its warning window. For workers with no preemption (on-demand cloud,
persistent machines), the watcher is simply inert.

**Shutdown sequence** (identical across providers, because everything is one
worker host under one supervisor): (1) stop the agent — `gt-worker-agent`
signals the agent process directly (native mode) or `docker stop`s the work
container (container mode); no cross-container PID-namespace dance is needed.
(2) Flush the final small delta to the checkpoint ref (same tracked-only,
gitignore-respecting staging as §9.2 — small because of continuous
checkpointing). (3) Exit / report shutdown complete, per provider. If the final
flush fails, at most one `checkpoint_interval` is lost.

### 9.4 Resume

Two distinct re-entry cases, distinguished by whether the worker session is
still alive (reconciled with the identity-based discovery in §5):

- **Worker still alive** (e.g. the *daemon* restarted, the *worker* did not):
  `Discover` finds the live worker for the identity, and `Provision`
  **reattaches** to it — no new worker, no reprovision. This is the §5
  discovery path.
- **Worker gone** (preemption, worker crash, session lost): no live worker is
  found, so `Provision` acquires a **fresh** environment that resumes from the
  checkpoint.

In the gone case the polecat never reached `gt done`; its bead stays `working`
with a stale heartbeat, and Witness's existing **restart-first** policy drives
the re-provision. The fresh environment MUST `git fetch` and **reset its
worktree to the checkpoint ref** (`refs/checkpoints/polecat/<name>`, the
in-progress source of truth per §9.2 — not the polecat branch, which only holds
completed/`gt done` work), then re-attach to the **same** bead — so interrupted
work resumes from the last checkpoint rather than restarting or losing it.
`Provision` is idempotent across both cases: reattach if live,
resume-from-checkpoint if not.

### 9.5 Teardown & zombie cap

- After `gt done` (or idle), the reaper waits `cooldown`, then calls
  `Teardown()` (provider-defined release: destroy the sandbox, or end the
  session and clean up on a persistent machine).
- `max_runtime` is an absolute wall-clock cap checked in `reapIdlePolecat`,
  independent of heartbeat freshness, to kill busy zombies. **On expiry the
  reaper does a graceful flush first, not an immediate kill:** it sends a
  flush/stop signal over the provider channel (the same path as an interruption)
  so `gt-worker-agent` stops the agent and pushes a final checkpoint (and
  surfaces tail logs), waits a short grace window (e.g. 60–120s), and only then
  forces `Teardown`. If the grace window expires it tears down anyway. This
  preserves partial progress on a timed-out long task instead of discarding
  everything since the last checkpoint.
- **Worker-side self-release watchdog (cost/safety backstop).** The host reaper
  is not trustworthy for teardown when the orchestrator is a laptop that may
  sleep or lose connectivity — a missed teardown means a worker running (and, on
  billed providers, billing) indefinitely. So `gt-worker-agent` also enforces
  its own limits **locally**: it self-releases (after a final checkpoint) when
  it reaches `max_runtime`, **or** when it loses contact with the
  orchestrator/control plane for a dead-man's-switch interval (a few ×
  `checkpoint_interval`). The host reaper is the primary, graceful path; the
  worker-side watchdog guarantees the session ends even if the laptop never
  comes back. What "self-release" means is provider-defined: an ephemeral cloud
  worker terminates itself; a persistent machine stops the session and preserves
  itself (see the provider specs).

### 9.6 Orchestrator connectivity & dynamic host

The orchestrator is often a developer laptop — dynamic IP, sleep, Wi-Fi changes,
transient drops. The worker's link to the host proxy is therefore *not*
reliable, and the design must tolerate it:

- **Stable host address.** The worker must not pin a raw laptop IP. It reaches
  the proxy via a **stable hostname** (Tailscale / VPN / dynamic-DNS), or the
  daemon updates the worker's proxy endpoint out-of-band over the provider
  channel when its address changes. `GT_PROXY_URL`'s host half is resolved
  through that stable name.
- **Local checkpoint queueing.** `gt-worker-agent` commits checkpoints to the
  local checkpoint ref regardless of connectivity and **retries the push with
  exponential backoff**; a push outage delays durability but never blocks the
  agent or loses the local commit. `gt`/`bd` calls that need the host degrade
  gracefully (retry / surface a clear "control plane unreachable" rather than
  hang indefinitely).
- **Debounced host-side reaping.** When the *host's own* network was recently
  down (the daemon can detect its own offline window), the reaper applies a
  generous grace/debounce before treating a stale remote heartbeat as death —
  otherwise a laptop sleeping for five minutes would mass-reap healthy workers
  and re-provision needlessly.

---

## 10. Docker / nested-container workloads

Agents frequently need a **real Docker daemon** — `docker build`, `docker
compose up` to bring up dependent services for integration tests,
testcontainers, etc. This is a **capability** a provider does or does not offer,
and a major criterion in provider selection.

Where the worker runs `dockerd`: in `container` mode the work container is
started with `/var/run/docker.sock` bind-mounted, so the agent's
`docker`/`docker compose` talks to the **worker-host daemon** and spins up
*sibling* containers (the standard "Docker-outside-of-Docker" pattern). In
`native` mode the agent simply uses the host daemon directly. Either way,
compose stacks, image builds, and testcontainers work.

**Security note (all providers):** a bind-mounted Docker socket is effectively
worker-host root. Whatever ambient credentials or identity the worker host
holds, a container escape via the socket can reach. Single-tenant ephemerality
limits *blast radius* but does not stop credential theft within the rig's own
run — and on persistent machines the compromise can outlive the session.
Required mitigations, in provider-appropriate form:

1. **Block worker credential endpoints from the container network** where the
   provider exposes any (cloud metadata services and the like) — and understand
   the limits: network-level blocks require bridge networking (§6.1.1) and **do
   not contain a Docker-socket-holding agent**, whose spawned containers
   originate traffic as the host daemon. Each provider spec details its
   endpoints and mitigations.
2. **Mandatory (not deferred) hardening for untrusted code** — rigs in
   `sandboxed` mode or running untrusted PRs MUST use rootless dockerd / nested
   userns (or skip the socket entirely); "single-tenant, acceptable" reasoning
   only holds for *trusted* rigs. Untrusted + raw host socket is not a safe
   combination on any provider.
3. **Least-privilege worker identity** — scope whatever platform identity the
   worker holds to exactly what that rig needs, so a stolen credential is
   narrowly bounded.

**Provider support:**

| Provider | Docker capability |
|---|---|
| `local` | Host daemon (today's behavior). |
| [EC2](remote-polecat-execution-ec2.md) | **Supported** — the worker image runs `dockerd`; see that spec's §10 for metadata-endpoint and UID mitigations. |
| [Socket](remote-polecat-execution-socket.md) | **Supported if the worker machine runs `dockerd`** — declared in the worker's capability handshake; see that spec's security model. |
| AWS Fargate (secondary, deferred) | **Not supported** — no Docker daemon; see the EC2 spec's Fargate appendix. |

**Backend gating.** `requires_docker: true` (§4) makes preflight (§6.3) reject
any provider/worker that cannot supply a daemon — so a Docker-needing rig can
never be silently scheduled where the daemon is absent. This makes "needs
Docker" an explicit, validated backend-selection criterion alongside the iOS
"needs local" case.

---

## 11. Implementation phases

**Tier 1 — config + safety rails (no provider dependency; ships value alone):**
1. `RigSettings.Execution` block (§4) + version bump — shared fields + opaque
   provider extension.
2. Absolute `max_runtime` cap in `reapIdlePolecat` (the genuine §9.5 gap).
3. Auto cert-issuance (CN `gt-<rig>-<name>`), the **CSR-signing CA primitive**
   (§7.2), and the `GT_PROXY_*`/`GIT_SSL_*` env contract — wires the *existing*
   proxy into the spawn path.

**Tier 2 — agnostic core:**
4. `ExecutionBackend` interface + `LocalBackend` (refactor today's path behind
   it; no behavior change), including the `BuildStartupCommand` →
   `WrapCommand` delegation (§5).
5. `gt-worker-agent` as a provider-neutral program: CSR flow, local relay, work
   env preparation (container/native), checkpoint loop (§9.2), shutdown
   sequence + watchdog (§9.3, §9.5) — with the provider channel and interruption
   signal behind small internal interfaces.
6. Heartbeat-only liveness for remote backends (§8.1) and remote session-env
   injection (§7.4).

**Tier 3 — first providers (independent of each other; either can ship first):**
7. **[EC2 provider](remote-polecat-execution-ec2.md)** — provisioning, worker
   image, cert-over-provider-channel, exec channel, interruption, egress
   postures; phased per that spec.
8. **[Socket provider](remote-polecat-execution-socket.md)** — `gt-worker-client`
   service, enrollment + session protocol, exec streaming; phased per that spec.

**Tier 4 — optimizations + secondary providers:**
9. Pre-warmed idle worker pools per remote rig to hide cold-start latency.
10. Secondary providers (e.g. AWS Fargate — see the EC2 spec's appendix).

Each tier is independently testable; Tier 1 is useful before any remote provider
exists, and Tier 2 is fully exercisable against `LocalBackend`.

---

## 12. Open questions (core)

Provider-specific questions live in the provider specs. Universal:

1. **Provision latency vs. dispatch.** Acquiring a worker takes
   seconds-to-minutes in the dispatch path (varies by provider). Accept
   synchronously for v1; pre-warming (Tier 4) is the optimization.
2. **Default dev image scope.** Which toolchains ship in the gastown default
   work image vs. left to custom images?
3. **Egress gateway abstraction (§7.3).** Should the `gateway` mode's provider
   integration be an interface (one Zero Trust product first, others later —
   Tailscale, a squid/HTTP filtering proxy), and what is the default `gateway`
   policy (a curated registry allowlist vs. allow-all-but-log)?
4. **Sandboxed-mode dependency story.** For `sandboxed` rigs that still need
   packages, standardize on a pull-through cache/internal mirror vs. deps
   pre-baked in the image/worker base? (Mechanisms are provider-specific; the
   policy choice is shared.)
5. **Docker socket hardening (§10).** Default to a bind-mounted host socket
   (single-tenant) or invest in rootless dockerd / nested userns as the default
   for defense-in-depth against a malicious agent escaping to worker-host root?
6. **Distroless work images (§6.2).** Confirm whether an exec channel can drive
   a shell injected by absolute path into a `/bin/sh`-less image, or whether v1
   simply requires `/bin/sh`. Note a second pitfall beyond the exec path:
   distroless images lack `glibc` and the dynamic linker
   (`/lib64/ld-linux-x86-64.so.2`), so any injected shell/binary **must be fully
   statically linked** (e.g. static `busybox`/`toybox`) to run at all — a
   dynamically linked one fails with a missing-loader error. This pushes the v1
   stance further toward simply requiring `/bin/sh`.
7. **Worker sizing UX.** Providers with elastic sizing want `cpu`/`memory`
   hints; fixed machines have none. Should sizing hints live in the core schema
   (advisory, ignorable) or stay purely provider-specific (current position:
   provider-specific)?
