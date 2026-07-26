package workerclient

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// stagingDir holds a partially-received or deferred binary.
func (s *Service) stagingDir() string { return filepath.Join(s.cfg.StateDir, "staging") }

// pushState accumulates one in-flight transfer on a connection. A push is
// per-connection so two orchestrators (or a retry after a drop) cannot
// interleave chunks into one file.
type pushState struct {
	name string
	f    *os.File
	sum  []byte // running sha256 over what was written
	n    int64
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

	if !pushableBinaries[m.Name] {
		fail("bad_request", "%q is not a pushable binary (allowed: %s)", m.Name, strings.Join([]string{BinProxyClient, BinWorkerClient}, ", "))
		return
	}

	// Starting a different file mid-stream is a protocol error, not a reset:
	// silently discarding a half-written binary would hide a bug that could
	// otherwise install a spliced file.
	if c.push != nil && c.push.name != m.Name {
		fail("proto", "push for %q interrupted by %q on the same connection", c.push.name, m.Name)
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
		c.push = &pushState{name: m.Name, f: f, sum: nil}
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
	name := c.push.name
	c.push = nil
	c.pushHash = nil

	applied, err := s.installPushed(name, staged)
	if err != nil {
		_ = os.Remove(staged)
		s.log.Warn("installing pushed binary", "name", name, "err", err)
		_ = c.send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Code: "io", Msg: err.Error()})
		return
	}
	s.log.Info("pushed binary accepted", "name", name, "bytes", "-", "applied", applied)
	_ = c.send(&sockproto.Message{Type: sockproto.TypePushBinaryAck, ID: m.ID, Name: name, Applied: applied})
}

// installPushed moves a verified binary into place, or stages it when applying
// now would kill work. Returns "installed" or "staged".
func (s *Service) installPushed(name, staged string) (string, error) {
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
