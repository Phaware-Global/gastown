# Remote Polecat Execution (AWS, EC2-first)

> **Date:** 2026-06-21
> **Author:** crew
> **Status:** Proposal
> **Related:** sandboxed-polecat-execution.md, persistent-polecat-pool.md, proxy-server.md, federation.md

---

## 1. Problem Statement

Every polecat today runs on the orchestrator host: the daemon's `SessionManager`
execs the agent inside a tmux session under the user's UID, with direct loopback
access to Dolt, `.runtime/`, and mail. A single developer machine cannot sustain
10–20 simultaneous agent sessions without resource contention.

We want to **offload polecat execution to remote cloud instances** to increase
compute capacity, while keeping **certain rigs pinned to the orchestrator host**
(e.g. iOS development, which needs local provisioning profiles and signing keys).

This document specifies a per-rig, pluggable **execution backend**. The primary
target is **EC2** — it gives us full host control (most importantly a real Docker
daemon, so agents can run `docker build` / `docker compose` / testcontainers; see
§10), prebaked AMIs for fast warm starts, and arbitrary instance sizing. The
backend provisions an ephemeral instance, launches a polecat inside it, routes all
control-plane and git traffic back to the host over the existing mTLS proxy,
preserves work across interruptions, and tears the instance down after a cooldown
to conserve cost.

**Spot is the desired default** (cost), but the backend **also supports on-demand**
instances per rig (`instance_lifecycle`), for interruption-intolerant work or
predictable capacity. Fargate is retained as a **later, secondary** backend for
lightweight rigs that need neither Docker nor a custom host (§6.4).

### Goals

1. **Per-rig execution host config** — each rig declares where its polecats run.
2. **Provisioned ephemeral EC2 execution** — spot *or* on-demand, from a Packer
   AMI, created on demand, auto-launching the polecat.
3. **mTLS control plane** — the remote polecat reaches `gt`/`bd` and git only
   through the host proxy; no direct Dolt auth, no GitHub access from the box.
4. **Recovery + cleanup after cooldown** — work is recoverable if an instance
   dies; instances are torn down after a cooldown to save money.
5. **Configurable CPU/memory per rig** — beefier infra for projects that need it.
6. **Spot interruption handling** — react to AWS reclamation signals from inside
   the instance and flush work before shutdown (no-op for on-demand).
7. **Zombie timeouts** — nuke instances that run too long.
8. **Docker / nested-container support** — agents can use a real Docker daemon
   (the main reason EC2 is the primary backend; §10).

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

We do **not** build on Path B: it models *named long-lived hosts*, the wrong
abstraction for per-task ephemeral instances that don't exist until provisioned,
and it has no transport story for beads.

### 2.3 Persistent-pool tension

`persistent-polecat-pool.md` deliberately *reuses* polecats (identity + worktree
survive across assignments; "no nuke in the happy path"). The ephemeral-instance
cost model wants the opposite: sandboxes torn down per task. We resolve this in
§9: persistent **identity** (host-side), ephemeral **sandbox** (the instance),
with the polecat branch in `.repo.git` as the durable artifact.

---

## 3. Architecture (EC2)

```
Orchestrator host                         AWS — EC2 instance (spot or on-demand)
┌────────────────────────────┐            ┌───────────────────────────────────────┐
│ GasTown daemon             │            │ EC2 instance (Packer AMI, gt-pinned)   │
│  SpawnPolecatForSling      │            │  dockerd · amazon-ssm-agent            │
│   └─ ExecutionBackend      │ RunInstances│                                       │
│        .Provision() ───────┼───────────►│  gt-node-agent  (host systemd service) │
│   └─ argv from             │            │   • redeems bootstrap token → cert     │
│        .WrapCommand()       │            │     in host tmpfs (§7.2)               │
│                            │            │   • LOCAL relay 127.0.0.1:9899         │
│  gt-proxy-server ◄──mTLS──── relay ◄────┤   • checkpoint+push loop               │
│   /v1/exec  (gt/bd)        │            │   • IMDS spot-interrupt poller (§9.3)  │
│   /v1/git/<rig> (.repo.git)│            │                                        │
│        │ async push        │            │  work (custom/default image) in Docker │
│        ▼                   │            │   • gt/bd + /opt/gt bind-mounted in     │
│  GitHub (host-only)        │            │   • worktree on EBS bind-mounted in     │
│                            │            │   • /var/run/docker.sock bind-mounted   │
└────────────────────────────┘            │     → agent can docker compose (§10)    │
                                          │  origin/GT_PROXY_URL → 127.0.0.1:9899   │
                                          │  no direct Dolt / GitHub / ctrl-plane   │
                                          │  (work-egress per network.mode, §7.3)   │
                                          └───────────────────────────────────────┘
```

The remote polecat never contacts Dolt, GitHub, or the gastown control plane
directly — all of that flows host ↔ proxy, and the host pushes to GitHub. Its
*own* outbound internet (package installs, APIs) is governed by a per-rig egress
posture (`sandboxed` / Zero Trust `gateway` / `open`); it also reaches a narrow
allowlist of AWS managed-service endpoints via VPC endpoints. See §7.3.

Because we own the EC2 host, the gastown binaries, relay, checkpointing, and
interrupt handling all live in a single host service (`gt-node-agent`) — no
sidecar container, no shared-volume copy, no cross-container signalling. The agent
runs in a Docker container so per-rig custom images and Docker-in-workflow both
work (§6.1, §10).

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
  "backend": "ec2",                    // "local" | "ec2" | "fargate" (later)
  "region": "us-east-1",

  // Purchasing model. Spot is the cost-optimized default; on_demand for
  // interruption-intolerant work or predictable capacity (req. — see §9.3).
  "instance_lifecycle": "spot",        // "spot" | "on_demand"
  "spot_max_price": null,              // optional cap; null = current on-demand price

  // Resource sizing (req. #5). Either name an instance_type directly, or give
  // cpu/memory and let the backend pick the cheapest matching type/class.
  "instance_type": "c7i.2xlarge",      // optional explicit type
  "cpu": "8",                          // else: vCPU…
  "memory": "16Gi",                    // …and memory → instance-type selection
  "ebs_gb": 80,                        // root/worktree EBS (gp3)

  // Execution environment (§6). Container mode (default) runs the agent in a
  // Docker container from this image; gt injects gt/bd + worktree + docker.sock.
  "exec_mode": "container",            // "container" (default) | "native"
  "image": "123456789.dkr.ecr.us-east-1.amazonaws.com/my-dev-env:latest",
  "image_auth": { "type": "ecr" },     // see §7
  "requires_docker": true,             // gate: forces EC2; rejects Fargate (§10)

  // agent (LLM) auth (see §7)
  "agent_auth": { "mode": "bedrock_role" },

  // network egress posture (see §7.3). "gateway" is the default for real dev
  // work (package installs etc.) routed through a Zero Trust egress gateway.
  "network": {
    "mode": "gateway",                 // "sandboxed" | "gateway" | "open"
    "gateway": {
      "provider": "cloudflare_zero_trust",
      "token_secret_arn": "arn:aws:secretsmanager:...:gt-cf-egress"
    }
  },

  // lifecycle (req. #4, #6, #7)
  "checkpoint_interval": "5m",         // continuous work checkpointing
  "cooldown": "10m",                   // delay before teardown after DONE
  "max_runtime": "4h"                  // absolute zombie cap
}
```

`RigSettings` is a versioned, pointer-block struct; adding `Execution
*ExecutionConfig` follows the established pattern (`MergeQueue`, `Review`,
`CodeGraph`, …) and bumps `CurrentRigSettingsVersion`.

**Spot vs. on-demand.** `instance_lifecycle: "spot"` (default) launches via the
EC2 spot market (`InstanceMarketOptions`), with the §9.3 interruption handling
armed. `"on_demand"` launches a normal instance: no reclamation, so the spot
poller is simply inert — but `cooldown`, `max_runtime`, and continuous
checkpointing still apply (they also guard against host crashes). A rig can switch
between the two with a one-line config change and no other behavioral difference.

---

## 5. The `ExecutionBackend` interface

```go
// Resolved per rig. local = no-op; ec2/fargate provision real infra.
type ExecutionBackend interface {
    // Provision creates the execution environment and blocks until the agent
    // can be launched into it. Idempotent for resume (see §9.4). Returns the
    // handle the daemon uses for WrapCommand/Teardown.
    Provision(ctx context.Context, spec PolecatSpec) (Endpoint, error)

    // WrapCommand takes the fully-resolved agent command (argv) and session env
    // and returns the complete argv the daemon should exec on the orchestrator
    // host to launch the agent remotely. The backend controls the ENTIRE
    // structure — it does not merely prepend a prefix (see note below).
    //   EC2:     aws ssm start-session … (a doc that runs `docker exec -e … <argv>`)
    //   Fargate: aws ecs execute-command … --command "<argv>"
    //   Local:   the argv unchanged (today's behavior).
    WrapCommand(ep Endpoint, agentArgv []string, env map[string]string) []string

    // Teardown destroys the environment. Called by the reaper after cooldown,
    // on max_runtime expiry, or on explicit nuke.
    Teardown(ctx context.Context, ep Endpoint) error
}
```

> **`BuildStartupCommand` must be refactored — the prefix model is insufficient.**
> Today `BuildStartupCommand` (`internal/config/loader.go`) builds the startup
> string by appending the agent command as **trailing positional args** to the
> `ExecWrapper` prefix (`exec env VAR=val … <prefix> <agent> <args>`). That works
> for local wrappers (`exitbox`, `sudo`) that take a trailing command, but **breaks
> for remote backends**: `aws ssm start-session` needs a document + parameters and
> `aws ecs execute-command` needs the command in `--command "…"`, not as trailing
> argv. So the existing static `ExecWrapper []string` (a pure prefix) is replaced,
> for remote backends, by `ExecutionBackend.WrapCommand`, which receives the
> resolved agent argv + env and returns the full argv — letting each backend place
> the command (and env, §7.4) in the slot its launcher actually requires. Local
> rigs keep the prefix path unchanged.

- `LocalBackend` — `Provision`/`Teardown` no-ops; `WrapCommand` returns the agent
  argv unchanged (today's path, refactored behind the interface, no behavior change).
- `EC2SpotBackend` — **the primary, first-to-ship cloud backend.** Provisions a
  spot or on-demand instance from a Packer AMI. Despite the name it honors
  `instance_lifecycle` for both purchasing models.
- `FargateBackend` — **later, secondary** (§6.4). For lightweight rigs needing
  neither Docker nor a custom host. Supports `FARGATE_SPOT` and `FARGATE`
  (on-demand) capacity providers.

`PolecatSpec` carries the resolved per-rig config (lifecycle / sizing / image /
auth / exec_mode) plus the polecat identity (`<rig>/<name>`), so backends are
config-driven, not hard-coded.

### Endpoint discovery — surviving a daemon restart

`Endpoint` MUST be reconstructable from AWS, not just from daemon memory, because
the daemon can crash or restart while remote instances are still running. Every
backend therefore **tags its AWS resources** (EC2 instance / ECS task) with the
polecat identity (`gt:rig`, `gt:polecat`, `gt:session`) at `Provision`. On startup
the daemon (and `Teardown`) re-discovers live endpoints by listing resources
filtered on those tags, rather than persisting endpoint handles locally. This is
what makes `Provision` idempotent for resume (§9.4) and prevents orphaned, billable
instances after a crash. The reaper additionally sweeps for tagged instances with
no corresponding live agent bead and tears them down.

### Wiring points

- **Provision hook:** inserted between `SpawnPolecatForSling` returning and the
  deferred `StartSession` call (`internal/cmd/polecat_spawn.go`) — a natural gap,
  since session start is already deferred. This is also where the daemon mints the
  per-polecat cert (CN `gt-<rig>-<name>`) and arranges its **secure delivery** to
  the instance (§7.2 — never as plaintext that lingers in cleartext metadata).
- **WrapCommand:** replaces the static `ExecWrapper` prefix-append in
  `BuildStartupCommand` (`internal/config/loader.go`) for remote backends — the
  command builder is refactored to delegate final-argv construction (command +
  env placement) to the backend (see the note in §5 and §7.4). Local rigs keep the
  prefix path.
- **Teardown + cooldown:** `killIdlePolecat` (`internal/daemon/daemon.go`) gains a
  `backend.Teardown()` call; a cooldown timestamp in the heartbeat makes the
  reaper wait before tearing down.
- **Zombie cap:** `reapIdlePolecat` gains an absolute `max_runtime`
  (wall-clock-since-spawn) check, independent of heartbeat freshness — today's
  reaper is idle-based and would not catch a busy-but-looping polecat.

---

## 6. EC2 execution model

The EC2 backend runs the polecat in one of two modes:

- **`native` (simplest):** the agent runs directly on the instance; the AMI *is*
  the dev environment. Lowest overhead, full Docker, but the toolchain is the AMI.
- **`container` (default; per-rig toolchains):** the agent runs in a Docker
  container from the rig's `image`, with gastown bits and the worktree bind-mounted
  in from the host. This preserves custom images per rig *and* gives Docker (§10).

Because we own the instance, gastown is delivered by **host bind-mounts**, not
container gymnastics: a single host service (`gt-node-agent`) holds the cert, runs
the relay, and owns checkpointing/interrupts. Nothing about the proxy, cert, or
control plane depends on what the work image contains.

### 6.1 Host injection (the primary mechanism)

The Packer AMI is **version-pinned to the orchestrator's gt release** so the proxy
client matches the server protocol.

The AMI carries **`git`** as well — host-side checkpointing (§9.2) runs `git` in
`gt-node-agent`, so it is an AMI requirement independent of whether the *work
image* ships git (the Fargate sidecar likewise bundles git for the same reason).

```
EC2 instance (AMI: dockerd · amazon-ssm-agent · gt-node-agent · gt/bd · git · idle bin)
│
├── gt-node-agent.service   (systemd; the host-side gastown supervisor)
│     • redeems the bootstrap token → cert/key in HOST tmpfs (§7.2)
│     • runs the LOCAL relay; terminates mTLS to the host proxy (:9876) upstream
│     • runs the checkpoint+push loop over the worktree on EBS
│     • runs the IMDS spot-interrupt poller (§9.3)
│     • prepares the env once relay + cert are up — container mode: `docker run`
│       the IDLE work container (idle entrypoint, no agent yet); native: nothing
│
└── agent PROCESS — launched on demand by the ORCHESTRATOR, not gt-node-agent:
      WrapCommand → aws ssm start-session → (container: `docker exec`) -- <argv>
      • bind mounts (container): /opt/gt (gt/bd + idle) · EBS worktree · docker.sock
      • env: GT_PROXY_URL + git origin → the host relay (address per §6.1.1)
      • holds NO cert/key — mTLS is gt-node-agent's job
```

**mTLS termination lives entirely in `gt-node-agent` on the host.** The agent's
`gt`/`bd`/git talk to a plaintext local relay; the host service adds the client
cert and forwards over mTLS to the host proxy. **The private key never enters the
work container or its env.** The hop is instance-internal and never leaves the box.
Two distinct ports avoid confusion: the local relay is `…:9899`; the host proxy
(mTLS upstream) is `:9876`.

#### 6.1.1 Container networking — how the agent reaches the relay

`127.0.0.1` inside a bridge-network container is the *container's* loopback, **not**
the host — so a relay bound to host `127.0.0.1:9899` is unreachable from a default
bridge container. Two supported wirings (the backend picks one and sets
`GT_PROXY_URL` / git `origin` to match):

1. **Host networking** (`--network host`, default **for trusted rigs only**). The
   container shares the host network namespace, so `127.0.0.1:9899` *is* the relay.
   Simplest, but it **defeats the IMDS isolation mitigations** (§10): with no bridge
   and no routing hop, the container's traffic is indistinguishable from the host's,
   so neither the bridge `iptables` drop nor the IMDSv2 hop-limit can keep it away
   from `169.254.169.254`. **Acceptable only for trusted rigs.**
2. **Bridge + host-gateway** (**required for `sandboxed` / untrusted-code rigs**).
   Keep bridge isolation: bind the relay to the docker bridge gateway (or
   `0.0.0.0:9899`, firewalled to the bridge subnet) and start the container with
   `--add-host=host.docker.internal:host-gateway`; the agent reaches the relay at
   `http://host.docker.internal:9899`. This is the mode in which the §10 IMDS
   defenses actually work, so untrusted rigs **must** use it (preflight enforces
   this pairing).

So the networking default is **trust-dependent**, not unconditional: host
networking for trusted rigs (simplicity), bridge for untrusted ones (so IMDS
hardening is effective). `native` mode has no container; the agent uses
`127.0.0.1:9899`, and IMDS must be locked down at the instance level instead (§10).

#### 6.1.2 Startup ordering & command construction

Two distinct lifecycle steps, by two different actors:
1. **`gt-node-agent` prepares the environment** (systemd/Docker ordering, no
   health-check gymnastics): redeem the cert, confirm the relay is listening (it
   probes the relay itself), and — in container mode — `docker run` the **idle**
   work container (running only the injected idle entrypoint; no agent yet).
2. **The orchestrator launches the agent process** on demand via `WrapCommand` →
   `aws ssm start-session` → (container) `docker exec` into the prepared container.
   This is the same blocking-pane model as local; `gt-node-agent` never starts the
   agent itself — it only readies the box and supervises checkpoint/interrupt.

> **Command construction (avoid an injection footgun).** The remote command
> (`WrapCommand` → SSM → `docker exec -- …`) is built from the tokenized agent argv
> (§5, §8) with the same `ShellQuote` discipline `BuildStartupCommand` already
> applies (`internal/config/loader.go`): every token individually shell-quoted,
> config-derived parts (model flags, custom-agent args, the free-form
> `InitialPrompt`) treated as **untrusted data to be quoted, never interpolated
> raw** (pass the prompt as a single quoted arg or via stdin/file). Session env is
> injected per §7.4, not via the orchestrator's local `exec env`.

### 6.2 Image contract (container mode only)

In `container` mode the image is the dev environment and **carries only the
toolchain and agent runtime; gastown injects the rest from the host.** Concisely:

**MUST provide:** (1) the **agent runtime binary** the rig resolves to (`claude`,
`codex`, …) on `PATH`; (2) the **project toolchain** (language runtimes, build/test
tools — the reason for a custom image); (3) a **POSIX shell (`/bin/sh`)** — not
because `docker exec` itself uses one (it `execve`s the binary directly), but
because the SSM/exec **string** interface means `WrapCommand` delivers the agent
as a single shell-quoted command line that we run as `sh -c "<argv>"`; that, plus
interactive sessions, needs `/bin/sh` (v1 requires it; distroless is a known
limitation — open question 8); (4) a **Docker client** *only if* the rig's
workflows call `docker`/`docker compose` (§10).

**MUST NOT be expected to carry:** gastown binaries (`gt`/`bd`/proxy client —
bind-mounted at `/opt/gt`); a specific entrypoint/`CMD`/init (gastown supplies an
injected idle entrypoint); an SSM agent (SSM terminates on the host); any
credentials/certs/Dolt config (injected per §7; the proxy key never enters the
container); or any assumption about direct egress (control plane is proxied; the
agent's own internet is governed by `network.mode`, §7.3).

`git` is needed in the image only if the agent's *own* workflows call it; gastown's
checkpoint/push runs host-side in `gt-node-agent`, independent of the image.

**Default image:** when `image` is empty, the backend uses a gastown-published
default dev image satisfying the above for the `claude` agent plus a common
toolchain — same injection path, no special case.

### 6.3 Preflight

During `Provision`, validate (container mode): the resolved agent runtime resolves
in the image, and the image has `/bin/sh`. For any backend, reject `requires_docker`
on a Fargate rig (§10). Fail fast so misconfiguration surfaces at provision time,
not as a silently dead session.

### 6.4 Fargate backend (later, secondary)

> Deferred — for lightweight rigs that need **neither Docker (§10) nor a custom
> host** (e.g. pure code-edit/review polecats). Ships after EC2 (Tier 4). Captured
> here only so the interface stays backend-agnostic; skip on a first read.

Fargate gives no host and no shared host filesystem, so the host-bind-mount model
above does not apply. Injection instead uses a **version-pinned `gt-sidecar`
container + a shared task volume**: the sidecar copies `gt`/`bd` + the idle
entrypoint (+ `busybox sh` for distroless) onto the volume, redeems the bootstrap
token to a cert in its own tmpfs, and runs the relay + checkpoint/interrupt logic —
the in-task analogue of `gt-node-agent`. The work container `dependsOn` the
sidecar's **container health check** (binaries copied + relay listening; ECS only
honors `HEALTHY` when a health check is defined), shares the task network namespace
(so the relay is reachable without the §6.1.1 host-gateway dance), and is driven by
`aws ecs execute-command --command "<argv>"`. Interruption needs
`initProcessEnabled: true` (so `tini` is PID 1 and forwards SIGTERM), plus
`pidMode: task` and matching UID/`CAP_KILL` for the sidecar to signal the agent (or
a shared-volume STOP-marker fallback; §9.3). The extra moving parts — and the lack
of Docker — are exactly why Fargate is secondary.

---

## 7. Credentials & identity

A single per-instance IAM-role + secret-store layer covers **four** distinct
credential concerns. The invariant is that no **long-lived, high-value** secret
material — a private key, an API key, a password, the proxy client cert/key — ever
appears in the image, in instance/task metadata cleartext, in the remote command
string, or in process args. The **one deliberate, bounded exception** is the
single-use proxy bootstrap token (§7.2): it *is* secret material and *is* delivered
via launch metadata (EC2 userdata / task env, hence briefly visible via
`DescribeInstanceAttribute` / `ecs:DescribeTasks`), but it is low-value by
construction — single-use, short-TTL, inert once `gt-node-agent` redeems it for the
real cert seconds after boot. Where even that window is unacceptable, deliver the
token itself as a secret reference (§7.2 option 2). Secret **references** — Secrets
Manager / SSM ARNs — are expected and not sensitive; AWS resolves them at launch
without exposing the value.

| Concern | Mechanism |
|---|---|
| **Control-plane identity** (proxy cert) | Daemon mints a short-lived leaf cert (CN `gt-<rig>-<name>`) at provision and delivers it **securely** per §7.2 — never as lingering plaintext. Identity is the cert, enforced by `gt-proxy-server`. |
| **Image pull auth** | ECR (same/cross-account): the **instance profile** IAM role (`ecr:*`) + repo policy for cross-account. Other registries: `docker login` from a Secrets Manager secret the instance role can read. |
| **LLM auth** | Default `bedrock_role`: set `CLAUDE_CODE_USE_BEDROCK=1`, grant the instance role `bedrock:InvokeModel` — **no key to inject**. Alternative `secret`: source `ANTHROPIC_API_KEY` (or provider var) from Secrets Manager into the container env. |
| **Agent AWS identity** | The instance IAM role (for any AWS work the agent itself does). |

### 7.1 LLM auth detail

`internal/config/env.go` already defines the per-provider auth allowlist
(`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, Bedrock AWS vars, Foundry, Vertex,
…) and emits them into the agent env. **But** it delivers them by reading the
orchestrator's shell env and baking them into the host `exec env` prefix — which
(a) does not propagate through SSM / `docker exec` into the container, and (b)
would leak the secret via CloudTrail / SSM session logs / process args if inlined
into the remote command. So for remote backends, the **names** are accounted for
but the **delivery** must change:

- Route auth vars through **instance/container-env injection** (instance profile,
  Secrets Manager → container env), never the command line.
- Default remote rigs to **Bedrock-via-role** to sidestep secrets entirely; the
  instance role then does triple duty (ECR pull + Secrets Manager + Bedrock invoke).

A useful security property falls out: the key lives in the secret store, not the
orchestrator's shell — the remote path is *more* isolated than local.

`agent_auth`:

```jsonc
"agent_auth": { "mode": "bedrock_role" }                 // default; no secret
// or
"agent_auth": {
  "mode": "secret",
  "env_var": "ANTHROPIC_API_KEY",
  "secret_arn": "arn:aws:secretsmanager:...:gt-anthropic-key"
}
```

### 7.2 Secure proxy-cert delivery

The per-polecat client cert and its **private key** are the most sensitive
material in the system: they grant `gt`/`bd` and git access as that identity. The
key MUST NOT be injected as lingering plaintext (EC2 userdata and ECS task env are
both readable via the AWS API) **and should never travel over the network at all.**

**The orchestrator drives delivery over SSM — no inbound port on the laptop.**
Rather than expose a bootstrap listener on the orchestrator (problematic behind
NAT / on a dynamic laptop IP — and `:9876` rejects certless connections via
`ClientAuth: tls.RequireAndVerifyClientCert`, while `:9877` is the admin port,
`internal/proxy/server.go`), delivery is **orchestrator-initiated via SSM**, which
the daemon already has credentials for:

1. **CSR over SSM (preferred).** `gt-node-agent` (a) **generates the private key
   locally** in host tmpfs — it never leaves the instance — and (b) writes a
   **Certificate Signing Request** (CN `gt-<rig>-<name>`) to a known path. The
   daemon (c) retrieves the CSR and (d) returns **only the signed cert** — both
   directions over `aws ssm` (`send-command` / a session), bound to the
   single-use bootstrap token. **No private key crosses the wire, and the
   orchestrator opens no inbound port** (SSM is orchestrator-→-instance, so it
   traverses NAT and survives a dynamic host IP). The token is replay-resistant
   and single-use (burned on first redemption, short-TTL).
2. **Per-instance Secrets Manager secret (fallback).** The daemon writes the cert
   **+ key** to a short-TTL Secrets Manager secret scoped to the instance role;
   `gt-node-agent` fetches it at boot. This *does* transmit the key (and leaves it
   at rest in Secrets Manager for the instance lifetime), so it is strictly weaker
   than the CSR flow — prefer (1).

> **Ongoing relay traffic** (the `:9876` mTLS control plane) likewise avoids an
> inbound laptop port: either the instance dials a **stable host name** (Tailscale
> / VPN, §9.6), or the orchestrator establishes an **SSM port-forwarding session**
> so the relay tunnels orchestrator-→-instance. Both keep the laptop free of
> inbound firewall holes; the `:9876`/`RequireAndVerifyClientCert` config is
> unchanged, it just rides the tunnel.

The private key lives only in host tmpfs (never in the work container, and under
the CSR flow never on the wire), and the cert is short-lived (`proxy_cert_ttl`,
default 24h) so exposure is bounded even if an instance is compromised. The CA can
revoke a leaked serial via the proxy denylist.

### 7.3 Network model & egress posture

Two **orthogonal** network planes, governed separately:

- **Control plane** (`gt`/`bd`, git, beads) → **always** the host proxy, in every
  mode. This is about identity, not isolation: it is how the polecat reaches Dolt
  without DB auth. It never changes with the egress posture.
- **AWS control-plane allowlist** — the instance always needs scoped access to the
  managed services this design uses, ideally via **VPC endpoints / PrivateLink**
  (NAT fallback), with the security group denying everything else *not* otherwise
  permitted by the egress mode:

  | Destination | Why | Path |
  |---|---|---|
  | ECR (api + dkr) + S3 gateway | image pull | VPC endpoints |
  | SSM / SSMMessages / EC2Messages | SSM session/exec | VPC endpoints |
  | Secrets Manager | `secret`-mode auth, registry creds | VPC endpoint |
  | Bedrock runtime | `agent_auth.mode = bedrock_role` | VPC endpoint |
  | Host proxy (`:9876`) | all control-plane + git | direct (VPC / VPN / Tailscale) |

- **Work-egress plane** — the agent's *own* outbound internet (npm, PyPI,
  crates.io, the Go module proxy, apt mirrors, GitHub for dependencies, arbitrary
  HTTP APIs the task legitimately calls). This is what `network.mode` controls.

> **Why this matters:** a fully locked-down box would break `npm install`,
> `pip install`, `go mod download`, `apt-get`, and most real build steps. Total
> isolation is correct for *untrusted* work but is the wrong default for ordinary
> development. So egress is a **per-rig spectrum**, not a binary.

#### `network.mode`

1. **`sandboxed`** — no general egress; only the proxy + AWS allowlist above.
   Maximum isolation — the original goal of `sandboxed-polecat-execution.md`
   (prevent credential exfiltration / malicious-MCP reach-out). Dependencies must
   be **pre-baked into the image/AMI** or served from an **internal mirror /
   pull-through cache** reachable via a VPC endpoint (e.g. CodeArtifact, an
   S3-backed registry proxy). Use for high-sensitivity rigs or untrusted code.

2. **`gateway`** *(recommended default for dev work)* — full outbound internet,
   but **mediated by a Zero Trust egress gateway** rather than raw NAT. A
   Cloudflare Zero Trust setup (WARP / `cloudflared` running as a host service on
   the instance, with Gateway DNS/network/HTTP policies) lets legitimate package
   and API traffic through while it **enforces destination policy, blocks known-bad
   endpoints, and logs every flow** for audit/DLP. The happy medium: a real
   security posture — exfiltration is policed and observable — without crippling
   the agent. The gateway token is injected as a secret reference (§7), and
   `gt-node-agent` brings the tunnel up before the work container starts.

3. **`open`** — unrestricted NAT egress. Simplest, least safe; for fully trusted
   rigs where the gateway hop is unwanted. Allowed but never the default.

In **all** modes the control plane and git still flow through the gastown proxy —
only the *work-egress* plane differs. The shift from `sandboxed` to `gateway` is a
shift from **isolation-by-prevention** to **mediation-by-policy + observability**:
appropriate when the agent must reach real registries, but you still want every
byte of egress attributable and governed.

### 7.4 Session-env propagation

The agent needs gastown's session env to function. These fall into two groups:

- **Existing session vars** — `GT_ROLE`, `GT_SESSION`, `GT_ROOT`, `BD_ACTOR`,
  `GT_RIG`, `GT_POLECAT`, the Dolt host/port, etc. — produced today by `AgentEnv`
  (`internal/config/env.go`) and emitted as the `exec env VAR=val …` prefix in
  `BuildStartupCommand`.
- **New relay vars this design adds** — `GT_PROXY_URL` and the `GIT_SSL_*` group
  (and friends) that point gt/bd/git at the local relay (§6.1). These are **not**
  emitted by `AgentEnv` today; they are introduced by this work (set per the chosen
  container-networking mode, §6.1.1) and injected alongside the existing vars.

The problem is the same for both groups: the local `exec env` prefix **runs on the
orchestrator host and does not cross the boundary** — neither SSM nor `docker exec`
forwards the client's environment to the remote process. So a naive remote launch
would start the agent with **none** of these set, and `gt`/`bd` would fail to
resolve their role, rig, or workspace.

The backend therefore injects session env **remotely**, as part of `WrapCommand`
(§5) — never via the orchestrator's local `exec env`:

- **Container mode:** pass each var through `docker exec -e VAR=val …` (or an
  `--env-file` that `gt-node-agent` writes to the bind-mounted `/opt/gt` and
  references), so they are set in the agent's process, not the host's.
- **Native mode:** `gt-node-agent` writes them to an env file it `source`s (or
  prepends `env VAR=val …` to the launched argv) on the instance.

Split by sensitivity, consistent with §7.1:

- **Non-secret session env** (`GT_ROLE`, `GT_SESSION`, `GT_ROOT`, `BD_ACTOR`,
  `GT_PROXY_URL`, …) travels in the `WrapCommand` payload / env-file. These are not
  secrets; appearing in the SSM/`docker exec` invocation is acceptable.
- **Secret env** (`ANTHROPIC_API_KEY`, registry creds, the proxy cert/key) is
  **never** in the command payload — it is injected via the secret store / instance
  profile (§7.1–7.2) or, for the proxy cert, terminated entirely in `gt-node-agent`
  so it never reaches the agent at all.

This is why `WrapCommand` takes the env map (§5): the backend, not
`BuildStartupCommand`, is responsible for landing the session env in the remote
process by whatever mechanism its launcher requires.

---

## 8. Model configuration carry-over

Rig model/agent config (`Agent` preset → `RoleAgents["polecat"]` → custom agent →
`Command`/`Args`/`Env`) resolves **host-side in `BuildStartupCommand`** before the
wrapper is applied. The remote needs none of gastown's agent config files; only
the resolved command + env cross. The config splits three ways:

| Surface | Examples | Crosses to the remote? |
|---|---|---|
| **Command + Args + prompt** | `claude --model claude-opus-4-8`, custom `Command: codex`, `--dangerously-skip-permissions`, `InitialPrompt` | **Free** — it *is* the wrapped command string (§6.1.2). |
| **Env-based model config** (`rc.Env`) | `ANTHROPIC_BASE_URL` (Groq/MiniMax), `ANTHROPIC_MODEL`, `ANTHROPIC_DEFAULT_*_MODEL` | **Inject** — same boundary as auth: non-secret → plain container env; secret → secret store. |
| **Agent runtime binary** | the binary `Command` names | **Must be in the image/AMI** (§6.2). |

So custom models carry over: CLI config for free, env config via the §7 injection
path, and the runtime via the §6.2 image contract.

### 8.1 Liveness consequence

`GT_PROCESS_NAMES` (used by Witness/reaper to match `pane_current_command`) is
meaningless for remote: the host pane runs `aws ssm`/`session-worker`, not the
agent. **Remote backends use heartbeat-based liveness only** (the
`.runtime/heartbeats/` mechanism via the proxy), not process-name matching. This is
required regardless of model config; custom-agent rigs just make it concrete.

---

## 9. Lifecycle, recovery, and interruption

### 9.1 Identity vs. sandbox (persistent-pool reconciliation)

For remote backends there is **no host-side worktree**. Persistent **identity**
(name, agent bead, CV) stays host-side; the **sandbox** is the ephemeral instance;
the durable artifact is the polecat branch in `~/gt/<rig>/.repo.git`. This diverges
from the reuse-the-worktree pool model and is carved out explicitly for
`backend != local`.

### 9.2 Recovery — git push via proxy (no storage snapshots)

There is no EBS/AMI snapshot lifecycle; durability is host-side git. **Two refs
with two roles** (this is the source-of-truth split):

- The **polecat branch** (`polecat/<name>/<issue>`) is the artifact for
  *completed/intentional* work — it advances only on the agent's own real commits
  and `gt done`, and is what becomes a PR. It stays clean.
- The **checkpoint ref** (`refs/checkpoints/polecat/<name>`) is the resume
  source-of-truth for *in-progress/interrupted* work — force-updated every interval
  with the latest worktree state.

To de-risk the tight interrupt window, the polecat **checkpoints continuously**:
every `checkpoint_interval` (and on quiescence) `gt-node-agent` commits + pushes
the **checkpoint ref** through the proxy, so the host is never more than one
interval stale. **Recovery resets the worktree from the checkpoint ref** (not the
branch), then the agent continues; on `gt done` the real work is already on the
clean branch. This applies to **on-demand** instances too (it guards host crashes,
not just spot reclamation).

Checkpointing must stay cheap and must not pollute the branch or bloat the repo:

- **Disposable, non-accumulating commits.** Each checkpoint is an **orphan commit**
  (no parent) — or a single commit always re-parented on the branch tip — and the
  ref is **force-moved**, never appended. Old checkpoint commits become
  unreferenced and are reclaimed by periodic `git gc --prune` on `.repo.git`, so a
  long session does not accumulate a 5-minute-granularity commit chain. (Unchanged
  blobs/trees are shared by content-addressing; only changed trees cost objects.)
- **Tracked-only, gitignore-respecting staging.** Stage with the repo's `.gitignore`
  honored and **do not** blindly `git add -A` over untracked trees — avoid
  committing `node_modules`, build/compiler caches, and logs (bandwidth + bloat
  through the proxy). Untracked-but-wanted files are the rare exception, handled
  explicitly.
- **Tracked-only, gitignore-respecting staging.** Stage with the repo's `.gitignore`
  honored and **do not** blindly `git add -A` over untracked trees — avoid
  committing `node_modules`, build/compiler caches, and logs (bandwidth + bloat
  through the proxy). Untracked-but-wanted files are the rare exception, handled
  explicitly.
- **Quiescence guard.** Trigger a checkpoint only when the worktree is momentarily
  settled (no in-flight writes for a short debounce), so a half-written file is not
  captured mid-flush. The interval is a ceiling, not a hard metronome.

### 9.3 Spot interruption — in-instance (no-op for on-demand)

Interruption is handled **inside the instance** by `gt-node-agent`. It applies only
to `instance_lifecycle: "spot"`; for on-demand the poller runs but never fires.

- **EC2 Spot:** there is no reliable advance SIGTERM. `gt-node-agent` **polls IMDS**
  `/spot/instance-action` (and optionally the rebalance recommendation, which fires
  earlier). The ~2-min window is best-effort.

**Shutdown sequence** (much simpler than Fargate, because everything is one host
under one root): (1) stop the agent — `gt-node-agent` signals the agent process
directly (native mode) or `docker stop`s the work container (container mode); no
cross-container PID-namespace / `CAP_KILL` dance is needed. (2) Flush the final
small delta to the checkpoint ref (same tracked-only, gitignore-respecting staging
as §9.2 — small because of continuous checkpointing). (3) Exit. If the final flush
fails, at most one `checkpoint_interval` is lost.

> **Fargate (secondary) interruption** is more involved: ECS SIGTERMs every
> container at once, so the work container's idle entrypoint must trap/ignore
> SIGTERM (under `initProcessEnabled` `tini`), and the sidecar needs `pidMode: task`
> + matching UID/`CAP_KILL` (or a shared-volume STOP marker) to stop the agent
> before flushing. This asymmetry is another reason EC2 is primary.

### 9.4 Resume

An interrupted polecat never reaches `gt done`; its bead stays `working` with a
stale heartbeat. Witness's existing **restart-first** policy re-provisions. The
resumed instance MUST `git fetch` and **reset to the pushed branch tip** before
starting, and re-attach to the **same** bead — so interrupted work resumes rather
than restarting. `Provision` is idempotent for this re-entry (it finds no live
tagged instance for the identity and creates a fresh one that resumes the branch).

### 9.5 Teardown & zombie cap

- After `gt done` (or idle), the reaper waits `cooldown`, then calls `Teardown()`
  (`TerminateInstances`).
- `max_runtime` is an absolute wall-clock cap checked in `reapIdlePolecat`,
  independent of heartbeat freshness, to kill busy zombies. **On expiry the reaper
  does a graceful flush first, not an immediate kill:** it sends a flush/stop signal
  (via SSM, the same path as a spot interrupt) so `gt-node-agent` stops the agent
  and pushes a final checkpoint (and surfaces tail logs), waits a short grace
  window (e.g. 60–120s), and only then calls `TerminateInstances`. If the grace
  window expires it terminates anyway. This preserves partial progress on a
  timed-out long task instead of discarding everything since the last checkpoint.

### 9.6 Orchestrator connectivity & dynamic host

The orchestrator is often a developer laptop — dynamic IP, sleep, Wi-Fi changes,
transient drops. The remote instance's link to the host proxy is therefore *not*
reliable, and the design must tolerate it:

- **Stable host address.** The instance must not pin a raw laptop IP. It reaches the
  proxy via a **stable hostname** (Tailscale / VPN / dynamic-DNS), or the daemon
  updates the instance's proxy endpoint out-of-band (SSM Parameter Store / an SSM
  send-command) when its address changes. `GT_PROXY_URL`'s host half is resolved
  through that stable name.
- **Local checkpoint queueing.** `gt-node-agent` commits checkpoints to the local
  checkpoint ref regardless of connectivity and **retries the push with exponential
  backoff**; a push outage delays durability but never blocks the agent or loses
  the local commit. `gt`/`bd` calls that need the host degrade gracefully (retry /
  surface a clear "control plane unreachable" rather than hang indefinitely).
- **Debounced host-side reaping.** When the *host's own* network was recently down
  (the daemon can detect its own offline window), the reaper applies a generous
  grace/debounce before treating a stale remote heartbeat as death — otherwise a
  laptop sleeping for five minutes would mass-reap healthy instances and re-provision
  needlessly.

---

## 10. Docker / nested-container workloads

A major reason EC2 is the primary backend: agents frequently need a **real Docker
daemon** — `docker build`, `docker compose up` to bring up dependent services for
integration tests, testcontainers, etc.

- **EC2 (supported).** The AMI runs `dockerd`. In `container` mode the work
  container is started with `/var/run/docker.sock` bind-mounted, so the agent's
  `docker`/`docker compose` talks to the **host daemon** and spins up *sibling*
  containers on the instance (the standard "Docker-outside-of-Docker" pattern). In
  `native` mode the agent simply uses the host daemon directly. Either way,
  compose stacks, image builds, and testcontainers work.
  - *Security note:* a bind-mounted Docker socket is effectively host root, and the
    **host carries the instance IAM role** (ECR, Secrets Manager, Bedrock). A
    container escape via the socket (launch a privileged sibling, mount host `/`)
    reaches the host and can then hit **IMDS (`169.254.169.254`) to steal the role's
    temporary credentials.** Single-tenant ephemerality limits *blast radius* but
    does not stop credential theft within the rig's own run. Required mitigations:
    1. **Block IMDS from the container network** — host `iptables` dropping
       `169.254.169.254` from the docker bridge, and IMDSv2 with a hop limit of 1 so
       a container cannot reach it even via the gateway. **This requires bridge
       networking** (§6.1.1 option 2): under `--network host` there is no bridge and
       no extra hop, so both controls are bypassed — which is exactly why untrusted
       rigs MUST use bridge mode and `native` mode must instead lock IMDS down at the
       instance level (disable IMDS post-boot, or a host firewall scoped to
       `gt-node-agent`'s UID).
    2. **Mandatory (not deferred) hardening for untrusted code** — rigs in
       `sandboxed` mode or running untrusted PRs MUST use rootless dockerd / nested
       userns (or skip the socket entirely); the "single-tenant, acceptable"
       rationale only holds for *trusted* rigs.
    3. **Per-rig least-privilege IAM** — scope each rig's instance role to exactly
       the ECR repos / secrets / Bedrock models it needs, not one broad shared role,
       so a stolen credential is narrowly bounded.
    Depth-of-hardening beyond (1)–(3) is open question 9.
- **Fargate (NOT supported).** Fargate exposes no Docker daemon, forbids
  `privileged`, and cannot run Docker-in-Docker. So `docker build` / `docker
  compose` / testcontainers **do not work** on Fargate. The only partial path is to
  model required services (postgres, redis) as **additional containers in the same
  task** (shared localhost) — static, declared at provision time, no image builds,
  cannot run the repo's own compose file dynamically.

**Backend gating.** `requires_docker: true` (§4) forces the rig onto EC2 and makes
preflight (§6.3) reject a Fargate selection — so a Docker-needing rig can never be
silently scheduled where the daemon is absent. This makes "needs Docker" an
explicit, validated backend-selection criterion alongside the iOS "needs local"
case.

---

## 11. Implementation phases

**Tier 1 — config + safety rails (no AWS dependency; ships value alone):**
1. `RigSettings.Execution` block (§4) + version bump — per-rig backend / lifecycle
   / sizing / image / `requires_docker`.
2. Absolute `max_runtime` cap in `reapIdlePolecat` (the genuine §9.5 gap).
3. Auto cert-issuance (CN `gt-<rig>-<name>`) + secure delivery (§7.2) and the
   `GT_PROXY_*`/`GIT_SSL_*` env contract — wires the *existing* proxy into the
   spawn path.

**Tier 2 — interface + EC2 backend (the primary):**
4. `ExecutionBackend` interface + `LocalBackend` (refactor today's path behind it;
   no behavior change).
5. The Packer AMI (dockerd + amazon-ssm-agent + `gt-node-agent` + gt/bd + idle
   binary), version-pinned to the gt release.
6. `EC2SpotBackend.Provision/Teardown` honoring `instance_lifecycle` (spot **and**
   on-demand), the provision hook, instance-profile credential wiring (ECR /
   agent_auth), tag-based discovery.
7. `gt-node-agent`: bootstrap-token redemption, local relay, container launch with
   bind-mounts (`/opt/gt`, worktree, docker.sock), `exec_mode` container/native.
8. SSM-based `WrapCommand` (blocking-pane wrapper) + remote session-env injection
   (§7.4) + heartbeat-only liveness (§8.1).

**Tier 3 — full lifecycle:**
9. Cooldown + `Teardown` from `killIdlePolecat`; continuous checkpoint loop (§9.2).
10. IMDS spot-interrupt poller + resume logic (§9.3–9.4).
11. Network egress posture (§7.3): `sandboxed` (SG-only) → `gateway` (Zero Trust
    tunnel as a host service) → `open`.

**Tier 4 — secondary backend + optimizations:**
12. `FargateBackend` (§6.4) for lightweight no-Docker rigs (`FARGATE_SPOT` /
    `FARGATE`).
13. Pre-warmed idle instance pool per remote rig to hide cold-start latency.

Each tier is independently testable; Tier 1 is useful before any cloud backend
exists.

---

## 12. Open questions

1. **Provision latency vs. dispatch.** EC2 + AMI warm start is faster than a cold
   image pull, but still seconds-to-minutes in the dispatch path. Accept
   synchronously for v1; pre-warming (Tier 4) is the optimization.
2. **Default dev image / AMI scope.** Which toolchains ship in the gastown default
   image and the base AMI vs. left to custom images?
3. **Bedrock model parity.** Confirm target models (e.g. Opus 4.8) are available on
   Bedrock in the chosen region before defaulting a rig to `bedrock_role`;
   otherwise use `secret` + direct Anthropic API.
4. **Cross-account ECR.** Standardize on instance-role + repo policy, or an assumed
   pull-role ARN in `image_auth`?
5. **Instance sizing UX.** Prefer explicit `instance_type`, or `cpu`/`memory` →
   cheapest-matching-type selection (and across which families)? How to express
   GPU / arch (arm64 vs x86) needs?
6. **Egress gateway abstraction (§7.3).** Should `network.gateway.provider` be an
   interface (Cloudflare Zero Trust first, others later — Tailscale, a squid/HTTP
   filtering proxy), and what is the default `gateway` policy (a curated registry
   allowlist vs. allow-all-but-log)?
7. **Sandboxed-mode dependency story.** For `sandboxed` rigs that still need
   packages, standardize on a pull-through cache reachable by VPC endpoint
   (CodeArtifact, an S3-backed mirror) vs. deps pre-baked in the image/AMI.
8. **Distroless work images (§6.2).** Confirm whether `docker exec` (over SSM)
   can drive a shell injected by absolute path into a `/bin/sh`-less image, or
   whether v1 simply requires `/bin/sh`. Note a second pitfall beyond the exec
   path: distroless images lack `glibc` and the dynamic linker
   (`/lib64/ld-linux-x86-64.so.2`), so any injected shell/binary **must be fully
   statically linked** (e.g. static `busybox`/`toybox`) to run at all — a
   dynamically linked one fails with a missing-loader error. This pushes the v1
   stance further toward simply requiring `/bin/sh`.
9. **Docker socket hardening (§10).** Default to a bind-mounted host socket
   (single-tenant, ephemeral) or invest in rootless dockerd / nested userns for
   defense-in-depth against a malicious agent escaping to host root?
10. **On-demand fallback.** Should `instance_lifecycle: "spot"` optionally fall
    back to on-demand on `InsufficientInstanceCapacity`, or fail and retry spot?
