package socket

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/execution"
	"github.com/steveyegge/gastown/internal/sockproto"
)

// TestPushBinaries_SkipsWhenVersionsMatch pins that a fresh worker costs
// nothing: Provision runs on every polecat start, so pushing 13MB when there is
// nothing to update would be a per-session tax.
func TestPushBinaries_SkipsWhenVersionsMatch(t *testing.T) {
	w := newFakeWorker(t)
	w.gtVersion = "1.2.3"
	b, _ := testBackend(t, w)
	b.GTVersion = "1.2.3"

	c, err := b.dial(context.Background())
	require.NoError(t, err)
	defer c.close()

	require.NoError(t, b.pushBinaries(context.Background(), c))
	assert.Empty(t, w.pushed(), "a matching version must not transfer anything")
}

// TestPushBinaries_SkipsDevBuilds pins that an unversioned build opts out: "dev"
// on either side would otherwise differ from everything and push on every
// provision, forever.
func TestPushBinaries_SkipsDevBuilds(t *testing.T) {
	for _, tc := range []struct{ orch, worker string }{
		{"dev", "1.2.3"},
		{"1.2.3", "dev"},
		{"", "1.2.3"},
	} {
		w := newFakeWorker(t)
		w.gtVersion = tc.worker
		b, _ := testBackend(t, w)
		b.GTVersion = tc.orch

		c, err := b.dial(context.Background())
		require.NoError(t, err)
		require.NoError(t, b.pushBinaries(context.Background(), c))
		assert.Empty(t, w.pushed(), "orch=%q worker=%q must not push", tc.orch, tc.worker)
		c.close()
	}
}

// TestPushBinaries_ForeignPlatformNeedsItsOwnArtifacts pins what replaced the
// blanket refusal: a worker on another platform is served from the artifact
// tree, and when that platform is missing the error says which one to build
// rather than shipping a binary the worker cannot execute.
func TestPushBinaries_ForeignPlatformNeedsItsOwnArtifacts(t *testing.T) {
	root := t.TempDir()
	defer SetBinarySourceForTest(root)()

	w := newFakeWorker(t)
	w.gtVersion = "1.0.0"
	w.os, w.arch = "windows", "386"
	b, _ := testBackend(t, w)
	b.GTVersion = "1.2.3"

	c, err := b.dial(context.Background())
	require.NoError(t, err)
	defer c.close()

	err = b.pushBinaries(context.Background(), c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "windows/386")
	assert.Contains(t, err.Error(), "make dist")
	assert.Empty(t, w.pushed(), "nothing may be sent until that platform is built")

	// Build that platform's artifacts and it goes through.
	dir := filepath.Join(root, PlatformDir("windows", "386"))
	require.NoError(t, os.MkdirAll(dir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gt-proxy-client"), []byte("win-payload"), 0755))

	c2, err := b.dial(context.Background())
	require.NoError(t, err)
	defer c2.close()
	require.NoError(t, b.pushBinaries(context.Background(), c2))
	got := w.pushed()
	require.Contains(t, got, "gt-proxy-client")
	assert.Equal(t, []byte("win-payload"), got["gt-proxy-client"].data)
}

// TestPushBinaries_AlsoSendsContainerPlatform pins the case that makes this
// necessary on a single machine: the work container is a LINUX container, so a
// macOS worker needs Linux gt/bd for it — tagged, so the worker stores them for
// injection rather than installing binaries it cannot run.
func TestPushBinaries_AlsoSendsContainerPlatform(t *testing.T) {
	root := t.TempDir()
	native := filepath.Join(root, PlatformDir(runtime.GOOS, runtime.GOARCH))
	require.NoError(t, os.MkdirAll(native, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(native, "gt-proxy-client"), []byte("native"), 0755))
	linux := filepath.Join(root, PlatformDir("linux", "arm64"))
	require.NoError(t, os.MkdirAll(linux, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(linux, "gt-proxy-client"), []byte("linux"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(linux, "gt-worker-client"), []byte("linux-worker"), 0755))
	defer SetBinarySourceForTest(root)()

	w := newFakeWorker(t)
	w.gtVersion = "1.0.0"
	w.containerPlatform = "linux-arm64"
	b, _ := testBackend(t, w)
	b.GTVersion = "1.2.3"

	c, err := b.dial(context.Background())
	require.NoError(t, err)
	defer c.close()
	require.NoError(t, b.pushBinaries(context.Background(), c))

	got := w.pushed()
	assert.Equal(t, []byte("native"), got["gt-proxy-client"].data, "the worker's own binary is untagged")
	assert.Equal(t, []byte("linux"), got["linux-arm64/gt-proxy-client"].data, "the container's binary is tagged")
	assert.NotContains(t, got, "linux-arm64/gt-worker-client",
		"a container never runs the worker service, so it must not be sent one")
}

// TestPushOne_StreamsAndVerifies pins the transfer itself: chunked, with a
// whole-file digest the worker can check before installing anything.
func TestPushOne_StreamsAndVerifies(t *testing.T) {
	w := newFakeWorker(t)
	b, _ := testBackend(t, w)

	// Larger than one chunk, so the multi-chunk path is what runs.
	payload := make([]byte, sockproto.PushChunkBytes*2+1234)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "gt-proxy-client")
	require.NoError(t, os.WriteFile(path, payload, 0755))

	c, err := b.dial(context.Background())
	require.NoError(t, err)
	defer c.close()

	applied, err := pushOne(context.Background(), c, "gt-proxy-client", path, "")
	require.NoError(t, err)
	assert.Equal(t, "installed", applied)

	got := w.pushed()
	require.Len(t, got, 1)
	assert.Equal(t, payload, got["gt-proxy-client"].data, "every byte must arrive intact across chunks")
	sum := sha256.Sum256(payload)
	assert.Equal(t, hex.EncodeToString(sum[:]), got["gt-proxy-client"].sha,
		"the digest must cover the whole file, not a chunk")
}

// TestPushOne_ReportsWorkerRefusal pins that a worker's refusal surfaces rather
// than being counted as success.
func TestPushOne_ReportsWorkerRefusal(t *testing.T) {
	w := newFakeWorker(t)
	w.refusePush = "integrity"
	b, _ := testBackend(t, w)

	path := filepath.Join(t.TempDir(), "gt-proxy-client")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0755))

	c, err := b.dial(context.Background())
	require.NoError(t, err)
	defer c.close()

	_, err = pushOne(context.Background(), c, "gt-proxy-client", path, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "integrity")
}

var _ = base64.StdEncoding

// TestPushBinariesTo_ForcesRegardlessOfVersion pins the operator path: someone
// who ran the command explicitly wants the transfer, even when the versions
// already agree (a worker whose files were tampered with or half-copied reports
// the same version as a healthy one).
func TestPushBinariesTo_ForcesRegardlessOfVersion(t *testing.T) {
	w := newFakeWorker(t)
	w.gtVersion = "1.2.3"
	b, _ := testBackend(t, w)
	b.GTVersion = "1.2.3"

	root := t.TempDir()
	native := filepath.Join(root, PlatformDir(runtime.GOOS, runtime.GOARCH))
	require.NoError(t, os.MkdirAll(native, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(native, "gt-proxy-client"), []byte("payload"), 0755))
	defer SetBinarySourceForTest(root)()

	results, err := b.PushBinariesTo(context.Background())
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "gt-proxy-client", results[0].Name)
	assert.Equal(t, "installed", results[0].Applied)
}

// TestPushBinariesTo_ReportsErrors pins the difference from the Provision hook:
// an operator who asked for a push must be told it did not happen.
func TestPushBinariesTo_ReportsErrors(t *testing.T) {
	w := newFakeWorker(t)
	w.os, w.arch = "plan9", "mips"
	b, _ := testBackend(t, w)

	_, err := b.PushBinariesTo(context.Background())
	require.Error(t, err, "the operator path must not swallow what the automatic path logs")
	assert.Contains(t, err.Error(), "plan9/mips")
}

// TestProvision_RetriesARestartingWorker pins the contract the worker's
// "restarting" refusal claims: its supervisor brings it straight back, so a
// polecat start that races a binary upgrade must succeed on the retry rather
// than fail once. Nothing upstream retries — buildRemoteArgv calls Provision
// exactly once — so this is the only place it can happen.
func TestProvision_RetriesARestartingWorker(t *testing.T) {
	prev := provisionRetryDelay
	provisionRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { provisionRetryDelay = prev })

	w := newFakeWorker(t)
	w.restartingUntilAttempt = 2 // refuse the first two session_opens
	b, _ := testBackend(t, w)

	ep, err := b.Provision(context.Background(), execution.PolecatSpec{
		Rig: "demo", Polecat: "furiosa", Session: "gt-demo-furiosa", Config: b.cfg,
	})
	require.NoError(t, err, "a restarting worker must not fail the provision")
	assert.Equal(t, "gt-demo-furiosa", ep.ID)
	assert.GreaterOrEqual(t, w.sessionOpenAttempts(), 3, "it must actually have retried")
}

// TestProvision_GivesUpOnAWorkerThatStaysDown pins the bound: retries are for a
// supervisor restart, not for masking a dead worker into a long stall.
func TestProvision_GivesUpOnAWorkerThatStaysDown(t *testing.T) {
	prev := provisionRetryDelay
	provisionRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { provisionRetryDelay = prev })

	w := newFakeWorker(t)
	w.restartingUntilAttempt = 99
	b, _ := testBackend(t, w)

	_, err := b.Provision(context.Background(), execution.PolecatSpec{
		Rig: "demo", Polecat: "furiosa", Session: "gt-demo-furiosa", Config: b.cfg,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restarting")
	assert.Equal(t, provisionRetries+1, w.sessionOpenAttempts(), "attempts must be bounded")
}

// TestProvision_DoesNotRetryARealRejection pins that only the transient shape
// retries: a worker at its session limit must fail immediately, not three times.
func TestProvision_DoesNotRetryARealRejection(t *testing.T) {
	w := newFakeWorker(t)
	w.failOpenWith = "max_sessions"
	b, _ := testBackend(t, w)

	_, err := b.Provision(context.Background(), execution.PolecatSpec{
		Rig: "demo", Polecat: "furiosa", Session: "gt-demo-furiosa", Config: b.cfg,
	})
	require.Error(t, err)
	assert.Equal(t, 1, w.sessionOpenAttempts(), "a real rejection must not be retried")
}

// TestPushBinaries_RefusesAMaliciousWorkerPlatform pins that the ORCHESTRATOR
// validates what the worker claims before joining it to a local path. The
// receiver already checks the tag we send it; checking on one side only is how
// a traversal gets in — here an enrolled-but-compromised worker could walk this
// machine's filesystem for a file named gt-proxy-client and have it streamed
// back to it.
func TestPushBinaries_RefusesAMaliciousWorkerPlatform(t *testing.T) {
	root := t.TempDir()
	defer SetBinarySourceForTest(root)()

	t.Run("os/arch", func(t *testing.T) {
		w := newFakeWorker(t)
		w.gtVersion = "1.0.0"
		w.os, w.arch = "../../../../tmp/evil", "x"
		b, _ := testBackend(t, w)
		b.GTVersion = "1.2.3"

		c, err := b.dial(context.Background())
		require.NoError(t, err)
		defer c.close()

		err = b.pushBinaries(context.Background(), c)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid platform")
		assert.Empty(t, w.pushed())
	})

	t.Run("container platform", func(t *testing.T) {
		w := newFakeWorker(t)
		w.gtVersion = "1.0.0"
		w.containerPlatform = "../../../etc-x"
		b, _ := testBackend(t, w)
		b.GTVersion = "1.2.3"

		// The worker's own platform is fine, so it gets that far; the container
		// tag is what must be refused.
		native := filepath.Join(root, PlatformDir(runtime.GOOS, runtime.GOARCH))
		require.NoError(t, os.MkdirAll(native, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(native, "gt-proxy-client"), []byte("ok"), 0755))

		c, err := b.dial(context.Background())
		require.NoError(t, err)
		defer c.close()

		err = b.pushBinaries(context.Background(), c)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "container platform")
	})
}
