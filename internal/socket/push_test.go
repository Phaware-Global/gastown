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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

// TestPushBinaries_RefusesForeignPlatform pins the guard that matters most:
// this orchestrator has binaries for its OWN platform only, and installing one
// a worker cannot execute would replace a working `gt` with garbage — strictly
// worse than being stale.
func TestPushBinaries_RefusesForeignPlatform(t *testing.T) {
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
	assert.Contains(t, err.Error(), runtime.GOOS+"/"+runtime.GOARCH)
	assert.Empty(t, w.pushed(), "nothing may be sent to a platform we cannot build for")
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

	applied, err := pushOne(context.Background(), c, "gt-proxy-client", path)
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

	_, err = pushOne(context.Background(), c, "gt-proxy-client", path)
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

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "gt-proxy-client"), []byte("payload"), 0755))
	defer SetBinarySourceForTest(src)()

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
