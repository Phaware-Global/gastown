package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
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

	// ProxyClient is the host path to the CONTAINER-platform gt-proxy-client to
	// inject as the agent's gt/bd. Empty skips injection (the image is then
	// expected to carry its own, which nothing currently does).
	ProxyClient string

	// AgentBinary, when set, is preflighted worker-side after the idle
	// container starts (§6.3): the resolved agent runtime must be on PATH in
	// the image, and /bin/sh must exist. Failing preflight stops the
	// container and errors before any agent launch.
	AgentBinary string

	// Docker overrides the docker binary (tests). Default "docker".
	Docker string
}

// proxyClientName is the injected binary's name inside /opt/gt.
const proxyClientName = "gt-proxy-client"

// Where the injected CLI lives inside the container, and the PATH prefix that
// makes it win.
//
// Prefixing PATH is what makes the resolution deterministic: linking into
// /usr/local/bin is a convenience, but an image whose PATH puts another
// directory first would still shadow it. The mount is read-only, so nothing in
// the container can replace what it resolves to.
const (
	mountedGtPath = "/opt/gt/gt"
	mountedBdPath = "/opt/gt/bd"

	// AgentPathPrefix is prepended to a container command line so the injected
	// CLI resolves first. It expands the image's own PATH, so the agent runtime
	// stays findable (§6.2's contract).
	//
	// It EXPORTS rather than using an assignment prefix. `PATH=x cmd1 && cmd2`
	// applies the assignment to cmd1 ONLY — a POSIX rule that made an earlier
	// version resolve `gt` against the mount and `bd` against the image, so
	// every injected container session was refused. Verified in dash, bash and
	// zsh; TestAgentPathPrefix_AppliesToEveryCommand runs the real shell.
	AgentPathPrefix = "export PATH=/opt/gt:$PATH; "
)

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

	// The agent's `gt` and `bd` inside the container: gt-proxy-client under
	// those names, from the CONTAINER's platform (a macOS worker's own binary
	// would not execute here). The links are made HOST-side because /opt/gt is
	// mounted read-only.
	if w.cfg.ProxyClient != "" {
		if err := copyExecutable(w.cfg.ProxyClient, filepath.Join(w.cfg.GTDir, proxyClientName)); err != nil {
			return fmt.Errorf("inject %s: %w", proxyClientName, err)
		}
		for _, name := range []string{"gt", "bd"} {
			if err := atomicSymlink(proxyClientName, filepath.Join(w.cfg.GTDir, name)); err != nil {
				return fmt.Errorf("linking injected %s: %w", name, err)
			}
		}
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
		// The relay a container reaches is the bridge gateway, not loopback, so
		// gt-proxy-client needs this to use relay mode (it refuses plaintext to
		// a non-loopback host otherwise, which is the guard working as intended).
		"-e", "GT_PROXY_RELAY=1",
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
	// Verification runs whether or not we injected: the question is "can this
	// agent reach the control plane", and an image that ships its own gt/bd
	// answers it just as well. Injection only changes what we EXPECT to resolve.
	{
		// /opt/gt is read-only, so the CLI is linked into a directory that is
		// on every reasonable PATH. Done as root because the image's user may
		// not own /usr/local/bin.
		//
		// This is a hard failure rather than a warning: an agent that cannot
		// run `gt` cannot call `gt done`, take mail, or update a bead — the
		// session would look alive and accomplish nothing.
		var linkErr error
		if w.cfg.ProxyClient != "" {
			// mkdir -p first: a minimal image (busybox is the canonical one) has
			// no /usr/local/bin at all, and `ln -sf` into a missing directory
			// fails. Before injection existed, such an image came up degraded;
			// it must not now fail to come up for want of one mkdir.
			link := "mkdir -p /usr/local/bin && ln -sf /opt/gt/gt /usr/local/bin/gt && ln -sf /opt/gt/bd /usr/local/bin/bd"
			_, linkErr = w.docker(ctx, "exec", "-u", "0", w.name, "/bin/sh", "-c", link)
		}

		// Probe with the same PATH the agent will run with: AgentPathPrefix puts
		// the read-only mount first, and container execs deliberately do NOT
		// carry the worker host's PATH (see workerclient.agentEnv), so both this
		// probe and the agent expand the IMAGE's PATH.
		//
		// BOTH names are resolved and BOTH are checked: `bd` is the same injected
		// binary under another name — the beads CLI — and checking only `gt` left
		// an image that ships its own `bd` completely unexamined.
		resolved, err := w.docker(ctx, "exec", w.name, "/bin/sh", "-c",
			AgentPathPrefix+"command -v gt && command -v bd")
		if err != nil {
			if linkErr != nil {
				return fmt.Errorf("image preflight: could not put gt/bd on PATH in image %s (linking failed: %v; the agent could not reach the control plane): %w", w.cfg.Image, linkErr, err)
			}
			return fmt.Errorf("image preflight: gt/bd not on PATH in image %s and none were injected — the agent could not reach the control plane (if this worker should have received a client, check that the orchestrator has artifacts for this container's platform): %w", w.cfg.Image, err)
		}
		lines := strings.Split(strings.TrimSpace(resolved), "\n")
		gtPath, bdPath := "", ""
		if len(lines) > 0 {
			gtPath = strings.TrimSpace(lines[0])
		}
		if len(lines) > 1 {
			bdPath = strings.TrimSpace(lines[1])
		}

		if w.cfg.ProxyClient != "" {
			// We injected a client, so the mount comes first on PATH and both
			// names MUST resolve inside it. Anything else means the image
			// shadowed us — usually accidentally (an image that bakes in an old
			// gt), which is precisely the confusing failure to catch early.
			//
			// This is not a defense against a HOSTILE image: the agent runs
			// inside that image, so an image that wants to interpose can. It
			// stops an accident from silently swapping the control-plane CLI.
			for _, got := range []struct{ name, path, want string }{
				{"gt", gtPath, mountedGtPath},
				{"bd", bdPath, mountedBdPath},
			} {
				if got.path != got.want {
					return fmt.Errorf("image preflight: %s resolves %s to %q, not the injected client (%s) — refusing to run the agent against an image-supplied control-plane CLI",
						w.cfg.Image, got.name, got.path, got.want)
				}
			}
		}
		if linkErr != nil {
			// Survivable: the mount is what the agent uses. The link is a
			// convenience for anything that resets PATH.
			slog.Default().Warn("gt/bd link into /usr/local/bin failed; the agent uses the mounted client",
				"image", w.cfg.Image, "container", w.name, "resolved_gt", gtPath, "err", linkErr)
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

// atomicSymlink replaces a symlink in one step: create it under a unique name,
// then rename over the target.
//
// GTDir is shared by every session on the worker and mounted live into running
// containers, so a remove-then-symlink is wrong twice over: two concurrent
// Prepares interleave into an EEXIST that fails a session, and the gap between
// the two calls is a window where an already-running agent's `gt` is simply
// missing. Rename over an existing symlink does neither.
func atomicSymlink(target, link string) error {
	tmp, err := os.MkdirTemp(filepath.Dir(link), ".link-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	staged := filepath.Join(tmp, filepath.Base(link))
	if err := os.Symlink(target, staged); err != nil {
		return err
	}
	return os.Rename(staged, link)
}

// copyExecutable copies through a temp file + rename, so a container never
// mounts a half-written binary.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
