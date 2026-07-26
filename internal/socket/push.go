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

// binarySourceFn locates the binaries to push; a var so tests can point it at
// a fixture dir instead of the test binary's own directory.
var binarySourceFn = binarySource

// binarySource locates the binaries to push: the ones installed alongside the
// running gt, so a worker ends up on exactly the build this orchestrator is.
func binarySource() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating this binary: %w", err)
	}
	return filepath.Dir(self), nil
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

	// The orchestrator only has binaries for its OWN platform. Pushing a
	// darwin-arm64 build to a linux worker would replace a working `gt` with an
	// unexecutable file — worse than being stale — so refuse, and name both
	// sides so the operator knows what to build.
	if ack.OS != "" && ack.Arch != "" && (ack.OS != runtime.GOOS || ack.Arch != runtime.GOARCH) {
		return nil, fmt.Errorf("socket: worker %s is %s/%s but this orchestrator has %s/%s binaries — refusing to push a binary it cannot run (upgrade that worker in place)",
			ack.WorkerID, ack.OS, ack.Arch, runtime.GOOS, runtime.GOARCH)
	}

	dir, err := binarySourceFn()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()

	var results []PushResult
	for _, name := range PushableBinaries {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			// Not every install has every companion; skip what is not there
			// rather than fail the whole refresh.
			continue
		}
		applied, err := pushOne(ctx, c, name, path)
		if err != nil {
			return results, fmt.Errorf("socket: pushing %s: %w", name, err)
		}
		results = append(results, PushResult{Name: name, Applied: applied})
	}
	return results, nil
}

// PushResult reports what happened to one binary on the worker.
type PushResult struct {
	Name    string `json:"name"`
	Applied string `json:"applied"` // "installed" | "staged"
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
	defer c.close()
	return b.pushTo(ctx, c, true)
}

// pushOne streams a single binary and returns how the worker applied it
// ("installed" or "staged").
func pushOne(ctx context.Context, c *conn, name, path string) (string, error) {
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
			Type: sockproto.TypePushBinary,
			Name: name,
			Data: base64.StdEncoding.EncodeToString(buf[:n]),
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

// SetBinarySourceForTest overrides where pushable binaries are read from, so an
// end-to-end test can drive a real transfer without writing into the directory
// the test binary happens to live in. Returns a restore func.
func SetBinarySourceForTest(dir string) func() {
	prev := binarySourceFn
	binarySourceFn = func() (string, error) { return dir, nil }
	return func() { binarySourceFn = prev }
}
