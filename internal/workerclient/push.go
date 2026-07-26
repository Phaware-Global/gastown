package workerclient

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/sockproto"
)

// Binary freshness (§4.1 push_binaries, §11 phase 4).
//
// The worker runs two gastown binaries that are version-coupled to the
// orchestrator's control plane: gt-proxy-client — which IS `gt` and `bd` here,
// so a stale one presents as an agent that mysteriously cannot call `gt done` —
// and gt-worker-client itself. Keeping them in step is the orchestrator's job,
// not an operator's, because nothing else notices the drift.
const (
	// BinProxyClient is installed live: the gt/bd symlinks point at it, so an
	// atomic rename updates both with nothing else to do.
	BinProxyClient = "gt-proxy-client"
	// BinWorkerClient is THIS service. It cannot be swapped under a running
	// session — a restart abandons the agent — so it is staged and applied when
	// the worker is idle.
	BinWorkerClient = "gt-worker-client"
)

// pushableBinaries is an allowlist, not a sanitizer: `name` arrives from the
// wire, and joining it to a path is precisely how a traversal gets in.
var pushableBinaries = map[string]bool{
	BinProxyClient:  true,
	BinWorkerClient: true,
}

// maxPushBytes bounds a single pushed binary. gt is ~130MB; this leaves room
// while refusing a stream that would fill the worker's disk.
const maxPushBytes = 512 << 20

// binDir is the worker-owned directory holding the binaries it runs: the
// gt/bd shims point into it, and pushes land here. Worker-owned matters — the
// orchestrator must never need write access to a system path.
func (s *Service) binDir() string { return filepath.Join(s.cfg.StateDir, "bin") }

// containerBinDir holds binaries for a DIFFERENT platform than the worker's
// own: the Linux ones injected into a work container. They are never installed
// as the worker's binaries — a macOS worker cannot run them.
func (s *Service) containerBinDir(platform string) string {
	return filepath.Join(s.cfg.StateDir, "container-bin", platform)
}

// The platform tag's shape check is sockproto.ValidPlatformTag — shared with
// the orchestrator, which validates the values it receives the same way.

// stagingDir holds a partially-received or deferred binary.
func (s *Service) stagingDir() string { return filepath.Join(s.cfg.StateDir, "staging") }

// pushState accumulates one in-flight transfer on a connection. A push is
// per-connection so two orchestrators (or a retry after a drop) cannot
// interleave chunks into one file.
type pushState struct {
	name     string
	platform string // "" = the worker's own
	f        *os.File
	sum      []byte // running sha256 over what was written
	n        int64
}

func (p *pushState) abort() {
	if p.f != nil {
		name := p.f.Name()
		_ = p.f.Close()
		_ = os.Remove(name)
	}
}

// handlePushBinary consumes one chunk of a push_binaries stream. The final
// chunk (EOF) verifies the whole-file digest and installs.
func (s *Service) handlePushBinary(c *connState, m *sockproto.Message) {
	fail := func(code, format string, args ...any) {
		if c.push != nil {
			c.push.abort()
			c.push = nil
		}
		msg := fmt.Sprintf(format, args...)
		s.log.Warn("push_binaries refused", "name", m.Name, "err", msg)
		_ = c.send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Code: code, Msg: msg})
	}

	if m.Platform != "" && !sockproto.ValidPlatformTag(m.Platform) {
		fail("bad_request", "platform %q is not a <goos>-<goarch> tag", m.Platform)
		return
	}
	if !pushableBinaries[m.Name] {
		fail("bad_request", "%q is not a pushable binary (allowed: %s)", m.Name, strings.Join([]string{BinProxyClient, BinWorkerClient}, ", "))
		return
	}

	// Starting a different file mid-stream is a protocol error, not a reset:
	// silently discarding a half-written binary would hide a bug that could
	// otherwise install a spliced file.
	if c.push != nil && (c.push.name != m.Name || c.push.platform != m.Platform) {
		fail("proto", "push for %q (%s) interrupted by %q (%s) on the same connection",
			c.push.name, c.push.platform, m.Name, m.Platform)
		return
	}

	if c.push == nil {
		if err := os.MkdirAll(s.stagingDir(), 0700); err != nil {
			fail("io", "creating staging dir: %v", err)
			return
		}
		f, err := os.CreateTemp(s.stagingDir(), m.Name+".part-*")
		if err != nil {
			fail("io", "creating staging file: %v", err)
			return
		}
		c.push = &pushState{name: m.Name, platform: m.Platform, f: f, sum: nil}
		c.pushHash = sha256.New()
	}

	if m.Data != "" {
		chunk, err := base64.StdEncoding.DecodeString(m.Data)
		if err != nil {
			fail("bad_request", "chunk is not valid base64: %v", err)
			return
		}
		if c.push.n+int64(len(chunk)) > maxPushBytes {
			fail("too_large", "binary exceeds the %d-byte limit", maxPushBytes)
			return
		}
		if _, err := c.push.f.Write(chunk); err != nil {
			fail("io", "writing staging file: %v", err)
			return
		}
		_, _ = c.pushHash.Write(chunk)
		c.push.n += int64(len(chunk))
	}

	if !m.EOF {
		return // mid-stream: no ack per chunk, the sender streams
	}

	// Verify BEFORE anything is installed: this is the one integrity gate
	// (§12 decision 2 — mTLS + enrollment + this digest; release-key signatures
	// are post-v1).
	got := hex.EncodeToString(c.pushHash.Sum(nil))
	want := strings.ToLower(strings.TrimSpace(m.SHA256))
	if want == "" {
		fail("bad_request", "push for %q carried no sha256", m.Name)
		return
	}
	if got != want {
		fail("integrity", "sha256 mismatch for %q: got %s, want %s", m.Name, got, want)
		return
	}
	if err := c.push.f.Sync(); err != nil {
		fail("io", "syncing staging file: %v", err)
		return
	}
	staged := c.push.f.Name()
	if err := c.push.f.Close(); err != nil {
		fail("io", "closing staging file: %v", err)
		return
	}
	if err := os.Chmod(staged, 0755); err != nil {
		fail("io", "chmod staged binary: %v", err)
		return
	}
	name, platform := c.push.name, c.push.platform
	c.push = nil
	c.pushHash = nil

	applied, err := s.installPushed(name, platform, staged)
	if err != nil {
		_ = os.Remove(staged)
		s.log.Warn("installing pushed binary", "name", name, "err", err)
		_ = c.send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Code: "io", Msg: err.Error()})
		return
	}
	s.log.Info("pushed binary accepted", "name", name, "bytes", "-", "applied", applied)
	_ = c.send(&sockproto.Message{Type: sockproto.TypePushBinaryAck, ID: m.ID,
		Name: name, Platform: platform, Applied: applied})
}

// installPushed moves a verified binary into place, or stages it when applying
// now would kill work. Returns "installed" or "staged".
func (s *Service) installPushed(name, platform, staged string) (string, error) {
	// A tagged platform is for the work container, not for us: install it into
	// the injection tree and never near the binaries this worker executes.
	if platform != "" {
		dir := s.containerBinDir(platform)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("creating container bin dir: %w", err)
		}
		if err := os.Rename(staged, filepath.Join(dir, name)); err != nil {
			return "", fmt.Errorf("installing %s for %s: %w", name, platform, err)
		}
		return "installed", nil
	}

	if err := os.MkdirAll(s.binDir(), 0755); err != nil {
		return "", fmt.Errorf("creating bin dir: %w", err)
	}

	if name == BinWorkerClient {
		// This is the running service, and applying it means exiting for the
		// supervisor to restart us. That must NEVER happen on the request path:
		// the orchestrator is still holding this connection — Provision reuses
		// it for session_open immediately after the push — so exiting here would
		// kill the ack, drop the connection, and fail the very provision the
		// refresh is supposed to be transparent to.
		//
		// So a push only ever STAGES this binary. Applying it is a separate,
		// genuinely-idle event: a teardown that empties the worker, or a control
		// connection closing with no session live (see applyPendingIfIdle).
		pending := filepath.Join(s.stagingDir(), BinWorkerClient+".pending")
		if err := os.Rename(staged, pending); err != nil {
			return "", fmt.Errorf("staging %s: %w", name, err)
		}
		return "staged", nil
	}

	if err := os.Rename(staged, filepath.Join(s.binDir(), name)); err != nil {
		return "", fmt.Errorf("installing %s: %w", name, err)
	}
	return "installed", nil
}

// applyWorkerClient replaces this service's own binary. The supervisor
// (launchd KeepAlive / systemd Restart) brings the new one up; exiting is how a
// service updates itself without an exec dance that would strand the listener.
func (s *Service) applyWorkerClient(staged string) error {
	// The bin dir normally exists (the service installer creates it), but a
	// staged binary can outlive a state-dir wipe, and failing the apply here
	// would leave the worker refusing sessions until it restarted.
	if err := os.MkdirAll(s.binDir(), 0755); err != nil {
		return fmt.Errorf("creating bin dir: %w", err)
	}
	if err := os.Rename(staged, filepath.Join(s.binDir(), BinWorkerClient)); err != nil {
		return fmt.Errorf("installing %s: %w", BinWorkerClient, err)
	}
	s.log.Info("worker binary replaced; exiting for the supervisor to restart it",
		"path", filepath.Join(s.binDir(), BinWorkerClient))
	if s.restartHook != nil {
		s.restartHook()
	}
	return nil
}

// ApplyPendingWorkerClient installs a previously staged worker binary if the
// worker is idle. Called when a teardown empties the worker and when a control
// connection closes, so a deferred upgrade lands at the first safe moment
// rather than waiting for the next push.
//
// The idle decision and the restart are ONE critical section, via s.restarting:
// otherwise a session_open on another connection could slip in between and be
// abandoned mid-bringup by the exit. Once restarting is set, session_open is
// refused with a retryable error rather than accepted and killed.
func (s *Service) ApplyPendingWorkerClient() {
	pending := filepath.Join(s.stagingDir(), BinWorkerClient+".pending")
	if _, err := os.Stat(pending); err != nil {
		return
	}

	s.mu.Lock()
	if len(s.sessions) > 0 || s.restarting {
		s.mu.Unlock()
		return
	}
	s.restarting = true
	s.mu.Unlock()

	if err := s.applyWorkerClient(pending); err != nil {
		s.log.Warn("applying staged worker binary", "err", err)
		// The swap failed, so we are not restarting after all: let sessions
		// through again rather than wedging the worker into refusing forever.
		s.mu.Lock()
		s.restarting = false
		s.mu.Unlock()
	}
}

// containerPlatform reports "<goos>-<goarch>" of this worker's docker daemon,
// or "" when there is none. It is what the work container runs — a macOS worker
// drives Linux containers, so this is routinely NOT the worker's own platform,
// and injecting the worker's own `gt` into a container would produce a binary
// the container cannot execute.
//
// Cached: the answer cannot change without the daemon restarting, and the
// handshake is on the hot path for every provision.
func (s *Service) containerPlatform() string {
	if !s.cfg.Docker {
		return ""
	}
	s.mu.Lock()
	cached := s.containerPlatformCache
	s.mu.Unlock()
	if cached != "" {
		return cached
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "version",
		"--format", "{{.Server.Os}}-{{.Server.Arch}}").Output()
	if err != nil {
		s.log.Warn("could not determine the container platform; container-mode binaries will not be pushed", "err", err)
		return ""
	}
	got := strings.TrimSpace(string(out))
	if !sockproto.ValidPlatformTag(got) {
		s.log.Warn("docker reported an unexpected platform", "platform", got)
		return ""
	}
	s.mu.Lock()
	s.containerPlatformCache = got
	s.mu.Unlock()
	return got
}

// containerInject resolves what a work container needs mounted: the dir bound
// at /opt/gt, and the CONTAINER-platform gt-proxy-client to inject as gt/bd.
//
// The operator's -gt-dir still wins when set — it predates this and may hold
// bits we know nothing about. Otherwise the worker uses its own state dir, and
// the proxy client comes from what the orchestrator pushed for the container's
// platform (§4.1). A worker that has not been pushed those yet simply injects
// nothing: the session still comes up, and the next provision (which pushes
// before opening) fixes it.
func (s *Service) containerInject() (gtDir, proxyClient string, err error) {
	gtDir = s.cfg.GTDir
	if gtDir == "" {
		gtDir = filepath.Join(s.cfg.StateDir, "gtdir")
	}
	if err := os.MkdirAll(gtDir, 0755); err != nil {
		return "", "", fmt.Errorf("creating gt-dir: %w", err)
	}

	platform := s.containerPlatform()
	if platform == "" {
		return gtDir, "", nil
	}

	// A LINUX worker running local Linux docker — the canonical
	// container-execution host — has a container platform equal to its own, and
	// the orchestrator does not send a redundant tagged copy for it. Its own
	// gt-proxy-client runs unmodified inside that container, so use it.
	//
	// (The earlier version of this looked only in the container tree, which
	// made injection silently no-op on exactly that mainstream deployment: the
	// agent came up with no gt/bd at all.)
	candidates := []string{filepath.Join(s.containerBinDir(platform), BinProxyClient)}
	if platform == sockproto.PlatformTag(runtime.GOOS, runtime.GOARCH) {
		candidates = append(candidates, filepath.Join(s.binDir(), BinProxyClient))
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return gtDir, candidate, nil
		}
	}
	// Fail LOUDLY rather than start a mute agent. An earlier version warned and
	// carried on, claiming the next provision would fix it — which is false on
	// the cross-platform path, where an up-to-date worker was never sent
	// container binaries at all. A container session whose agent cannot run
	// `gt` cannot call `gt done`: it looks alive and accomplishes nothing,
	// which is worse than a session that refuses to start with a clear reason.
	return "", "", fmt.Errorf("no gt-proxy-client to inject for container platform %s (looked in %v) — the orchestrator has not pushed it; `gt worker push-binaries <rig>` from the orchestrator, or `make dist` there if it has no %s artifacts",
		platform, candidates, platform)
}

// hasContainerBinaries reports whether an injectable client exists for the
// container platform, so the orchestrator can push one even when versions
// already match.
func (s *Service) hasContainerBinaries() bool {
	platform := s.containerPlatform()
	if platform == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(s.containerBinDir(platform), BinProxyClient)); err == nil {
		return true
	}
	// A same-platform container runs the worker's own binary.
	if platform == sockproto.PlatformTag(runtime.GOOS, runtime.GOARCH) {
		_, err := os.Stat(filepath.Join(s.binDir(), BinProxyClient))
		return err == nil
	}
	return false
}
