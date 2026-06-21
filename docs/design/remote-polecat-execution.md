# Remote Polecat Execution (AWS spot backends)

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

This document specifies a per-rig, pluggable **execution backend** that can
provision an ephemeral AWS spot instance (EC2 Spot or Fargate Spot), launch a
polecat inside it, route all control-plane and git traffic back to the host over
the existing mTLS proxy, preserve work across spot interruptions, and tear the
instance down after completion to conserve cost.

### Goals

1. **Per-rig execution host config** — each rig declares where its polecats run.
2. **Provisioned ephemeral execution** — spot EC2 (from a Packer AMI) or Fargate
   Spot (ECS task), created on demand, auto-launching the polecat.
3. **mTLS control plane** — the remote polecat reaches `gt`/`bd` and git only
   through the host proxy; no direct Dolt auth, no GitHub access from the box.
4. **Snapshot/recovery + cleanup after cooldown** — work is recoverable if an
   instance dies; instances are torn down after a cooldown to save money.
5. **Configurable CPU/memory per rig** — beefier infra for projects that need it.
6. **Spot interruption handling** — react to AWS reclamation signals from inside
   the instance and flush work before shutdown.
7. **Zombie timeouts** — nuke instances that run too long.

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
  mTLS CLI relay: the container runs `gt-proxy-client` as `gt`/`bd`, which
  forwards argv to the host, where the real `gt`/`bd` execute against the host's
  Dolt. Git fetch/push are relayed to `~/gt/<rig>/.repo.git`. Identity is the
  client cert CN (`gt-<rig>-<name>`); a denylist gates subcommands.

This is exactly the control-plane transport this design needs. **Gap:** the proxy
is a standalone binary — it is *not* wired into the spawn path. Nothing issues
per-polecat certs or injects `GT_PROXY_*` env automatically today.

### 2.2 Path B — `Connection` / `MachineRegistry` / SSH (SCAFFOLDED, UNUSED)

`internal/connection/` defines a `Connection` interface, `LocalConnection`, a
`MachineRegistry` (`{name, type: local|ssh, host, key_path, town_path}`), and an
address parser for `[machine:]rig[/polecat]`. The SSH implementation is a stub
(`"ssh connections not yet implemented"`) and **nothing in dispatch reads it**.

We do **not** build on Path B: it models *named long-lived hosts*, the wrong
abstraction for per-task ephemeral spot instances that don't exist until
provisioned, and it has no transport story for beads.

### 2.3 Persistent-pool tension

`persistent-polecat-pool.md` deliberately *reuses* polecats (identity + worktree
survive across assignments; "no nuke in the happy path"). The spot cost model
wants the opposite: ephemeral sandboxes torn down per task. We resolve this in
§9: persistent **identity** (host-side), ephemeral **sandbox** (the instance),
with the polecat branch in `.repo.git` as the durable artifact.

---

## 3. Architecture

```
Orchestrator host                         AWS (per-rig backend)
┌────────────────────────────┐            ┌───────────────────────────────────┐
│ GasTown daemon             │            │ ECS Fargate Spot task / EC2 Spot   │
│  SpawnPolecatForSling      │            │                                    │
│   └─ ExecutionBackend      │ provision  │  gt-sidecar (known image)          │
│        .Provision() ───────┼───────────►│   • copies gt/bd + idle entrypoint │
│   └─ exec_wrapper tokens   │            │     into shared volume             │
│        from .Attach()      │            │   • spot-interrupt agent           │
│                            │            │   • checkpoint+push loop           │
│  gt-proxy-server  ◄────mTLS─────────────┤  work container (custom/default    │
│   /v1/exec  (gt/bd)        │            │     image) runs the agent          │
│   /v1/git/<rig> (.repo.git)│            │     against worktree on shared vol  │
│        │ async push        │            │                                    │
│        ▼                   │            │  origin = https://host/v1/git/<rig> │
│  GitHub (host-only)        │            │  no direct internet / Dolt / GitHub │
└────────────────────────────┘            └───────────────────────────────────┘
```

The remote polecat never contacts Dolt, GitHub, or the internet directly. All
control-plane and git traffic flows host ↔ proxy. The host pushes to GitHub.

---

## 4. Per-rig configuration

A new optional `Execution` block on `RigSettings`
(`internal/config/types.go`), loaded from each rig's `settings/config.json`.
Absent or `backend: "local"` → today's behavior (this is how iOS rigs stay
pinned to the host).

```jsonc
// settings/config.json
"execution": {
  "backend": "fargate_spot",          // "local" | "fargate_spot" | "ec2_spot"
  "region": "us-east-1",

  // resource sizing (req. #5)
  "cpu": "2",                          // vCPU (Fargate units / EC2 instance class)
  "memory": "8Gi",
  "ephemeral_storage_gb": 40,          // Fargate task storage (20–200); EC2 EBS

  // execution image (req. #2) — see §6 for the image contract
  "image": "123456789.dkr.ecr.us-east-1.amazonaws.com/my-dev-env:latest",
  "image_auth": { "type": "ecr" },     // see §7

  // agent (LLM) auth (req. — see §7)
  "agent_auth": { "mode": "bedrock_role" },

  // lifecycle (req. #4, #6, #7)
  "checkpoint_interval": "5m",         // continuous work checkpointing
  "cooldown": "10m",                   // delay before teardown after DONE
  "max_runtime": "4h"                  // absolute zombie cap
}
```

`RigSettings` is a versioned, pointer-block struct; adding `Execution
*ExecutionConfig` follows the established pattern (`MergeQueue`, `Review`,
`CodeGraph`, …) and bumps `CurrentRigSettingsVersion`.

---

## 5. The `ExecutionBackend` interface

```go
// Resolved per rig. local = no-op; fargate/ec2 provision real infra.
type ExecutionBackend interface {
    // Provision creates the execution environment and blocks until the agent
    // can be launched into it. Idempotent for resume (see §8). Returns the
    // handle the daemon uses for Attach/Teardown.
    Provision(ctx context.Context, spec PolecatSpec) (Endpoint, error)

    // WrapperTokens returns the dynamic exec_wrapper inserted by
    // BuildStartupCommand, e.g. ["aws", "ecs", "execute-command", ...,
    // "--command", "..."] for Fargate or an ssh/ssm command for EC2.
    WrapperTokens(ep Endpoint) []string

    // Teardown destroys the environment. Called by the reaper after cooldown,
    // on max_runtime expiry, or on explicit nuke.
    Teardown(ctx context.Context, ep Endpoint) error
}
```

- `LocalBackend` — `Provision`/`Teardown` no-ops; `WrapperTokens` returns nil
  (today's path, refactored behind the interface with **no behavior change**).
- `FargateSpotBackend` — first to ship (no AMI pipeline).
- `EC2SpotBackend` — adds a Packer AMI; for rigs needing prebaked/beefy infra.

`PolecatSpec` carries the resolved per-rig config (cpu/memory/image/auth) plus
the polecat identity (`<rig>/<name>`), so backends are config-driven, not
hard-coded.

### Wiring points

- **Provision hook:** inserted between `SpawnPolecatForSling` returning and the
  deferred `StartSession` call (`internal/cmd/polecat_spawn.go`) — a natural gap,
  since session start is already deferred. This is also where the daemon issues
  the per-polecat cert (CN `gt-<rig>-<name>`) and sets `GT_PROXY_*` / `GIT_SSL_*`.
- **Attach:** `WrapperTokens` feeds the existing `exec_wrapper` insertion in
  `BuildStartupCommand` (`internal/config/loader.go`). No change to the command
  builder itself.
- **Teardown + cooldown:** `killIdlePolecat` (`internal/daemon/daemon.go`) gains a
  `backend.Teardown()` call; a cooldown timestamp in the heartbeat makes the
  reaper wait before tearing down.
- **Zombie cap:** `reapIdlePolecat` gains an absolute `max_runtime`
  (wall-clock-since-spawn) check, independent of heartbeat freshness — today's
  reaper is idle-based and would not catch a busy-but-looping polecat.

---

## 6. Runtime image contract

The execution image (custom or default) is the dev environment the polecat works
in. To keep custom images pristine and decoupled from gastown releases, the
contract is split: **the image provides the toolchain and the agent runtime;
gastown injects everything else at launch.**

### 6.1 What the image MUST provide

1. **The agent runtime binary** named by the rig's resolved agent config
   (`RuntimeConfig.Command`): `claude`, or `codex`/`opencode`/`gemini`/etc. for
   custom-agent rigs. This must be on `PATH` (or at a path the resolved command
   names). gastown injects `gt`/`bd` but **not** the agent runtime — runtimes are
   large and version-sensitive and belong in the image.
2. **The project toolchain** the polecat needs (language runtimes, build tools,
   linters, test frameworks) — the reason to use a custom image at all.
3. **`git`** (used for the checkpoint/push loop against the mounted worktree).

### 6.2 What the image MUST NOT be expected to carry

1. **Gastown binaries** (`gt`, `bd`, the proxy client). Injected at launch (§6.4).
2. **A specific entrypoint or `CMD`.** gastown overrides the work container's
   entrypoint with an injected idle binary so the agent can be launched via
   `ecs execute-command` / ssm. The image need not provide a shell, `sleep`, or
   an init.
3. **An SSM agent** (Fargate provides the exec bits itself).
4. **Any credentials, certs, or Dolt config.** All injected as env/secrets (§7).
5. **Network egress to GitHub/Dolt/the internet.** The box is locked to the proxy.

### 6.3 Default image

When `execution.image` is empty, the backend uses a gastown-published default dev
image that satisfies §6.1 for the default agent (`claude`) plus a common
toolchain. The default image follows the **same** injection path — it does not
bake gastown binaries — so there is one code path, not a special case.

### 6.4 Injection mechanism (Fargate)

A single **known sidecar image** (in our ECR, **tag pinned to the orchestrator's
gt version** so the proxy client matches the server protocol) does double duty in
one ECS task:

```
ECS Task (Fargate Spot)
├── volume: gt-shared           # task-scoped ephemeral; holds binaries + worktree
├── container "gt-sidecar"      # known image, version-pinned
│     • on start: copies gt-proxy-client (as gt+bd), a static idle entrypoint,
│       and the checkpoint script into /opt/gt on gt-shared; marks HEALTHY
│     • then runs as the spot-interrupt agent + checkpoint/push loop over the
│       worktree (also on gt-shared)
│     • holds GT_PROXY_* / cert
└── container "work"            # custom or default image; carries nothing gastown
      • dependsOn gt-sidecar: HEALTHY        (binaries present before it matters)
      • entryPoint = injected idle binary    (no shell/sleep assumption)
      • CWD = worktree on gt-shared; PATH includes /opt/gt
      • host drives the agent via: aws ecs execute-command ... -- <resolved cmd>
```

Putting the **worktree on the shared volume** lets the sidecar (which we control,
with a proper signal handler and `git`) own checkpoint+push and interrupt
handling — so reliability does not depend on the user image being signal-aware or
carrying `git` tooling beyond §6.1.

> EFS is **not** used to deliver binaries (a versioned binary on EFS silently
> drifts from the orchestrator; the version-pinned sidecar image does not). EFS
> remains an option later for large *shared data* (e.g. build caches).

### 6.5 Injection mechanism (EC2)

The host controls the instance: the Packer AMI bakes (or userdata copies) the
gastown binaries and idle entrypoint; the worktree and binaries live on the
instance filesystem. The agent may run directly on the instance or in a container
with a host bind-mount. The same `gt/bd`-on-`PATH` + worktree contract applies.

### 6.6 Preflight

During `Provision`, validate that the resolved agent runtime (§6.1.1) resolves in
the image, and fail fast with a clear error if a custom-agent rig points at an
image lacking that runtime. Misconfiguration should surface at provision time,
not as a silently dead session.

---

## 7. Credentials & identity

A single per-backend IAM-role + secret-store layer covers **four** distinct
credential concerns. None of them place secrets in the image, the task-def
plaintext, the `execute-command` command string, or process args.

| Concern | Mechanism |
|---|---|
| **Control-plane identity** (proxy cert) | Daemon issues a short-lived leaf cert (CN `gt-<rig>-<name>`) at provision; injects `GT_PROXY_*` / `GIT_SSL_*`. Identity is the cert, enforced by `gt-proxy-server`. |
| **Image pull auth** | ECR (same/cross-account): task **execution role** IAM (`ecr:*`) + repo policy for cross-account. Other registries: task-def `repositoryCredentials` → Secrets Manager secret + execution-role `secretsmanager:GetSecretValue`. |
| **LLM auth** | Default `bedrock_role`: set `CLAUDE_CODE_USE_BEDROCK=1`, grant the task role `bedrock:InvokeModel` — **no key to inject**. Alternative `secret`: a task-def `secrets` entry sources `ANTHROPIC_API_KEY` (or provider var) from Secrets Manager into the container env. |
| **Agent AWS identity** | The task/instance IAM role (for any AWS work the agent itself does). |

### 7.1 LLM auth detail

`internal/config/env.go` already defines the per-provider auth allowlist
(`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, Bedrock AWS vars, Foundry, Vertex,
…) and emits them into the agent env. **But** it delivers them by reading the
orchestrator's shell env and baking them into the host `exec env` prefix — which
(a) does not propagate through `execute-command`/`ssm` into the container, and
(b) would leak the secret via CloudTrail / SSM session logs / process args if
inlined into the remote command. So for remote backends, the **names** are
accounted for but the **delivery** must change:

- Route auth vars through **backend container-env injection** (task-def `secrets`
  / instance profile), never the command line.
- Default remote rigs to **Bedrock-via-role** to sidestep secrets entirely; the
  task role then does triple duty (ECR pull + Secrets Manager + Bedrock invoke).

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

---

## 8. Model configuration carry-over

Rig model/agent config (`Agent` preset → `RoleAgents["polecat"]` → custom agent
→ `Command`/`Args`/`Env`) resolves **host-side in `BuildStartupCommand`** before
the wrapper is applied. The container needs none of gastown's agent config files;
only the resolved command + env cross. The config splits three ways:

| Surface | Examples | Crosses to container? |
|---|---|---|
| **Command + Args + prompt** | `claude --model claude-opus-4-8`, custom `Command: codex`, `--dangerously-skip-permissions`, `InitialPrompt` | **Free** — it *is* the wrapped command string. |
| **Env-based model config** (`rc.Env`) | `ANTHROPIC_BASE_URL` (Groq/MiniMax), `ANTHROPIC_MODEL`, `ANTHROPIC_DEFAULT_*_MODEL` | **Inject** — same boundary as auth. Non-secret → plain task-def env; secret → secret store. |
| **Agent runtime binary** | the binary `Command` names | **Must be in the image** (§6.1.1). |

So custom models carry over: CLI config for free, env config via the §7
injection path, and the runtime via the §6 image contract.

### 8.1 Liveness consequence

`GT_PROCESS_NAMES` (used by Witness/reaper to match `pane_current_command`) is
meaningless for remote: the host pane runs `aws`/`ssm-session-worker`, not the
agent. **Remote backends use heartbeat-based liveness only** (the
`.runtime/heartbeats/` mechanism via the proxy), not process-name matching. This
is required regardless of model config; custom-agent rigs just make it concrete.

---

## 9. Lifecycle, recovery, and interruption

### 9.1 Identity vs. sandbox (persistent-pool reconciliation)

For remote backends there is **no host-side worktree**. Persistent **identity**
(name, agent bead, CV) stays host-side; the **sandbox** is the ephemeral
instance; the durable artifact is the polecat branch in `~/gt/<rig>/.repo.git`.
This diverges from the reuse-the-worktree pool model and is carved out
explicitly for `backend != local`.

### 9.2 Recovery — git push via proxy (no storage snapshots)

The recoverable artifact is the **polecat branch**, pushed host-side. There is no
EBS/AMI snapshot lifecycle. To de-risk the tight interrupt window, the polecat
**checkpoints continuously**: every `checkpoint_interval` (and on quiescence) the
sidecar commits + pushes the branch through the proxy, so `.repo.git` is never
more than one interval stale. Recovery = re-provision + reset to branch tip.

### 9.3 Spot interruption — in-instance, per-backend

Interruption is handled **inside the instance**, and the AWS signal differs by
backend — so this is a per-backend in-instance component (shipped in the sidecar
image / AMI), **not** a method on the host-side interface:

- **Fargate Spot:** ECS sends an in-container **SIGTERM**, then SIGKILL after
  `stopTimeout`. Set `stopTimeout: 120` in the task-def (default is 30s). The
  sidecar (PID-1-capable) traps SIGTERM.
- **EC2 Spot:** there is no reliable advance SIGTERM. An in-instance **poller**
  watches IMDS `/spot/instance-action` (and optionally the rebalance
  recommendation, which fires earlier). The ~2-min window is best-effort.

On interrupt the handler: (1) SIGKILLs the agent to stop new writes, (2) flushes
the final small delta (`git add -A && commit && push` — small because of
continuous checkpointing), (3) exits. If the final flush fails, at most one
checkpoint interval is lost.

### 9.4 Resume

An interrupted polecat never reaches `gt done`; its bead stays `working` with a
stale heartbeat. Witness's existing **restart-first** policy re-provisions. The
resumed instance MUST `git fetch` and **reset to the pushed branch tip** before
starting, and re-attach to the **same** bead — so interrupted work resumes rather
than restarting. `Provision` is idempotent for this re-entry.

### 9.5 Teardown & zombie cap

- After `gt done` (or idle), the reaper waits `cooldown`, then calls
  `Teardown()`.
- `max_runtime` is an absolute wall-clock cap checked in `reapIdlePolecat`,
  independent of heartbeat freshness, to kill busy zombies. On expiry the reaper
  tears down the instance unconditionally.

---

## 10. Implementation phases

**Tier 1 — config + safety rails (no AWS dependency; ships value alone):**
1. `RigSettings.Execution` block (§4) + version bump — per-rig host/cpu/mem/image.
2. Absolute `max_runtime` cap in `reapIdlePolecat` (the genuine §9.5 gap).
3. Auto cert-issuance (CN `gt-<rig>-<name>`) + `GT_PROXY_*`/`GIT_SSL_*` injection
   at spawn — wires the *existing* proxy into the spawn path.

**Tier 2 — interface + first backend:**
4. `ExecutionBackend` interface + `LocalBackend` (refactor today's path behind
   it; no behavior change).
5. `FargateSpotBackend.Provision/Teardown`, the provision hook, the gt-sidecar
   image (§6.4), and the §7 credential wiring (ECR pull, agent_auth).
6. Blocking-pane wrapper + heartbeat-only liveness for remote (§8.1).

**Tier 3 — full lifecycle + second backend:**
7. Cooldown + `Teardown` from `killIdlePolecat`; continuous checkpoint loop (§9.2).
8. Per-backend spot-interrupt agents + resume logic (§9.3–9.4).
9. `EC2SpotBackend` + Packer AMI pipeline.

Each tier is independently testable; Tier 1 is useful before any cloud backend
exists.

---

## 11. Open questions

1. **Provision latency vs. dispatch.** Fargate cold start (image pull) is
   ~30–60s and sits in the dispatch path. Accept synchronously for v1;
   pre-warming a small idle pool per remote rig is a Tier 4 optimization.
2. **Default dev image scope.** Which toolchains ship in the gastown default
   image vs. left to custom images?
3. **Bedrock model parity.** Confirm target models (e.g. Opus 4.8) are available
   on Bedrock in the chosen region before defaulting a rig to `bedrock_role`;
   otherwise use `secret` + direct Anthropic API.
4. **Cross-account ECR.** Standardize on execution-role + repo policy, or an
   assumed pull role ARN in `image_auth`?
5. **Ephemeral storage ceiling.** Large monorepos may exceed Fargate's 200 GB
   task-storage cap — EC2-only for those rigs, or EFS-backed worktrees?
