package workerclient

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/execution"
	"github.com/steveyegge/gastown/internal/socket"
	"github.com/steveyegge/gastown/internal/sockproto"
)

// attachStream provisions a session, then opens an exec stream on a fresh
// connection and returns it ready for frames (post-ack).
func attachStream(t *testing.T, addr string, sessionID string, argv []string, env map[string]string) (net.Conn, *sockproto.Codec) {
	t.Helper()
	nc := rawDial(t, addr)
	codec := sockproto.NewCodec(nc)
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeAuth, Token: "t0k"}))
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeHello, ID: "h", ProtoVersion: sockproto.ProtoVersion}))
	_, err := codec.Recv() // hello_ack
	require.NoError(t, err)

	require.NoError(t, codec.Send(&sockproto.Message{
		Type: sockproto.TypeAttach, ID: "a", Session: sessionID, Argv: argv, Env: env,
	}))
	ack, err := codec.Recv()
	require.NoError(t, err)
	require.Equal(t, sockproto.TypeAttachAck, ack.Type, "attach must be acked (got %s: %s)", ack.Type, ack.Msg)
	return nc, codec
}

// drainStream reads frames until the exit frame, returning stdout, stderr, and
// the exit code.
func drainStream(t *testing.T, codec *sockproto.Codec) (string, string, int) {
	t.Helper()
	var out, errOut strings.Builder
	for {
		ft, payload, err := sockproto.ReadFrame(codec.Reader())
		require.NoError(t, err, "stream must end with an exit frame, not an error")
		switch ft {
		case sockproto.FrameStdout:
			out.Write(payload)
		case sockproto.FrameStderr:
			errOut.Write(payload)
		case sockproto.FrameExit:
			code, err := sockproto.ExitCodeFromFrame(payload)
			require.NoError(t, err)
			return out.String(), errOut.String(), code
		}
	}
}

// provisionedService starts a proxy + service and provisions one native
// session, returning the worker address and the service.
func provisionedService(t *testing.T, cfgMut ...func(*Config)) (string, *Service) {
	t.Helper()
	proxyURL, ca, _ := startProxy(t)
	addr, svc := startService(t, proxyURL, cfgMut...)
	b := newBackend(t, addr, ca)
	_, err := b.Provision(context.Background(), polecatSpec())
	require.NoError(t, err)
	return addr, svc
}

// TestExecStream_NativeRoundTrip runs a REAL command on the worker through the
// §4.3 stream: stdout/stderr are framed back and the real exit code arrives.
func TestExecStream_NativeRoundTrip(t *testing.T) {
	addr, _ := provisionedService(t)

	_, codec := attachStream(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", "echo to-stdout; echo to-stderr >&2; exit 7"}, nil)
	out, errOut, code := drainStream(t, codec)

	assert.Equal(t, "to-stdout\n", out)
	assert.Equal(t, "to-stderr\n", errOut)
	assert.Equal(t, 7, code, "the real remote exit code must cross the stream")
}

func TestExecStream_StdinIsPiped(t *testing.T) {
	addr, _ := provisionedService(t)

	conn, codec := attachStream(t, addr, "gt-demo-furiosa", []string{"cat"}, nil)
	require.NoError(t, sockproto.WriteFrame(conn, sockproto.FrameStdin, []byte("hello from the pane\n")))
	// Half-close stdin by ending the frame stream: cat sees EOF and exits.
	require.NoError(t, conn.(*net.UnixConn).CloseWrite())

	out, _, code := drainStream(t, codec)
	assert.Equal(t, "hello from the pane\n", out)
	assert.Equal(t, 0, code)
}

func TestExecStream_RunsInWorktreeWithSessionEnv(t *testing.T) {
	addr, svc := provisionedService(t)

	_, codec := attachStream(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", "pwd; echo $GT_ROLE"},
		map[string]string{"GT_ROLE": "polecat"})
	out, _, code := drainStream(t, codec)
	require.Equal(t, 0, code)

	worktree := filepath.Join(svc.cfg.StateDir, "worktrees", "demo", "furiosa")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 2)
	// macOS /var is a symlink to /private/var; compare resolved paths.
	wantWT, err := filepath.EvalSymlinks(worktree)
	require.NoError(t, err)
	gotWT, err := filepath.EvalSymlinks(lines[0])
	require.NoError(t, err)
	assert.Equal(t, wantWT, gotWT, "the agent must run in the session worktree")
	assert.Equal(t, "polecat", lines[1], "non-secret session env must reach the agent")
}

func TestExecStream_RefusesSecretEnvFromWire(t *testing.T) {
	addr, _ := provisionedService(t)

	// The worker must refuse orchestrator-supplied secrets: agent credentials
	// come only from its own operator-provisioned env file (§8).
	nc := rawDial(t, addr)
	codec := sockproto.NewCodec(nc)
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeAuth, Token: "t0k"}))
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeHello, ID: "h", ProtoVersion: sockproto.ProtoVersion}))
	_, err := codec.Recv()
	require.NoError(t, err)
	require.NoError(t, codec.Send(&sockproto.Message{
		Type: sockproto.TypeAttach, ID: "a", Session: "gt-demo-furiosa",
		Argv: []string{"true"}, Env: map[string]string{"ANTHROPIC_API_KEY": "sk-injected"},
	}))
	resp, err := codec.Recv()
	require.NoError(t, err)
	assert.Equal(t, sockproto.TypeError, resp.Type)
	assert.Equal(t, "bad_request", resp.Code)
	assert.Contains(t, resp.Msg, "secret")
}

func TestExecStream_AgentEnvFileSuppliesCredentials(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "agent.env")
	require.NoError(t, os.WriteFile(envFile,
		[]byte("# operator-provisioned\nANTHROPIC_API_KEY=sk-from-worker\n\nEXTRA=1\n"), 0600))

	addr, _ := provisionedService(t, func(c *Config) { c.AgentEnvFile = envFile })

	_, codec := attachStream(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", "echo $ANTHROPIC_API_KEY:$EXTRA"}, nil)
	out, _, code := drainStream(t, codec)
	require.Equal(t, 0, code)
	assert.Equal(t, "sk-from-worker:1\n", out,
		"credentials must come from the worker's agent env file")
}

func TestExecStream_AttachRequiresReadySessionAndArgv(t *testing.T) {
	addr, _ := provisionedService(t)

	send := func(m *sockproto.Message) *sockproto.Message {
		t.Helper()
		nc := rawDial(t, addr)
		codec := sockproto.NewCodec(nc)
		require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeAuth, Token: "t0k"}))
		require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeHello, ID: "h", ProtoVersion: sockproto.ProtoVersion}))
		_, err := codec.Recv()
		require.NoError(t, err)
		require.NoError(t, codec.Send(m))
		resp, err := codec.Recv()
		require.NoError(t, err)
		return resp
	}

	t.Run("unknown session", func(t *testing.T) {
		resp := send(&sockproto.Message{Type: sockproto.TypeAttach, ID: "a",
			Session: "gt-demo-nobody", Argv: []string{"true"}})
		assert.Equal(t, sockproto.TypeError, resp.Type)
		assert.Equal(t, "no_session", resp.Code)
	})

	t.Run("empty argv", func(t *testing.T) {
		resp := send(&sockproto.Message{Type: sockproto.TypeAttach, ID: "a", Session: "gt-demo-furiosa"})
		assert.Equal(t, sockproto.TypeError, resp.Type)
		assert.Equal(t, "bad_request", resp.Code)
	})
}

func TestExecStream_UnstartableCommandReportsExit(t *testing.T) {
	// A command that cannot start must still produce an exit frame — otherwise
	// the launcher (and the tmux pane) would hang forever on a dead stream.
	addr, _ := provisionedService(t)

	_, codec := attachStream(t, addr, "gt-demo-furiosa",
		[]string{"/no/such/binary/xyzzy"}, nil)
	_, errOut, code := drainStream(t, codec)
	assert.Equal(t, 126, code)
	assert.Contains(t, errOut, "gt-worker-client")
}

func TestExecStream_SignalForwardedToAgent(t *testing.T) {
	addr, _ := provisionedService(t)

	// A shell that reports the signal it received, so we observe delivery
	// rather than just the process dying.
	conn, codec := attachStream(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", "trap 'echo got-int; exit 0' INT; while :; do sleep 0.05; done"}, nil)
	time.Sleep(400 * time.Millisecond) // let the trap install
	require.NoError(t, sockproto.WriteFrame(conn, sockproto.FrameSignal, []byte("SIGINT")))

	done := make(chan struct{})
	var out string
	var code int
	go func() {
		out, _, code = drainStream(t, codec)
		close(done)
	}()
	select {
	case <-done:
		assert.Contains(t, out, "got-int", "the signal must reach the agent")
		assert.Equal(t, 0, code)
	case <-time.After(15 * time.Second):
		t.Fatal("signal was not delivered to the agent")
	}
}

func TestExecStream_LargeOutputIsChunkedIntact(t *testing.T) {
	addr, _ := provisionedService(t)

	// More than one frame's worth, to exercise chunked pumping.
	const lines = 5000
	_, codec := attachStream(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", fmt.Sprintf("i=0; while [ $i -lt %d ]; do echo line-$i; i=$((i+1)); done", lines)}, nil)
	out, _, code := drainStream(t, codec)
	require.Equal(t, 0, code)

	got := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, got, lines, "no output may be dropped across frames")
	assert.Equal(t, "line-0", got[0])
	assert.Equal(t, fmt.Sprintf("line-%d", lines-1), got[lines-1])
}

func TestExecStream_OutputFlushedBeforeExitFrame(t *testing.T) {
	// The exit frame is terminal, so all output must precede it — a race here
	// would silently truncate the agent's last words.
	addr, _ := provisionedService(t)
	for i := 0; i < 5; i++ {
		_, codec := attachStream(t, addr, "gt-demo-furiosa",
			[]string{"sh", "-c", "echo final-line; exit 3"}, nil)
		out, _, code := drainStream(t, codec)
		require.Equal(t, 3, code, "iteration %d", i)
		assert.Equal(t, "final-line\n", out, "iteration %d: output must precede the exit frame", i)
	}
}

// TestWrapCommandEnv_KeepsCredentialOffArgv pins that the launcher's worker
// credential travels in the session env, never in a world-readable argv.
func TestWrapCommandEnv_KeepsCredentialOffArgv(t *testing.T) {
	proxyURL, ca, _ := startProxy(t)
	addr, _ := startService(t, proxyURL)
	b := newBackend(t, addr, ca)

	env := map[string]string{"GT_ROLE": "polecat"}
	argv, err := b.WrapCommand(execution.Endpoint{
		Backend: socket.BackendName, ID: "gt-demo-furiosa", Address: addr,
	}, []string{"claude", "--model", "opus"}, env)
	require.NoError(t, err)

	joined := strings.Join(argv, " ")
	assert.NotContains(t, joined, "t0k", "the worker token must not appear in argv")
	assert.Equal(t, "t0k", env["GT_WORKER_TOKEN"], "it must be delivered via the session env")
	assert.Contains(t, joined, "gt-worker-attach")
	assert.Contains(t, joined, "-- claude --model opus")
}

var _ = io.Discard
