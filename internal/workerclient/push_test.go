package workerclient

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/socket"
	"github.com/steveyegge/gastown/internal/sockproto"
	"github.com/steveyegge/gastown/internal/version"
)

// pushBinary streams payload to the worker as name and returns the final reply.
func pushBinary(t *testing.T, addr, name string, payload []byte, corruptSum bool) *sockproto.Message {
	t.Helper()
	nc := rawDial(t, addr)
	codec := sockproto.NewCodec(nc)
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeAuth, Token: "t0k"}))
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeHello, ID: "h", ProtoVersion: sockproto.ProtoVersion}))
	_, err := codec.Recv()
	require.NoError(t, err)

	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	if corruptSum {
		digest = hex.EncodeToString(make([]byte, 32))
	}

	const chunk = 4096
	for off := 0; off < len(payload); off += chunk {
		end := off + chunk
		if end > len(payload) {
			end = len(payload)
		}
		require.NoError(t, codec.Send(&sockproto.Message{
			Type: sockproto.TypePushBinary, Name: name,
			Data: base64.StdEncoding.EncodeToString(payload[off:end]),
		}))
	}
	require.NoError(t, codec.Send(&sockproto.Message{
		Type: sockproto.TypePushBinary, ID: "p1", Name: name, EOF: true, SHA256: digest,
	}))
	resp, err := codec.Recv()
	require.NoError(t, err)
	return resp
}

// TestPush_InstallsProxyClient pins the freshness path that matters most:
// gt-proxy-client IS the agent's gt and bd, so an install must land atomically
// in the worker's own bin dir — where the shims point.
func TestPush_InstallsProxyClient(t *testing.T) {
	proxyURL, _, _ := startProxy(t)
	addr, svc := startService(t, proxyURL)

	payload := []byte("#!/bin/sh\necho new-proxy-client\n")
	resp := pushBinary(t, addr, BinProxyClient, payload, false)
	require.Equal(t, sockproto.TypePushBinaryAck, resp.Type, "%s: %s", resp.Code, resp.Msg)
	assert.Equal(t, "installed", resp.Applied)

	got, err := os.ReadFile(filepath.Join(svc.binDir(), BinProxyClient))
	require.NoError(t, err)
	assert.Equal(t, payload, got)

	fi, err := os.Stat(filepath.Join(svc.binDir(), BinProxyClient))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0755), fi.Mode().Perm(), "the agent must be able to execute it")

	// Nothing is left behind in staging.
	entries, err := os.ReadDir(svc.stagingDir())
	if err == nil {
		assert.Empty(t, entries, "a completed push must not leave staging files")
	}
}

// TestPush_RefusesCorruptPayload pins the one integrity gate (§12 decision 2):
// a binary that does not match its digest must never reach the bin dir, since
// installing it would break `gt` on the worker with no way to notice.
func TestPush_RefusesCorruptPayload(t *testing.T) {
	proxyURL, _, _ := startProxy(t)
	addr, svc := startService(t, proxyURL)

	resp := pushBinary(t, addr, BinProxyClient, []byte("payload"), true)
	require.Equal(t, sockproto.TypeError, resp.Type)
	assert.Equal(t, "integrity", resp.Code)

	_, err := os.Stat(filepath.Join(svc.binDir(), BinProxyClient))
	assert.True(t, os.IsNotExist(err), "a mismatched binary must not be installed")
	entries, err := os.ReadDir(svc.stagingDir())
	if err == nil {
		assert.Empty(t, entries, "the partial file must be cleaned up")
	}
}

// TestPush_RefusesUnknownName pins the allowlist: `name` comes from the wire and
// is joined to a path, which is exactly how a traversal gets in.
func TestPush_RefusesUnknownName(t *testing.T) {
	proxyURL, _, _ := startProxy(t)
	addr, svc := startService(t, proxyURL)

	for _, name := range []string{"../../../../etc/cron.d/evil", "gt", "sshd", ""} {
		resp := pushBinary(t, addr, name, []byte("x"), false)
		require.Equal(t, sockproto.TypeError, resp.Type, "name %q must be refused", name)
		assert.Equal(t, "bad_request", resp.Code)
	}
	_, err := os.Stat(filepath.Join(svc.cfg.StateDir, "..", "..", "etc"))
	assert.Error(t, err)
}

// TestPush_WorkerClientNeverRestartsOnTheRequestPath is the regression test for
// the bug this file previously hid: applying gt-worker-client means exiting for
// the supervisor, and doing that inside the push handler killed the ack and the
// connection — the same connection Provision reuses for session_open, so the
// refresh failed the provision it was supposed to be invisible to.
//
// A push now only ever STAGES this binary, live session or not.
func TestPush_WorkerClientNeverRestartsOnTheRequestPath(t *testing.T) {
	restarted := make(chan struct{}, 4)
	proxyURL, _, _ := startProxy(t)
	addr, svc := startService(t, proxyURL, func(c *Config) {
		c.RestartHook = func() { restarted <- struct{}{} }
	})

	// Idle worker — the case that used to exit mid-request.
	nc := rawDial(t, addr)
	codec := sockproto.NewCodec(nc)
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeAuth, Token: "t0k"}))
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeHello, ID: "h", ProtoVersion: sockproto.ProtoVersion}))
	_, err := codec.Recv()
	require.NoError(t, err)

	payload := []byte("#!/bin/sh\necho newer-worker\n")
	sum := sha256.Sum256(payload)
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypePushBinary, Name: BinWorkerClient,
		Data: base64.StdEncoding.EncodeToString(payload)}))
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypePushBinary, ID: "p1",
		Name: BinWorkerClient, EOF: true, SHA256: hex.EncodeToString(sum[:])}))

	resp, err := codec.Recv()
	require.NoError(t, err, "the ack must arrive — the worker must not die mid-request")
	require.Equal(t, sockproto.TypePushBinaryAck, resp.Type, "%s: %s", resp.Code, resp.Msg)
	assert.Equal(t, "staged", resp.Applied)

	select {
	case <-restarted:
		t.Fatal("the worker restarted while the orchestrator still held the connection")
	case <-time.After(300 * time.Millisecond):
	}

	// The connection is still usable — which is the whole point, since
	// Provision issues session_open on it next.
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypePing, ID: "ping"}))
	pong, err := codec.Recv()
	require.NoError(t, err)
	assert.Equal(t, sockproto.TypePong, pong.Type)

	// Closing the connection is the safe moment: the upgrade lands there.
	require.NoError(t, nc.Close())
	select {
	case <-restarted:
	case <-time.After(15 * time.Second):
		t.Fatal("a staged upgrade never applied after the connection closed")
	}
	got, err := os.ReadFile(filepath.Join(svc.binDir(), BinWorkerClient))
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

// TestPush_WorkerClientDeferredWhileBusy pins that a live session holds the
// upgrade off entirely — and that ending the session is what releases it.
func TestPush_WorkerClientDeferredWhileBusy(t *testing.T) {
	restarted := make(chan struct{}, 4)
	proxyURL, ca, _ := startProxy(t)
	addr, svc := startService(t, proxyURL, func(c *Config) {
		c.RestartHook = func() { restarted <- struct{}{} }
	})
	b := newBackend(t, addr, ca)
	ep, err := b.Provision(context.Background(), polecatSpec())
	require.NoError(t, err)

	payload := []byte("#!/bin/sh\necho newer-worker\n")
	resp := pushBinary(t, addr, BinWorkerClient, payload, false)
	require.Equal(t, sockproto.TypePushBinaryAck, resp.Type, "%s: %s", resp.Code, resp.Msg)
	assert.Equal(t, "staged", resp.Applied)

	// pushBinary's own connection closed, but a session is live, so nothing
	// applies: a restart would abandon the agent.
	select {
	case <-restarted:
		t.Fatal("the worker restarted with a session live")
	case <-time.After(300 * time.Millisecond):
	}
	_, err = os.Stat(filepath.Join(svc.binDir(), BinWorkerClient))
	assert.True(t, os.IsNotExist(err), "the staged binary must not be live yet")

	require.NoError(t, b.Teardown(context.Background(), ep))
	select {
	case <-restarted:
	case <-time.After(30 * time.Second):
		t.Fatal("a staged upgrade never applied after the worker went idle")
	}
	got, err := os.ReadFile(filepath.Join(svc.binDir(), BinWorkerClient))
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

// TestPush_RestartingRefusesNewSessions pins the race the idle re-check used to
// leave open: between deciding "idle" and exiting, a session_open on another
// connection could be accepted and then abandoned mid-bringup.
func TestPush_RestartingRefusesNewSessions(t *testing.T) {
	proxyURL, _, _ := startProxy(t)
	addr, svc := startService(t, proxyURL, func(c *Config) {
		c.RestartHook = func() {} // hold the process in the restarting state
	})

	// Enter the restarting state exactly as an apply does.
	require.NoError(t, os.MkdirAll(svc.stagingDir(), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(svc.stagingDir(), BinWorkerClient+".pending"),
		[]byte("newer"), 0755))
	svc.ApplyPendingWorkerClient()

	svc.mu.Lock()
	restarting := svc.restarting
	svc.mu.Unlock()
	require.True(t, restarting, "applying a staged binary must mark the worker restarting")

	nc := rawDial(t, addr)
	codec := sockproto.NewCodec(nc)
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeAuth, Token: "t0k"}))
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeHello, ID: "h", ProtoVersion: sockproto.ProtoVersion}))
	_, err := codec.Recv()
	require.NoError(t, err)
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeSessionOpen, ID: "o",
		Session: "gt-demo-furiosa", Rig: "demo", Polecat: "furiosa", ExecMode: "native"}))
	resp, err := codec.Recv()
	require.NoError(t, err)
	assert.Equal(t, sockproto.TypeSessionError, resp.Type)
	assert.Equal(t, "restarting", resp.Code, "a session accepted now would be abandoned by the exit")
}

// TestPush_InterleavedNamesRefused pins that a second file cannot start
// mid-stream: splicing two binaries would produce a file that passes no digest
// but might have been installed by a buggier implementation.
func TestPush_InterleavedNamesRefused(t *testing.T) {
	proxyURL, _, _ := startProxy(t)
	addr, _ := startService(t, proxyURL)

	nc := rawDial(t, addr)
	codec := sockproto.NewCodec(nc)
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeAuth, Token: "t0k"}))
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeHello, ID: "h", ProtoVersion: sockproto.ProtoVersion}))
	_, err := codec.Recv()
	require.NoError(t, err)

	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypePushBinary, Name: BinProxyClient,
		Data: base64.StdEncoding.EncodeToString([]byte("first"))}))
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypePushBinary, ID: "x", Name: BinWorkerClient,
		Data: base64.StdEncoding.EncodeToString([]byte("second"))}))
	resp, err := codec.Recv()
	require.NoError(t, err)
	assert.Equal(t, sockproto.TypeError, resp.Type)
	assert.Equal(t, "proto", resp.Code)
}

// TestHelloAck_ReportsVersion pins the field the whole mechanism turns on:
// without it the orchestrator has nothing to compare and a worker drifts
// silently.
func TestHelloAck_ReportsVersion(t *testing.T) {
	proxyURL, _, _ := startProxy(t)
	addr, _ := startService(t, proxyURL)

	nc := rawDial(t, addr)
	codec := sockproto.NewCodec(nc)
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeAuth, Token: "t0k"}))
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeHello, ID: "h", ProtoVersion: sockproto.ProtoVersion}))
	ack, err := codec.Recv()
	require.NoError(t, err)
	assert.NotEmpty(t, ack.GTVersion, "hello_ack must carry the worker's gt version")
	assert.NotEmpty(t, ack.OS)
	assert.NotEmpty(t, ack.Arch)
}

var _ = socket.BackendName

// TestPushBinaries_EndToEnd drives the whole mechanism the way Provision does:
// a real SocketBackend, a real worker service, a version difference — and the
// binary lands in the worker's own bin dir, where the gt/bd shims point.
func TestPushBinaries_EndToEnd(t *testing.T) {
	src := t.TempDir()
	payload := make([]byte, sockproto.PushChunkBytes+77) // spans chunks
	for i := range payload {
		payload[i] = byte(i % 253)
	}
	require.NoError(t, os.WriteFile(filepath.Join(src, "gt-proxy-client"), payload, 0755))
	// BOTH binaries, as a real orchestrator has: the earlier version of this
	// test shipped only the proxy client, so the gt-worker-client path — the one
	// that used to kill the connection Provision reuses — was never exercised.
	require.NoError(t, os.WriteFile(filepath.Join(src, "gt-worker-client"), []byte("newer-worker"), 0755))
	defer socket.SetBinarySourceForTest(src)()

	// Both sides need a REAL version: an unversioned "dev" build opts out of
	// freshness entirely, which is itself the behavior TestPushBinaries_SkipsDevBuilds
	// covers.
	restore := version.GTVersion
	version.GTVersion = "1.0.0-worker"
	t.Cleanup(func() { version.GTVersion = restore })

	proxyURL, ca, _ := startProxy(t)
	addr, svc := startService(t, proxyURL)
	b := newBackend(t, addr, ca)
	b.GTVersion = "9.9.9-orchestrator"

	_, err := b.Provision(context.Background(), polecatSpec())
	require.NoError(t, err, "a binary refresh must not break session bringup")

	got, err := os.ReadFile(filepath.Join(svc.binDir(), BinProxyClient))
	require.NoError(t, err)
	assert.Equal(t, payload, got, "the agent's gt/bd must be the orchestrator's build")

	// The worker binary is staged, not applied: the session that just came up
	// must not be restarted out from under.
	_, err = os.Stat(filepath.Join(svc.stagingDir(), BinWorkerClient+".pending"))
	assert.NoError(t, err, "gt-worker-client must be staged for the next idle moment")
}

// TestPushBinaries_FailureDoesNotBlockProvision pins the trade the design makes:
// gt_version is not the compatibility gate (proto_version is), so a worker that
// cannot be refreshed still runs sessions rather than failing a polecat start
// someone is waiting on.
func TestPushBinaries_FailureDoesNotBlockProvision(t *testing.T) {
	// A source dir whose "binary" cannot be read.
	src := t.TempDir()
	bad := filepath.Join(src, "gt-proxy-client")
	require.NoError(t, os.WriteFile(bad, []byte("x"), 0755))
	require.NoError(t, os.Chmod(bad, 0000))
	t.Cleanup(func() { _ = os.Chmod(bad, 0755) })
	defer socket.SetBinarySourceForTest(src)()

	restore := version.GTVersion
	version.GTVersion = "1.0.0-worker"
	t.Cleanup(func() { version.GTVersion = restore })

	proxyURL, ca, _ := startProxy(t)
	addr, _ := startService(t, proxyURL)
	b := newBackend(t, addr, ca)
	b.GTVersion = "9.9.9-orchestrator"

	_, err := b.Provision(context.Background(), polecatSpec())
	require.NoError(t, err, "a failed refresh must not fail the session")
}
