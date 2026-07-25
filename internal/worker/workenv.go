package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// WorkEnv prepares and supervises the polecat's CONTAINER-mode execution
// environment on the worker (docs/design/remote-polecat-execution.md §6.1,
// §6.1.2): `docker run` the IDLE work container — injected idle entrypoint,
// no agent yet — so the orchestrator can later `docker exec` the agent into
// it via the provider exec channel. gt-worker-agent never starts the agent
// itself.
//
// It also supplies the §9.3 StopWork step (docker stop of the work
// container) and the container half of teardown. Native mode has no
// container and therefore no WorkEnv; stopping a native agent is the
// provider channel's concern.
type WorkEnv struct {
	cfg  WorkEnvConfig
	name string
}

// WorkEnvConfig configures the container work environment.
type WorkEnvConfig struct {
	Rig     string // polecat identity (also recorded as container labels)
	Polecat string
	Session string

	// Image is the rig's work image (§6.2 contract: toolchain + agent
	// runtime + /bin/sh; gastown injects the rest). Required.
	Image string
	// Worktree is the host-side worktree path, bind-mounted at /work.
	Worktree string
	// GTDir is the host dir holding injected gastown bits (gt/bd/idle
	// entrypoint), bind-mounted read-only at /opt/gt.
	GTDir string

	// ContainerNetwork selects the §6.1.1 wiring: "bridge" (default; the
	// relay is reached via host.docker.internal:host-gateway, and
	// network-level hardening works) or "host" (container shares the host
	// netns; trusted rigs only).
	ContainerNetwork string
	// Sandboxed marks the rig's egress posture as sandboxed. Host networking
	// defeats bridge-level hardening, so sandboxed+host is refused (§6.1.1:
	// preflight enforces the pairing).
	Sandboxed bool
	// MountDockerSocket bind-mounts /var/run/docker.sock for rigs whose
	// workflows need a real Docker daemon (§10; trusted rigs only —
	// enforcement of the trust pairing happens at orchestrator preflight).
	MountDockerSocket bool

	// RelayPort is the local relay's port, used to point GT_PROXY_URL at the
	// §6.1.1 address for the chosen networking mode.
	RelayPort int

	// AgentBinary, when set, is preflighted worker-side after the idle
	// container starts (§6.3): the resolved agent runtime must be on PATH in
	// the image, and /bin/sh must exist. Failing preflight stops the
	// container and errors before any agent launch.
	AgentBinary string

	// Docker overrides the docker binary (tests). Default "docker".
	Docker string
}

const (
	containerNetworkHost   = "host"
	containerNetworkBridge = "bridge"

	// idleEntrypoint is the injected idle entrypoint's path inside the
	// container (§6.1.2: the image is never expected to carry one).
	idleEntrypoint = "/opt/gt/gt-idle.sh"
)

// NewWorkEnv validates cfg and builds a WorkEnv.
func NewWorkEnv(cfg WorkEnvConfig) (*WorkEnv, error) {
	if cfg.Rig == "" || cfg.Polecat == "" {
		return nil, fmt.Errorf("workenv: rig and polecat are required")
	}
	if cfg.Image == "" {
		return nil, fmt.Errorf("workenv: image is required in container mode (§6.2)")
	}
	if cfg.Worktree == "" || cfg.GTDir == "" {
		return nil, fmt.Errorf("workenv: worktree and gt-dir are required")
	}
	switch cfg.ContainerNetwork {
	case "":
		cfg.ContainerNetwork = containerNetworkBridge
	case containerNetworkHost, containerNetworkBridge:
	default:
		return nil, fmt.Errorf("workenv: container network %q, want %q or %q",
			cfg.ContainerNetwork, containerNetworkHost, containerNetworkBridge)
	}
	// §6.1.1: bridge is REQUIRED for sandboxed rigs — host networking makes
	// the container's traffic indistinguishable from the host's, so no
	// bridge-level control (including credential-endpoint blocking) can
	// apply. Refuse rather than silently weaken the posture.
	if cfg.Sandboxed && cfg.ContainerNetwork == containerNetworkHost {
		return nil, fmt.Errorf("workenv: sandboxed egress posture requires bridge networking (§6.1.1); host networking defeats network-level isolation")
	}
	// The docker socket is a strictly LARGER isolation breach than host
	// networking (host docker daemon ≈ host root, §10) — refuse it for a
	// sandboxed rig locally, symmetric with the networking guard above,
	// rather than trusting orchestrator preflight alone.
	if cfg.Sandboxed && cfg.MountDockerSocket {
		return nil, fmt.Errorf("workenv: sandboxed egress posture cannot mount the docker socket (§10: host daemon access defeats sandbox containment; requires rootless dockerd, not shipped)")
	}
	if cfg.RelayPort <= 0 {
		return nil, fmt.Errorf("workenv: relay port is required")
	}
	if cfg.Docker == "" {
		cfg.Docker = "docker"
	}
	return &WorkEnv{
		cfg:  cfg,
		name: WorkContainerName(cfg.Rig, cfg.Polecat),
	}, nil
}

// ContainerName returns the work container's name — the handle the provider
// backend's WrapCommand needs to `docker exec` the agent into it.
func (w *WorkEnv) ContainerName() string { return w.name }

// ProxyURL returns the relay address the agent inside the container must use
// (§6.1.1): the container's loopback IS the host's under host networking;
// under bridge it reaches the host via the host-gateway alias.
func (w *WorkEnv) ProxyURL() string {
	if w.cfg.ContainerNetwork == containerNetworkHost {
		return fmt.Sprintf("http://127.0.0.1:%d", w.cfg.RelayPort)
	}
	return fmt.Sprintf("http://host.docker.internal:%d", w.cfg.RelayPort)
}

// docker runs the docker CLI, returning trimmed stdout.
//
// Cancellation attribution: exec's kill produces "signal: killed", which does
// NOT wrap context.Canceled — so when the context is canceled AND the process
// died by signal (never exited on its own), the returned error wraps the
// context cause instead, letting callers errors.Is-classify an interrupt. A
// genuine docker failure (nonzero exit) keeps its own error even if the
// context is canceled concurrently.
//
//nolint:unparam // deliberate: this wraps the docker CLI and returns its stdout;
// the call sites here happen to need only the error.
func (w *WorkEnv) docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, w.cfg.Docker, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) || !ee.Exited() {
				return "", fmt.Errorf("%s %s: %w", w.cfg.Docker, strings.Join(args, " "), context.Cause(ctx))
			}
		}
		return "", fmt.Errorf("%s %s: %w: %s", w.cfg.Docker, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Prepare brings up the idle work container (§6.1.2 step 1, container half):
// inject the idle entrypoint into GTDir, replace any stale container of the
// same name, start the image idle, and run worker-side preflight (§6.3).
// The agent is NOT started — that is the orchestrator's move.
func (w *WorkEnv) Prepare(ctx context.Context) error {
	// Injected idle entrypoint (§6.2: the image is never expected to carry
	// an entrypoint). /bin/sh is the only image dependency (v1 contract).
	//
	// PID-1 duty: the docker-exec'd agent is NOT our child, and if PID 1
	// exits the instant TERM arrives, the namespace teardown SIGKILLs the
	// agent mid-write with zero grace. So on TERM the entrypoint forwards
	// TERM to every process in the PID namespace (kill -1) and waits for
	// them to drain — up to 25s, inside docker stop's 30s window — before
	// exiting, so `docker stop -t 30` is genuinely graceful for the agent.
	idle := filepath.Join(w.cfg.GTDir, "gt-idle.sh")
	script := `#!/bin/sh
# gastown injected idle entrypoint: hold the container until docker exec,
# and forward TERM to the exec'd processes with a drain window on stop.
term() {
	kill -TERM -1 2>/dev/null
	# Reap our own children (the backgrounded sleep) immediately so the
	# only-PID-1-left check below doesn't wait on zombies; orphaned agent
	# descendants reparent to us and are reaped by the shell as they exit.
	wait 2>/dev/null
	# Drain window: ~25s inside docker stop's 30s grace, then exit anyway.
	i=0
	while [ "$i" -lt 25 ]; do
		set -- /proc/[0-9]*
		[ "$#" -le 1 ] && break
		i=$((i+1))
		sleep 1
	done
	exit 0
}
trap term TERM INT
while :; do sleep 60 & wait $!; done
`
	if err := writeFileAtomic(idle, []byte(script), 0755); err != nil {
		return fmt.Errorf("write idle entrypoint: %w", err)
	}

	// Replace, never reuse: a fresh Prepare means a fresh environment; a
	// same-name leftover (crashed worker-agent) is stale by definition.
	_, _ = w.docker(ctx, "rm", "-f", w.name)

	args := []string{
		"run", "-d", "--name", w.name,
		"--label", "gt.rig=" + w.cfg.Rig,
		"--label", "gt.polecat=" + w.cfg.Polecat,
		"--label", "gt.session=" + w.cfg.Session,
		"-v", w.cfg.GTDir + ":/opt/gt:ro",
		"-v", w.cfg.Worktree + ":/work",
		"-w", "/work",
		"-e", "GT_PROXY_URL=" + w.ProxyURL(),
	}
	if w.cfg.ContainerNetwork == containerNetworkHost {
		args = append(args, "--network", "host")
	} else {
		args = append(args, "--add-host", "host.docker.internal:host-gateway")
	}
	if w.cfg.MountDockerSocket {
		args = append(args, "-v", "/var/run/docker.sock:/var/run/docker.sock")
	}
	// End-of-options: a flag-shaped Image value must be a bad image name,
	// never a docker run flag.
	args = append(args, "--", w.cfg.Image, idleEntrypoint)

	if _, err := w.docker(ctx, args...); err != nil {
		return fmt.Errorf("start idle work container: %w", err)
	}

	if err := w.preflight(ctx); err != nil {
		// Fail fast BEFORE any agent launch (§6.3), and don't leave the
		// container running behind the error.
		_ = w.Teardown(ctx)
		return err
	}
	return nil
}

// preflight runs the §6.3 worker-side image-content checks inside the
// now-running idle container: /bin/sh must exist (v1 contract, decision 6)
// and the rig's resolved agent runtime must be on PATH.
func (w *WorkEnv) preflight(ctx context.Context) error {
	if _, err := w.docker(ctx, "exec", w.name, "/bin/sh", "-c", "true"); err != nil {
		return fmt.Errorf("image preflight: /bin/sh not usable in image %s (v1 requires a POSIX shell, §6.2): %w", w.cfg.Image, err)
	}
	if w.cfg.AgentBinary != "" {
		if _, err := w.docker(ctx, "exec", w.name, "/bin/sh", "-c", "command -v "+shellQuoteToken(w.cfg.AgentBinary)); err != nil {
			return fmt.Errorf("image preflight: agent runtime %q not on PATH in image %s (§6.2): %w", w.cfg.AgentBinary, w.cfg.Image, err)
		}
	}
	return nil
}

// StopWork gracefully stops the work container — the §9.3 shutdown step 1
// the Supervisor calls before its final checkpoint flush. docker stop sends
// TERM and escalates to KILL after the timeout.
func (w *WorkEnv) StopWork(ctx context.Context) error {
	if _, err := w.docker(ctx, "stop", "-t", "30", w.name); err != nil {
		return fmt.Errorf("stop work container: %w", err)
	}
	return nil
}

// Teardown removes the work container entirely (the container half of the
// §9.5 release; the worker machine's fate is the provider's concern).
func (w *WorkEnv) Teardown(ctx context.Context) error {
	if _, err := w.docker(ctx, "rm", "-f", w.name); err != nil {
		return fmt.Errorf("remove work container: %w", err)
	}
	return nil
}

// WorkContainerName is the deterministic work-container name for a polecat,
// so an orphaned container can be removed without a live WorkEnv handle.
func WorkContainerName(rig, polecat string) string {
	return "gt-work-" + rig + "-" + polecat
}

// RemoveWorkContainer force-removes a polecat's work container by name.
// Idempotent by intent: a missing container just yields a docker error the
// caller can ignore. dockerBin defaults to "docker".
func RemoveWorkContainer(ctx context.Context, dockerBin, rig, polecat string) error {
	if dockerBin == "" {
		dockerBin = "docker"
	}
	cmd := exec.CommandContext(ctx, dockerBin, "rm", "-f", WorkContainerName(rig, polecat))
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("remove work container %s: %w: %s", WorkContainerName(rig, polecat), err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// shellQuoteToken single-quotes a token for the sh -c preflight probe.
func shellQuoteToken(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
