# Remote Polecat Execution — Provider: AWS EC2

> **Date:** 2026-07-11 (extracted from the 2026-06-21 unified design)
> **Author:** crew
> **Status:** Proposal
> **Core:** [remote-polecat-execution.md](remote-polecat-execution.md) — read it first; this spec assumes its architecture, interface, invariants, and lifecycle protocol.
> **Sibling:** [Local Network (Socket) provider](remote-polecat-execution-socket.md)

This spec defines the **EC2 execution provider**: ephemeral AWS EC2 instances
(spot or on-demand) provisioned per task from a Packer-built AMI. It is the
first cloud implementation of the core's `ExecutionBackend` interface.

**Why EC2 first (among cloud options):** full host control — most importantly a
real Docker daemon, so agents can run `docker build` / `docker compose` /
testcontainers (core §10) — prebaked AMIs for fast warm starts, and arbitrary
instance sizing. **Spot is the desired default** (cost), but the backend also
supports on-demand instances per rig (`instance_lifecycle`) for
interruption-intolerant work or predictable capacity. Fargate is retained as a
**later, secondary** AWS backend for lightweight rigs that need neither Docker
nor a custom host (Appendix A).

---

## 1. Interface mapping

| Core method | EC2 implementation |
|---|---|
| `Provision` | `RunInstances` (spot or on-demand per `instance_lifecycle`) from the Packer AMI; tag with identity; inject version-sensitive binaries over SSM (§3); drive the CSR cert exchange over SSM (§6); wait for `gt-worker-agent` to report the relay up. Reattach path: tag-discovery first (§2). |
| `WrapCommand` | `aws ssm start-session` with a session document that runs `docker exec -e … -- <argv>` (container mode) or the argv directly (native mode) (§5). |
| `Teardown` | Graceful flush signal over SSM, then `TerminateInstances` (§9). |
| `Discover` | `DescribeInstances` filtered on the `gt:rig` / `gt:polecat` / `gt:session` tags (§2). |

The **provider channel** (core §3) is SSM — IAM-authenticated `SendCommand` /
`start-session` targeting the exact instance ID the daemon launched. The
**provider interruption signal** (core §9.3) is IMDS spot-interruption polling
(§8).

## 2. Provisioning model

- **Purchasing model.** `instance_lifecycle: "spot"` (default) launches via the
  EC2 spot market (`InstanceMarketOptions`), with §8 interruption handling
  armed. `"on_demand"` launches a normal instance: no reclamation, so the spot
  poller is simply inert — but `cooldown`, `max_runtime`, and continuous
  checkpointing still apply (they also guard against host crashes). A rig can
  switch between the two with a one-line config change and no other behavioral
  difference.
- **Sizing.** Either an explicit `instance_type`, or `cpu`/`memory` hints from
  which the backend picks the cheapest matching type/class (open question 3).
  Worktree/root storage is gp3 EBS sized by `ebs_gb`.
- **Identity tags (core §5 "Endpoint discovery").** Every instance is tagged
  `gt:rig`, `gt:polecat`, `gt:session` at `RunInstances`. Daemon restarts and
  the orphan reaper re-find live instances via `DescribeInstances` on these
  tags — endpoint handles are never persisted locally. This is what makes
  `Provision` idempotent (reattach if a live tagged instance exists;
  resume-from-checkpoint otherwise, core §9.4) and prevents orphaned, billable
  instances after a daemon crash.

## 3. Worker image (Packer AMI)

**The AMI is a stable base, decoupled from the `gt` release cadence** (core
§6.1). Baking a new AMI for every `gt` update would make active development
painfully slow. Instead the AMI carries only slow-moving infrastructure, and the
version-sensitive binaries (`gt`, `bd`, the proxy client, and the
`gt-worker-agent` program itself) are **injected at boot over SSM** from the
orchestrator, which *is* the matching `gt` release. This guarantees the proxy
client matches the server protocol without ever rebuilding the AMI on a `gt`
bump; AMI rebuilds are reserved for base-OS/security updates. (Same SSM channel
as the cert flow, §6.)

The AMI is defined by a Packer HCL2 template in this repo:
[`packer/worker-ec2.pkr.hcl`](../../packer/worker-ec2.pkr.hcl).

**Baked into the AMI (slow-moving):**

- Base distro: **Amazon Linux 2023** (first-party SSM agent + Docker packaging;
  Ubuntu LTS is an acceptable alternative — open question 2).
- `dockerd` (enabled at boot) — the real Docker daemon (core §10).
- `amazon-ssm-agent` (enabled at boot) — the provider channel.
- `git` — host-side checkpointing (core §9.2) runs `git` in `gt-worker-agent`,
  so it is an AMI requirement independent of whether the *work image* ships git.
- A **`gt-agent` system user** (non-root, member of the shared `gt` group) — the
  dedicated UID required by the native/host-net IMDS firewall (§10).
- A thin **`gt-worker-agent` bootstrapper**: a systemd oneshot that creates
  `/opt/gt`, waits for the SSM-injected binaries, verifies them, and starts the
  `gt-worker-agent` service. The bootstrapper is dumb on purpose — all
  version-sensitive logic arrives at boot.
- Systemd unit stubs for the bootstrapper and `gt-worker-agent`.

**MUST NOT be baked:** `gt`/`bd`/proxy-client/`gt-worker-agent` binaries
(injected at boot); any certificate, key, token, or secret; any rig- or
polecat-specific configuration; the work image (pulled per rig at provision).

## 4. Configuration schema extension

EC2-specific keys live under the `ec2` key of the core `execution` block (core
§4). Annotated (JSONC — the real `settings/config.json` must be strict JSON):

```jsonc
"execution": {
  // ── core shared fields (core §4) ──
  "backend": "ec2",
  "exec_mode": "container",            // "container" (default) | "native"
  "image": "123456789.dkr.ecr.us-east-1.amazonaws.com/my-dev-env:latest",
  "requires_docker": true,             // gate: forces a dockerd-capable provider
  "network": { "mode": "gateway" },    // "sandboxed" | "gateway" | "open" (§7)
  "checkpoint_interval": "5m",
  "cooldown": "10m",
  "max_runtime": "4h",

  // ── EC2 provider extension ──
  "ec2": {
    "region": "us-east-1",

    // Purchasing model (§2). Spot is the cost-optimized default; on_demand for
    // interruption-intolerant work or predictable capacity.
    "instance_lifecycle": "spot",      // "spot" | "on_demand"
    "spot_max_price": null,            // optional cap; null = current on-demand price

    // Resource sizing (§2). Either name an instance_type directly, or give
    // cpu/memory and let the backend pick the cheapest matching type/class.
    "instance_type": "c7i.2xlarge",    // optional explicit type
    "cpu": "8",                        // else: vCPU…
    "memory": "16Gi",                  // …and memory → instance-type selection
    "ebs_gb": 80,                      // root/worktree EBS (gp3)

    // Registry auth for `image` (§6.3)
    "image_auth": { "type": "ecr" },

    // agent (LLM) auth (§6.2)
    "agent_auth": { "mode": "bedrock_role" },

    // gateway-mode egress config (§7); only read when network.mode = "gateway".
    // v1 accepts only "cloudflare_zero_trust" (core §12, open question 3).
    "gateway": {
      "provider": "cloudflare_zero_trust",
      "token_secret_arn": "arn:aws:secretsmanager:us-east-1:123456789:secret:gt-cf-egress"
    }
  }
}
```

The same rig as strict, comment-free JSON:

```json
{
  "execution": {
    "backend": "ec2",
    "exec_mode": "container",
    "image": "123456789.dkr.ecr.us-east-1.amazonaws.com/my-dev-env:latest",
    "requires_docker": true,
    "network": { "mode": "gateway" },
    "checkpoint_interval": "5m",
    "cooldown": "10m",
    "max_runtime": "4h",
    "ec2": {
      "region": "us-east-1",
      "instance_lifecycle": "spot",
      "spot_max_price": null,
      "instance_type": "c7i.2xlarge",
      "ebs_gb": 80,
      "image_auth": { "type": "ecr" },
      "agent_auth": { "mode": "bedrock_role" },
      "gateway": {
        "provider": "cloudflare_zero_trust",
        "token_secret_arn": "arn:aws:secretsmanager:us-east-1:123456789:secret:gt-cf-egress"
      }
    }
  }
}
```

## 5. Command execution (the exec channel)

`WrapCommand` returns an `aws ssm start-session` invocation (a session document
that runs the command on the instance):

- **Container mode:** the document runs
  `docker exec -e VAR=val … <work-container> sh -c "<shell-quoted argv>"` into
  the idle work container `gt-worker-agent` prepared (core §6.1.2).
- **Native mode:** the document runs the shell-quoted argv directly, as the
  `gt-agent` user (§10), with the session env prepended per core §7.4.

The blocking-pane model is unchanged: the orchestrator's tmux pane runs the
`aws ssm start-session` process for the life of the agent.

Session-env placement follows core §7.4: non-secret vars via `docker exec -e` /
env-file; secrets never in the command (they arrive via the instance profile or
Secrets Manager, §6).

> **Exit codes don't propagate through `start-session`.** `aws ssm
> start-session` returns `0` when the session closes regardless of the remote
> process's exit status, so the launch command's exit code is **not** a reliable
> success signal. This satisfies (and motivated) the core §5 rule that success
> is never derived from the launcher's exit code: completion is `gt done`,
> crash-detection is stale-heartbeat liveness (core §8.1). If the true remote
> exit status is ever needed (e.g. diagnostics), capture it out-of-band — the
> remote wrapper writes `$?` to a file `gt-worker-agent` reads, or use `ssm
> send-command` (whose invocation status *does* carry the exit code) rather than
> `start-session`.

Command construction uses the core §6.1.2 `ShellQuote` discipline — every token
individually quoted; config-derived strings (model flags, `InitialPrompt`) are
untrusted data, never raw-interpolated into the SSM document.

## 6. Credentials & identity (EC2 mechanisms)

A single per-instance IAM-role + secret-store layer implements the core §7
invariants. No secret ever rides EC2 userdata, tags, the SSM command string, or
CloudTrail/SSM logs; Secrets Manager / SSM **ARNs** (references) are fine.

| Core concern | EC2 mechanism |
|---|---|
| Control-plane identity (proxy cert) | CSR-over-SSM (§6.1); Secrets Manager fallback (§6.1). |
| Image pull auth | ECR (same/cross-account): the **instance profile** role (`ecr:*`) + repo policy for cross-account. Other registries: `docker login` from a Secrets Manager secret the instance role can read. |
| LLM auth | Default `bedrock_role` (§6.2) — no key to inject. Alternative `secret`: Secrets Manager → container env. |
| Worker platform identity | The instance IAM role (§11), scoped per rig — also covers any AWS work the agent itself does. |

### 6.1 Proxy-cert delivery: CSR over SSM

Implements the core §7.2 protocol. **The orchestrator drives delivery over SSM
— no inbound port on the laptop.** Rather than expose a bootstrap listener on
the orchestrator (problematic behind NAT / on a dynamic laptop IP — and `:9876`
rejects certless connections via `ClientAuth: tls.RequireAndVerifyClientCert`,
while `:9877` is the admin port, `internal/proxy/server.go`), delivery is
**orchestrator-initiated via SSM**, which the daemon already has credentials
for:

1. **CSR over SSM (preferred).** The daemon issues a single SSM `SendCommand`
   (`AWS-RunShellScript`) that, on the instance, **generates the private key in
   host tmpfs and emits the CSR (CN `gt-<rig>-<name>`) on stdout**; the daemon
   **captures the CSR directly from the command output** (no poll-a-file dance),
   validates the CN against the expected identity, signs it with the CA (the
   core §7.2 CSR-signing primitive), and returns the **cert** via a second
   `SendCommand` (or a known path `gt-worker-agent` reads). Returning the cert
   this way is safe even though `send-command` output lands in CloudTrail/SSM
   logs: **a signed certificate is public material, not a secret** — only the
   private key is sensitive, and it never leaves the instance. **No bootstrap
   token is needed:** the SSM channel is IAM-authenticated and the daemon
   targets the exact instance ID it launched and tagged, so the binding a token
   would provide is inherent (core §7.2 "channel binding").
2. **Per-instance Secrets Manager secret (fallback).** The daemon writes the
   cert **+ key** to a short-TTL Secrets Manager secret scoped to the instance
   role; `gt-worker-agent` fetches it at boot. This *does* transmit the key (and
   leaves it at rest in Secrets Manager for the instance lifetime), so it is
   strictly weaker than the CSR flow — prefer (1).

> **Ongoing relay traffic** (the `:9876` mTLS control plane) likewise avoids an
> inbound laptop port via the core §7.2/§9.6 stable-hostname requirement
> (Tailscale / VPN). Note SSM Session Manager port-forwarding is
> **client→instance** (the orchestrator forwarding to a port *on* the instance),
> so it is **not** a clean reverse tunnel for the instance→host relay — it fits
> the orchestrator-initiated CSR exchange and commands, but the ongoing relay
> relies on the stable-hostname path, not SSM forwarding. When the laptop's
> address changes, the daemon can update the instance's proxy endpoint
> out-of-band (SSM Parameter Store or a `send-command`).

### 6.2 LLM auth (`agent_auth`)

Implements core §7.1. Default **`bedrock_role`**: set
`CLAUDE_CODE_USE_BEDROCK=1`, grant the instance role `bedrock:InvokeModel` —
**no key to inject**; the instance role then does triple duty (ECR pull +
Secrets Manager + Bedrock invoke). Alternative **`secret`**: source
`ANTHROPIC_API_KEY` (or the named provider var) from Secrets Manager into the
container env — worker-side, never the command line.

```jsonc
"agent_auth": { "mode": "bedrock_role" }                 // default; no secret
// or
"agent_auth": {
  "mode": "secret",
  "env_var": "ANTHROPIC_API_KEY",
  "secret_arn": "arn:aws:secretsmanager:us-east-1:123456789:secret:gt-anthropic-key"
}
```

### 6.3 Registry auth (`image_auth`)

`{ "type": "ecr" }` uses the instance profile for same- or cross-account ECR
(cross-account additionally needs a repo policy — open question 4). Other
registries: `{ "type": "secret", "secret_arn": "…" }` → `docker login`
worker-side from Secrets Manager.

## 7. Network egress posture (EC2 implementation)

Implements the core §7.3 planes and modes with VPC primitives.

**AWS platform allowlist (all modes).** The instance always needs scoped access
to the managed services this provider uses, ideally via **VPC endpoints /
PrivateLink** (NAT fallback), with the security group denying everything else
*not* otherwise permitted by the egress mode:

| Destination | Why | Path |
|---|---|---|
| ECR (api + dkr) + S3 gateway | image pull | VPC endpoints |
| SSM / SSMMessages / EC2Messages | SSM session/exec (provider channel) | VPC endpoints |
| Secrets Manager | `secret`-mode auth, registry creds | VPC endpoint |
| Bedrock runtime | `agent_auth.mode = bedrock_role` | VPC endpoint |
| S3 checkpoint-spool bucket | offline checkpoint fallback (§9.1) | S3 gateway endpoint |
| Host proxy (`:9876`) | all control-plane + git | direct (VPC / VPN / Tailscale) |

**Modes:**

1. **`sandboxed`** — security group allows only the proxy + the allowlist
   above; no general egress. Dependencies must be pre-baked into the image/AMI
   or served from an internal mirror / pull-through cache reachable via a VPC
   endpoint (e.g. CodeArtifact, an S3-backed registry proxy) — core open
   question 4.
2. **`gateway`** *(recommended default for dev work)* — full outbound internet
   mediated by a **Cloudflare Zero Trust** egress gateway: WARP / `cloudflared`
   runs as a host service on the instance, with Gateway DNS/network/HTTP
   policies enforcing destination policy, blocking known-bad endpoints, and
   logging every flow for audit/DLP. The gateway token is injected as a Secrets
   Manager reference (`ec2.gateway.token_secret_arn`), and `gt-worker-agent`
   brings the tunnel up before the work container starts. Per the core §12
   decision (open question 3), **`cloudflare_zero_trust` is the only accepted
   `gateway.provider` value in v1** — orchestrator-side preflight rejects
   anything else — and gt does not create or manage Gateway policies: the
   rig's policy (allowlists, DLP, logging rules) is administered in the
   Cloudflare dashboard/API, outside gt.
3. **`open`** — unrestricted NAT egress. Allowed but never the default.

## 8. Spot interruption (the provider interruption signal)

Implements core §9.3. Applies only to `instance_lifecycle: "spot"`; for
on-demand the poller runs but never fires.

**EC2 Spot has no reliable advance SIGTERM.** `gt-worker-agent` **polls IMDS**
`/spot/instance-action` (and optionally the rebalance recommendation, which
fires earlier). The ~2-minute warning window is best-effort. On a signal it runs
the core §9.3 shutdown sequence: stop the agent (`docker stop` / direct
signal), flush the final delta to the checkpoint ref, exit.

Note the §10 IMDS firewall must **not** blanket-disable IMDS: `gt-worker-agent`
needs the `/spot/instance-action` poller. The UID-scoped firewall preserves the
poller while denying the agent.

## 9. Teardown, zombie cap, self-termination watchdog

EC2 implementation of core §9.5:

1. **Cooldown teardown.** After `gt done` + `cooldown`, the reaper calls
   `Teardown()` → `TerminateInstances`.
2. **`max_runtime` graceful flush.** On expiry the reaper sends the flush/stop
   signal **via SSM** (the same path as a spot interrupt) so `gt-worker-agent`
   stops the agent, pushes a final checkpoint, and surfaces tail logs; after a
   60–120s grace window it calls `TerminateInstances` regardless.
3. **In-instance self-termination watchdog (cost backstop).** Core §9.5's
   worker-side watchdog, in EC2 form: `gt-worker-agent` self-terminates (after a
   final checkpoint) at `max_runtime` or after losing orchestrator contact for a
   dead-man's-switch interval (a few × `checkpoint_interval`). A missed teardown
   here means an instance billing indefinitely, so the belt-and-suspenders form
   is a `shutdown -h` self-stop plus `InstanceInitiatedShutdownBehavior:
   terminate`, guaranteeing the instance dies even if the laptop never comes
   back.
4. **Orphan sweep.** The reaper lists instances tagged `gt:*` with no live agent
   bead and terminates them (§2).

### 9.1 Offline checkpoint spool: S3

EC2 form of core §9.2's spool: when the proxy is unreachable at flush time (spot
interrupt while the laptop sleeps), `gt-worker-agent` uploads a **git bundle of
the checkpoint ref to S3** (the instance role has scoped access to a dedicated
spool bucket/prefix). On resume, the new instance pulls the S3 bundle when the
host `.repo.git` is behind it, **and immediately re-pushes that state to
`.repo.git` via the proxy** (or the daemon ingests the bundle directly) so the
host reconverges and S3 is not left as a second source-of-truth. Optional; only
engages on a proxy outage.

## 10. Docker support & IMDS hardening

EC2 **supports** the core §10 Docker capability: the AMI runs `dockerd`; in
`container` mode the work container gets `/var/run/docker.sock` bind-mounted
(Docker-outside-of-Docker, sibling containers); in `native` mode the agent uses
the host daemon directly.

**The EC2-specific credential endpoint is IMDS (`169.254.169.254`)** — the
instance profile's temporary credentials (ECR, Secrets Manager, Bedrock, S3) are
readable there, so a container escape via the socket reaches host root **and**
the role credentials. Single-tenant ephemerality limits *blast radius* but does
not stop credential theft within the rig's own run. Required mitigations (the
EC2 instantiation of core §10's list):

1. **Block IMDS from the container network** — host `iptables` dropping
   `169.254.169.254` from the docker bridge, and IMDSv2 with a hop limit of 1 so
   a container cannot reach it even via the gateway. **This requires bridge
   networking** (core §6.1.1 option 2): under `--network host` there is no
   bridge and no extra hop, so both controls are bypassed — which is exactly why
   untrusted rigs MUST use bridge mode. For `native` mode (and host-net) the
   primary control is a **host firewall scoped by UID** — but this only works if
   the two sides have *different* UIDs. So the agent must **not** run as root in
   these modes: `gt-worker-agent` stays root (UID 0, for dockerd/worktree), and
   it drops privileges to launch the agent as the **dedicated non-root
   `gt-agent` UID** baked into the AMI (§3). The firewall then allows only UID 0
   (and `gt-worker-agent`) to reach `169.254.169.254` and denies `gt-agent`. If
   the agent also ran as root the filter could not tell them apart — hence the
   dedicated UID is mandatory in native/host-net mode (it dovetails with the
   core §6.1 shared-`gt`-group worktree so checkpointing still works across the
   UID boundary). **Do not simply disable IMDS post-boot:** `gt-worker-agent`
   needs it for the `/spot/instance-action` poller (§8), so a blanket disable
   would break spot interruption handling. The UID-scoped firewall preserves the
   poller while denying the agent.
   **Caveat — the Docker socket bypasses the UID firewall.** If the `gt-agent`
   UID can write `/var/run/docker.sock`, it can `docker run` a container
   (host-net, or mounting host `/`) whose traffic originates as
   **root/dockerd**, not `gt-agent` — so it reaches IMDS regardless of the UID
   filter. The UID firewall (and the bridge `iptables` control) therefore only
   contain the *direct* escape paths; **they do not contain a
   Docker-socket-holding agent.** For an untrusted rig that also needs Docker,
   the socket *is* the hole, and the only real defense is mitigation (2):
   rootless dockerd / nested userns, where the nested daemon runs unprivileged
   and its containers cannot reach the host's IMDS credentials. Untrusted + raw
   host socket is not a safe combination at any UID.
2. **Mandatory hardening for untrusted code** — core §10(2): `sandboxed`/
   untrusted rigs MUST use rootless dockerd / nested userns or skip the socket.
3. **Per-rig least-privilege IAM** — scope each rig's instance role to exactly
   the ECR repos / secrets / Bedrock models / S3 prefixes it needs, not one
   broad shared role, so a stolen credential is narrowly bounded (§11).

Depth-of-hardening beyond (1)–(3) is core open question 5.

## 11. IAM requirements (instance profile)

Per-rig instance role, least-privilege (§10.3). Required actions by feature:

| Feature | Actions (scoped) |
|---|---|
| SSM channel (exec, cert flow, binary injection) | `ssm:UpdateInstanceInformation`, `ssmmessages:*`, `ec2messages:*` (the standard `AmazonSSMManagedInstanceCore` set) |
| Image pull | `ecr:GetAuthorizationToken`, `ecr:BatchGetImage`, `ecr:GetDownloadUrlForLayer` on the rig's repos |
| `agent_auth: secret` / registry secrets / gateway token | `secretsmanager:GetSecretValue` on the specific ARNs |
| `agent_auth: bedrock_role` | `bedrock:InvokeModel` (+ streaming variant) on the rig's model IDs |
| S3 checkpoint spool (§9.1) | `s3:PutObject`, `s3:GetObject`, `s3:ListBucket` on the spool bucket/prefix for this rig |
| Self-termination watchdog (§9) | none — `shutdown -h` + `InstanceInitiatedShutdownBehavior: terminate` needs no API call (deliberately: the instance role gets **no** `ec2:TerminateInstances`) |

The **daemon-side** (orchestrator) credentials additionally need
`ec2:RunInstances`, `ec2:TerminateInstances`, `ec2:DescribeInstances`,
`ec2:CreateTags`, `ssm:SendCommand`, `ssm:StartSession`, and `iam:PassRole` for
the instance profile.

## 12. Implementation phases (EC2)

Assumes core Tiers 1–2 (config, CA primitive, interface, provider-neutral
`gt-worker-agent`) are in place.

1. Packer AMI as the stable base (§3): dockerd + amazon-ssm-agent + git +
   `gt-agent` user + bootstrapper; boot-time SSM injection of gt/bd/
   proxy-client/`gt-worker-agent`.
2. `EC2Backend.Provision/Teardown` honoring `instance_lifecycle` (spot **and**
   on-demand), the provision hook, instance-profile credential wiring (ECR /
   `agent_auth`), tag-based discovery.
3. CSR-over-SSM cert acquisition (§6.1); container launch with bind-mounts
   (`/opt/gt`, worktree, docker.sock), `exec_mode` container/native.
4. SSM-based `WrapCommand` (blocking-pane wrapper) + remote session-env
   injection.
5. IMDS spot-interrupt poller + resume logic (§8, core §9.4); cooldown teardown
   + watchdog (§9); S3 spool (§9.1).
6. Egress postures (§7): `sandboxed` (SG-only) → `gateway` (Zero Trust tunnel as
   a host service) → `open`.

## 13. Open questions (EC2)

1. **Bedrock model parity.** Confirm target models (e.g. Opus 4.8) are available
   on Bedrock in the chosen region before defaulting a rig to `bedrock_role`;
   otherwise use `secret` + direct Anthropic API.
2. **AMI scope / base distro.** Amazon Linux 2023 vs Ubuntu LTS; which
   toolchains (if any) belong in the base AMI vs the default work image (core
   open question 2)?
3. **Instance sizing UX.** Prefer explicit `instance_type`, or `cpu`/`memory` →
   cheapest-matching-type selection (and across which families)? How to express
   GPU / arch (arm64 vs x86) needs?
4. **Cross-account ECR.** Standardize on instance-role + repo policy, or an
   assumed pull-role ARN in `image_auth`?
5. **On-demand fallback.** Should `instance_lifecycle: "spot"` optionally fall
   back to on-demand on `InsufficientInstanceCapacity`, or fail and retry spot?

---

## Appendix A — Fargate backend (later, secondary)

> Deferred — for lightweight rigs that need **neither Docker (core §10) nor a
> custom host** (e.g. pure code-edit/review polecats). Ships after EC2 (core
> Tier 4). Captured here only so the interface stays provider-agnostic; skip on
> a first read. Supports `FARGATE_SPOT` and `FARGATE` (on-demand) capacity
> providers; `WrapCommand` maps to `aws ecs execute-command --command "<argv>"`
> and `Discover` to `ECS ListTasks` on the identity tags.

Fargate gives no host and no shared host filesystem, so the host-bind-mount
model does not apply. Injection instead uses a **version-pinned `gt-sidecar`
container + a shared task volume**: the sidecar copies `gt`/`bd` + the idle
entrypoint (+ static `busybox sh` for distroless) onto the volume, obtains a
signed cert via the same key-local/CSR flow (§6.1, over ECS Exec instead of
SSM), and runs the relay + checkpoint/interrupt logic — the in-task analogue of
`gt-worker-agent`. The sidecar bundles `git` for host-side checkpointing (same
reason the AMI carries it, §3). The work container `dependsOn` the sidecar's
**container health check** (binaries copied + relay listening; ECS only honors
`HEALTHY` when a health check is defined), shares the task network namespace (so
the relay is reachable without the core §6.1.1 host-gateway dance), and is
driven by `aws ecs execute-command`.

**Interruption** is more involved than EC2: ECS SIGTERMs every container at
once, so the work container needs `initProcessEnabled: true` (`tini` as PID 1
forwarding SIGTERM) and its idle entrypoint must trap/ignore SIGTERM. Note
**`pidMode: task` is *not* supported on Fargate** (EC2 launch type only), so the
sidecar **cannot** signal the agent across the PID namespace there — Fargate
must rely entirely on **shared-volume STOP-marker** coordination (the work
container's idle entrypoint watches for `/opt/gt/STOP` and stops its own agent
child).

**Docker is NOT supported.** Fargate exposes no Docker daemon, forbids
`privileged`, and cannot run Docker-in-Docker — `docker build` / `docker
compose` / testcontainers do not work. The only partial path is to model
required services (postgres, redis) as **additional containers in the same
task** (shared localhost) — static, declared at provision time, no image builds,
cannot run the repo's own compose file dynamically. `requires_docker: true`
rigs are rejected for Fargate at orchestrator-side preflight (core §6.3).

The extra moving parts — and the lack of Docker — are exactly why Fargate is
secondary.
