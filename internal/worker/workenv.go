package worker

import (
	"bytes"
	"context"
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
	if cfg.RelayPort <= 0 {
		return nil, fmt.Errorf("workenv: relay port is required")
	}
	if cfg.Docker == "" {
		cfg.Docker = "docker"
	}
	return &WorkEnv{
		cfg:  cfg,
		name: "gt-work-" + cfg.Rig + "-" + cfg.Polecat,
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
func (w *WorkEnv) docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, w.cfg.Docker, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
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
	idle := filepath.Join(w.cfg.GTDir, "gt-idle.sh")
	script := "#!/bin/sh\n# gastown injected idle entrypoint: hold the container until docker exec.\ntrap 'exit 0' TERM INT\nwhile :; do sleep 60 & wait $!; done\n"
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
	args = append(args, w.cfg.Image, idleEntrypoint)

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

// shellQuoteToken single-quotes a token for the sh -c preflight probe.
func shellQuoteToken(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
