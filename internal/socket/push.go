package socket

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/sockproto"
)

// Binary freshness (§4.1 push_binaries, §5 Provision).
//
// A worker runs gastown binaries that are version-coupled to this
// orchestrator's control plane — gt-proxy-client is the agent's `gt` and `bd`
// — and nothing on the worker notices when they drift. So the orchestrator
// compares versions at handshake and pushes what differs.

// PushableBinaries are the companions a worker runs, in install order: the
// proxy client first, since it is what an agent needs, and the worker service
// last because applying it may cost a restart.
var PushableBinaries = []string{"gt-proxy-client", "gt-worker-client"}

// pushTimeout bounds a full binary transfer.
const pushTimeout = 10 * time.Minute

// EnvBinaryDir overrides where per-platform artifacts are read from.
const EnvBinaryDir = "GT_BINARY_DIR"

// artifactRootFn locates the per-platform artifact tree; a var so tests can
// point it at a fixture instead of the running binary's neighbourhood.
var artifactRootFn = artifactRoot

// artifactRoot is the tree `make dist` writes: <root>/<goos>-<goarch>/<binary>.
// Derived from the running binary so an orchestrator installed anywhere finds
// its own artifacts (~/.local/bin/gt → ~/.local/share/gt/binaries).
func artifactRoot() (string, error) {
	if dir := os.Getenv(EnvBinaryDir); dir != "" {
		return dir, nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating this binary: %w", err)
	}
	return filepath.Join(filepath.Dir(self), "..", "share", "gt", "binaries"), nil
}

// PlatformDir is the artifact subdirectory for a platform.
func PlatformDir(goos, goarch string) string { return goos + "-" + goarch }

// binariesFor resolves the directory holding the binaries for a platform.
//
// A same-platform worker falls back to the orchestrator's own install dir when
// no artifact tree exists, so a plain `make install` (which also populates the
// tree) keeps working for the common single-platform case. A DIFFERENT platform
// has no such fallback: pushing this machine's binaries there is precisely the
// mistake the os/arch guard existed to prevent.
func binariesFor(goos, goarch string) (string, error) {
	root, err := artifactRootFn()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, PlatformDir(goos, goarch))
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return dir, nil
	}
	if goos == runtime.GOOS && goarch == runtime.GOARCH {
		self, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("locating this binary: %w", err)
		}
		return filepath.Dir(self), nil
	}
	return "", fmt.Errorf("socket: no %s/%s binaries in %s — build them with `make dist` (this orchestrator is %s/%s, so its own binaries cannot run there)",
		goos, goarch, root, runtime.GOOS, runtime.GOARCH)
}

// pushBinaries streams the companion binaries to a worker whose version
// differs. It is best-effort from Provision's point of view: a worker that
// cannot be refreshed still runs sessions (protocol version, not gt version, is
// the compatibility gate), so a push failure is reported and stepped over
// rather than failing the session a user is waiting on.
func (b *SocketBackend) pushBinaries(ctx context.Context, c *conn) error {
	_, err := b.pushTo(ctx, c, false)
	return err
}

// pushTo compares versions (unless forced) and streams what differs.
func (b *SocketBackend) pushTo(ctx context.Context, c *conn, force bool) ([]PushResult, error) {
	ack := c.ack
	if ack == nil {
		return nil, fmt.Errorf("socket: no handshake to compare versions against")
	}
	if !force {
		// "dev" on either side means an unversioned build: comparing it would
		// push on every single provision, so skip rather than guess.
		if b.GTVersion == "" || b.GTVersion == "dev" || ack.GTVersion == "dev" {
			return nil, nil
		}
		if ack.GTVersion == b.GTVersion {
			return nil, nil
		}
	}

	workerOS, workerArch := ack.OS, ack.Arch
	if workerOS == "" || workerArch == "" {
		workerOS, workerArch = runtime.GOOS, runtime.GOARCH
	}

	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()

	var results []PushResult

	// The worker's own binaries.
	sent, err := b.pushPlatform(ctx, c, workerOS, workerArch, "")
	results = append(results, sent...)
	if err != nil {
		return results, err
	}

	// Container mode needs a SECOND platform: the work container is a Linux
	// container, so a macOS worker cannot inject its own `gt` into one. The
	// worker reports the platform its docker daemon runs (§4.1), and those
	// binaries are stored separately for injection — never installed as the
	// worker's own.
	if cp := ack.Capabilities.GetContainerPlatform(); cp != "" && cp != PlatformDir(workerOS, workerArch) {
		cpOS, cpArch, ok := splitPlatform(cp)
		if !ok {
			return results, fmt.Errorf("socket: worker reported an unparseable container platform %q", cp)
		}
		sent, err := b.pushPlatform(ctx, c, cpOS, cpArch, cp)
		results = append(results, sent...)
		if err != nil {
			return results, err
		}
	}
	return results, nil
}

// pushPlatform streams every pushable binary for one platform. platform is the
// wire tag: empty means "the worker's own", anything else is stored for
// container injection.
func (b *SocketBackend) pushPlatform(ctx context.Context, c *conn, goos, goarch, platform string) ([]PushResult, error) {
	dir, err := binariesFor(goos, goarch)
	if err != nil {
		return nil, err
	}
	var results []PushResult
	for _, name := range PushableBinaries {
		if platform != "" && name == "gt-worker-client" {
			continue // the container never runs the worker service
		}
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			// Not every install has every companion; skip what is not there
			// rather than fail the whole refresh.
			continue
		}
		applied, err := pushOne(ctx, c, name, path, platform)
		if err != nil {
			return results, fmt.Errorf("socket: pushing %s (%s/%s): %w", name, goos, goarch, err)
		}
		results = append(results, PushResult{Name: name, Platform: PlatformDir(goos, goarch), Applied: applied})
	}
	return results, nil
}

// splitPlatform parses a "<goos>-<goarch>" tag.
func splitPlatform(p string) (goos, goarch string, ok bool) {
	goos, goarch, ok = strings.Cut(p, "-")
	return goos, goarch, ok && goos != "" && goarch != ""
}

// PushResult reports what happened to one binary on the worker.
type PushResult struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Applied  string `json:"applied"` // "installed" | "staged"
}

// PushBinariesTo refreshes a worker on demand — the operator path. Unlike the
// Provision hook it returns errors rather than logging them: an operator who
// asked for a push needs to know it did not happen, while a polecat start must
// not fail over a version bump.
func (b *SocketBackend) PushBinariesTo(ctx context.Context) ([]PushResult, error) {
	c, err := b.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.close() }()
	return b.pushTo(ctx, c, true)
}

// pushOne streams a single binary and returns how the worker applied it
// ("installed" or "staged").
func pushOne(ctx context.Context, c *conn, name, path, platform string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Digest first: the worker verifies the whole file before installing
	// anything, so the digest has to travel with the FINAL chunk, which means
	// knowing it up front.
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	buf := make([]byte, sockproto.PushChunkBytes)
	for {
		n, readErr := f.Read(buf)
		atEOF := readErr == io.EOF || (readErr == nil && n == 0)
		if readErr != nil && readErr != io.EOF {
			return "", readErr
		}

		msg := &sockproto.Message{
			Type:     sockproto.TypePushBinary,
			Name:     name,
			Platform: platform,
			Data:     base64.StdEncoding.EncodeToString(buf[:n]),
		}
		if atEOF {
			msg.ID = "push-" + name
			msg.EOF = true
			msg.SHA256 = sum
			// Only the terminal chunk expects a reply; mid-stream chunks are
			// streamed without a round trip per 512 KiB.
			resp, err := c.request(ctx, msg)
			if err != nil {
				return "", err
			}
			if resp.Type == sockproto.TypeError {
				return "", fmt.Errorf("worker refused: %s: %s", resp.Code, resp.Msg)
			}
			if resp.Type != sockproto.TypePushBinaryAck {
				return "", fmt.Errorf("unexpected reply %q to push_binaries", resp.Type)
			}
			return resp.Applied, nil
		}
		if err := c.sendOnly(ctx, msg); err != nil {
			return "", err
		}
	}
}

// SetBinarySourceForTest overrides the artifact ROOT, so an end-to-end test can
// drive a real transfer without writing into the directory the test binary
// happens to live in. Returns a restore func.
func SetBinarySourceForTest(root string) func() {
	prev := artifactRootFn
	artifactRootFn = func() (string, error) { return root, nil }
	return func() { artifactRootFn = prev }
}
