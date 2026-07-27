package workerclient

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// drainStreamAsync reads frames on ANOTHER goroutine and delivers the result
// over a channel. Tests that need a timeout must use this rather than calling
// drainStream in a `go func`: drainStream uses require, i.e. t.FailNow, which
// the testing package documents must only be called from the test goroutine —
// from elsewhere it Goexits that goroutine, so the test's channel never closes
// and it reports a timeout with the wrong cause.
type drainResult struct {
	stdout, stderr string
	code           int
	err            error
}

func drainStreamAsync(codec *sockproto.Codec) <-chan drainResult {
	ch := make(chan drainResult, 1)
	go func() {
		var out, errOut strings.Builder
		for {
			ft, payload, err := sockproto.ReadFrame(codec.Reader())
			if err != nil {
				ch <- drainResult{out.String(), errOut.String(), 0, err}
				return
			}
			switch ft {
			case sockproto.FrameStdout:
				out.Write(payload)
			case sockproto.FrameStderr:
				errOut.Write(payload)
			case sockproto.FrameExit:
				code, err := sockproto.ExitCodeFromFrame(payload)
				ch <- drainResult{out.String(), errOut.String(), code, err}
				return
			}
		}
	}()
	return ch
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
	assert.Contains(t, resp.Msg, "not permitted from the wire")
	assert.Contains(t, resp.Msg, "agent env file")
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

// TestExecStream_LegacySignalNameForwarded pins compatibility with a launcher
// built against an older gt, which sent Go's descriptive os.Signal.String()
// form: dropping it would mean the pane's Ctrl-C never reaches the agent.
func TestExecStream_LegacySignalNameForwarded(t *testing.T) {
	addr, _ := provisionedService(t)

	conn, codec := attachStream(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", "trap 'echo got-int; exit 0' INT; while :; do sleep 0.05; done"}, nil)
	time.Sleep(400 * time.Millisecond)
	require.NoError(t, sockproto.WriteFrame(conn, sockproto.FrameSignal, []byte(syscall.SIGINT.String())))

	done := make(chan struct{})
	var out string
	var code int
	go func() {
		out, _, code = drainStream(t, codec)
		close(done)
	}()
	select {
	case <-done:
		assert.Contains(t, out, "got-int", "a descriptive signal name must still be delivered")
		assert.Equal(t, 0, code)
	case <-time.After(15 * time.Second):
		t.Fatal("legacy signal name was not delivered to the agent")
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

// TestExecStream_HalfCloseIsStdinEOFNotDisconnect pins the ambiguity in a clean
// EOF: half-closing the write side is how a launcher hands the agent stdin EOF,
// so it must NOT be treated as a lost pane and kill the agent mid-work.
func TestExecStream_HalfCloseIsStdinEOFNotDisconnect(t *testing.T) {
	addr, _ := provisionedService(t)

	// Reads stdin to EOF, then keeps working before exiting cleanly.
	conn, codec := attachStream(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", "cat; sleep 0.4; echo still-alive; exit 0"}, nil)
	require.NoError(t, sockproto.WriteFrame(conn, sockproto.FrameStdin, []byte("in\n")))
	require.NoError(t, conn.(*net.UnixConn).CloseWrite())

	out, _, code := drainStream(t, codec)
	assert.Equal(t, 0, code, "a half-close must not kill the agent")
	assert.Equal(t, "in\nstill-alive\n", out)
}

// TestExecStream_DeadLauncherCancelsSilentAgent pins the liveness prober: an
// agent that produces NO output would never hit a failing pump write, so
// without probing, a killed pane would leave it running forever — pinning the
// session (MaxSessions=1 means pinning the whole worker).
func TestExecStream_DeadLauncherCancelsSilentAgent(t *testing.T) {
	restore := execProbeInterval
	execProbeInterval = 50 * time.Millisecond
	t.Cleanup(func() { execProbeInterval = restore })

	addr, svc := provisionedService(t)

	// Silent and long-lived: it writes a marker file when SIGTERM'd so we can
	// prove the cancel was a graceful signal, not just a lost connection.
	marker := filepath.Join(t.TempDir(), "termed")
	conn, _ := attachStream(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", fmt.Sprintf("trap 'touch %s; exit 0' TERM; while :; do sleep 0.05; done", marker)}, nil)
	time.Sleep(400 * time.Millisecond) // let the trap install

	require.NoError(t, conn.Close()) // the pane died

	// The exec must unregister, freeing the session for a reattach.
	require.Eventually(t, func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		return svc.sessions["gt-demo-furiosa"].execCancel == nil
	}, 15*time.Second, 50*time.Millisecond, "a dead launcher must release the session's exec slot")

	_, err := os.Stat(marker)
	assert.NoError(t, err, "cancellation must reach the agent as a graceful SIGTERM")
}

// TestExecStream_SecondAttachRefused pins that one session cannot run two
// agents: a second launcher on the same worktree would double-write the
// checkpointed tree.
func TestExecStream_SecondAttachRefused(t *testing.T) {
	addr, _ := provisionedService(t)

	_, first := attachStream(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", "while :; do sleep 0.05; done"}, nil)
	// Wait until the first exec is registered, so the second attach races
	// nothing: the refusal must be deterministic.
	time.Sleep(400 * time.Millisecond)

	_, second := attachStream(t, addr, "gt-demo-furiosa", []string{"true"}, nil)
	_, errOut, code := drainStream(t, second)
	assert.Equal(t, 125, code, "the second attach must be refused, not silently run")
	assert.Contains(t, errOut, "already has an attached agent")

	_ = first
}

// TestExecStream_TeardownKillsAttachedAgent pins that ending a session kills
// its agent BEFORE the worktree is removed — otherwise a native agent keeps
// writing into a directory that is being deleted underneath it.
func TestExecStream_TeardownKillsAttachedAgent(t *testing.T) {
	proxyURL, ca, _ := startProxy(t)
	addr, svc := startService(t, proxyURL)
	b := newBackend(t, addr, ca)
	ep, err := b.Provision(context.Background(), polecatSpec())
	require.NoError(t, err)

	_, codec := attachStream(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", "while :; do sleep 0.05; done"}, nil)
	time.Sleep(400 * time.Millisecond)

	done := make(chan int, 1)
	go func() {
		_, _, code := drainStream(t, codec)
		done <- code
	}()

	require.NoError(t, b.Teardown(context.Background(), ep))

	select {
	case code := <-done:
		assert.NotEqual(t, 0, code, "a torn-down agent must not report success")
	case <-time.After(30 * time.Second):
		t.Fatal("teardown left the attached agent running")
	}
	svc.mu.Lock()
	_, live := svc.sessions["gt-demo-furiosa"]
	svc.mu.Unlock()
	assert.False(t, live, "teardown must drop the session")
}

func TestCanonicalSignalNamesAreAccepted(t *testing.T) {
	// Both the canonical wire form and Go's descriptive os.Signal.String() form
	// (what an older launcher sends) must map to the same signal.
	for _, tc := range []struct {
		in   string
		want syscall.Signal
	}{
		{"SIGINT", syscall.SIGINT}, {"INT", syscall.SIGINT}, {"interrupt", syscall.SIGINT},
		{"SIGTERM", syscall.SIGTERM}, {"terminated", syscall.SIGTERM},
		{"SIGHUP", syscall.SIGHUP}, {"hangup", syscall.SIGHUP},
		{"SIGQUIT", syscall.SIGQUIT}, {"quit", syscall.SIGQUIT},
		{"SIGKILL", 0}, {"", 0}, {"bogus", 0},
	} {
		assert.Equal(t, tc.want, parseSignal(tc.in), "signal %q", tc.in)
	}
	// docker kill needs a canonical name for every signal we accept.
	assert.Equal(t, "SIGINT", signalName(syscall.SIGINT))
	assert.Equal(t, "SIGQUIT", signalName(syscall.SIGQUIT))
}

// TestExecStream_ProxyURLComesFromTheWorkerRelay pins that the agent's
// control-plane URL is the worker's OWN session relay, not an orchestrator
// value: the relay is what carries this polecat's identity to the proxy, and a
// wire endpoint is refused outright.
func TestExecStream_ProxyURLComesFromTheWorkerRelay(t *testing.T) {
	addr, svc := provisionedService(t)

	_, codec := attachStream(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", "echo $GT_PROXY_URL"}, nil)
	out, _, code := drainStream(t, codec)
	require.Equal(t, 0, code)

	svc.mu.Lock()
	relayAddr := svc.sessions["gt-demo-furiosa"].relay.Addr().String()
	svc.mu.Unlock()
	assert.Equal(t, "http://"+relayAddr+"\n", out,
		"the agent must talk to the worker's session relay")
}

// TestExecStream_RefusesWireEndpoint pins the endpoint class shut: a wire
// GT_PROXY_URL would redirect the agent's gt/bd RPC, so injected responses
// would become fake mail and beads.
func TestExecStream_RefusesWireEndpoint(t *testing.T) {
	addr, _ := provisionedService(t)

	nc := rawDial(t, addr)
	codec := sockproto.NewCodec(nc)
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeAuth, Token: "t0k"}))
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeHello, ID: "h", ProtoVersion: sockproto.ProtoVersion}))
	_, err := codec.Recv()
	require.NoError(t, err)
	require.NoError(t, codec.Send(&sockproto.Message{
		Type: sockproto.TypeAttach, ID: "a", Session: "gt-demo-furiosa",
		Argv: []string{"true"},
		Env:  map[string]string{"GT_PROXY_URL": "http://attacker.example"},
	}))
	resp, err := codec.Recv()
	require.NoError(t, err)
	assert.Equal(t, sockproto.TypeError, resp.Type)
	assert.Equal(t, "bad_request", resp.Code)
}

func TestEnvAllowed_AllowlistRefusesLoaderAndSecrets(t *testing.T) {
	for _, k := range []string{"GT_ROLE", "GT_RIG", "BD_DB", "ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "CLAUDECODE"} {
		assert.True(t, sockproto.EnvAllowed(k), "%s must be allowed", k)
	}
	// Loader vars would load attacker code into a NATIVE agent running on the
	// worker host; PATH would hijack every subprocess it spawns.
	for _, k := range []string{"LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "PATH", "HOME", "ANTHROPIC_API_KEY", "GT_AUTH_TOKEN", "GITHUB_TOKEN", "SSH_PRIVATE_KEY",
		"ANTHROPIC_BASE_URL", "GT_PROXY_URL", "GT_OTEL_LOGS_URL", "GT_DOLT_HOST", "GT_WORKER_TOKEN", "GT_WORKER_NAME", ""} {
		assert.False(t, sockproto.EnvAllowed(k), "%s must be refused", k)
	}
}

// TestAgentEnv_ContainerDoesNotInheritHostPaths pins that a container exec is
// NOT handed the worker host's PATH/HOME/TMPDIR. `docker exec -e PATH=…`
// overrides the image's own, so the agent runtime that the image preflight
// verified against the IMAGE's PATH could then be unfindable — and the host's
// directories do not exist in the container anyway.
func TestAgentEnv_ContainerDoesNotInheritHostPaths(t *testing.T) {
	s := &Service{cfg: Config{}}

	native, err := s.agentEnv(nil, "", false)
	require.NoError(t, err)
	assert.True(t, hasEnvKey(native, "PATH"), "a native agent runs on the host, so it needs the host PATH")

	inContainer, err := s.agentEnv(nil, "", true)
	require.NoError(t, err)
	for _, key := range []string{"PATH", "HOME", "TMPDIR"} {
		assert.False(t, hasEnvKey(inContainer, key), "%s must come from the image, not the worker host", key)
	}
}

// TestAgentEnv_ContainerIgnoresHostPathsFromTheEnvFile pins the second door into
// the same defect: the operator's env file is appended verbatim, so a PATH line
// there would ride `docker exec -e` and override the image's PATH just as the
// host's would have.
func TestAgentEnv_ContainerIgnoresHostPathsFromTheEnvFile(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "agent.env")
	require.NoError(t, os.WriteFile(envFile, []byte(
		"ANTHROPIC_API_KEY=sk-x\nPATH=/opt/operator/bin\nHOME=/home/operator\n"), 0600))
	s := &Service{cfg: Config{AgentEnvFile: envFile}, log: slog.Default()}

	inContainer, err := s.agentEnv(nil, "", true)
	require.NoError(t, err)
	assert.Contains(t, inContainer, "ANTHROPIC_API_KEY=sk-x", "credentials are what the file is for")
	for _, key := range []string{"PATH", "HOME"} {
		assert.False(t, hasEnvKey(inContainer, key), "%s from the env file must not reach a container", key)
	}

	// On a native worker the file's values are the operator's business.
	native, err := s.agentEnv(nil, "", false)
	require.NoError(t, err)
	assert.Contains(t, native, "PATH=/opt/operator/bin")
}

func hasEnvKey(env []string, key string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}

// TestAgentEnv_FileWinsOverWire pins the §8 invariant: the operator's agent env
// file is the sanctioned source of agent configuration, so a wire value can
// never override one it sets (os/exec keeps the LAST duplicate).
func TestAgentEnv_FileWinsOverWire(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "agent.env")
	require.NoError(t, os.WriteFile(envFile, []byte("ANTHROPIC_MODEL=file-model\n"), 0600))

	s := &Service{cfg: Config{AgentEnvFile: envFile}}
	env, err := s.agentEnv(map[string]string{"ANTHROPIC_MODEL": "wire-model"}, "", false)
	require.NoError(t, err)

	var lastVal string
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_MODEL=") {
			lastVal = kv
		}
	}
	assert.Equal(t, "ANTHROPIC_MODEL=file-model", lastVal,
		"the agent env file must win over a wire value")
}

// TestAttach_RefusesCredentialEndpointRedirect pins the exfiltration path shut:
// a wire ANTHROPIC_BASE_URL would send a file-provisioned ANTHROPIC_API_KEY to
// whatever endpoint the orchestrator named. The dedup order alone does NOT
// cover it — the file wins only for keys the file also sets, and a file with
// the key but no base URL is an ordinary config — so the key must be refused at
// the wire.
func TestAttach_RefusesCredentialEndpointRedirect(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "agent.env")
	require.NoError(t, os.WriteFile(envFile, []byte("ANTHROPIC_API_KEY=sk-from-worker\n"), 0600))

	addr, _ := provisionedService(t, func(c *Config) { c.AgentEnvFile = envFile })

	nc := rawDial(t, addr)
	codec := sockproto.NewCodec(nc)
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeAuth, Token: "t0k"}))
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeHello, ID: "h", ProtoVersion: sockproto.ProtoVersion}))
	_, err := codec.Recv()
	require.NoError(t, err)
	require.NoError(t, codec.Send(&sockproto.Message{
		Type: sockproto.TypeAttach, ID: "a", Session: "gt-demo-furiosa",
		Argv: []string{"true"},
		Env:  map[string]string{"ANTHROPIC_BASE_URL": "https://attacker.example"},
	}))
	resp, err := codec.Recv()
	require.NoError(t, err)
	assert.Equal(t, sockproto.TypeError, resp.Type, "the attach must be refused outright")
	assert.Equal(t, "bad_request", resp.Code)

	// And the agent env never carries a wire base URL even if one slipped past.
	s := &Service{cfg: Config{AgentEnvFile: envFile}}
	env, err := s.agentEnv(map[string]string{"GT_ROLE": "polecat"}, "", false)
	require.NoError(t, err)
	for _, kv := range env {
		assert.NotContains(t, kv, "attacker.example")
	}
}

// TestShutdown_FencesReattachDuringFinalFlush pins that a graceful shutdown
// fences the session exactly as teardown does: no launcher may start a fresh
// agent into the worktree the supervisor is checkpointing.
func TestShutdown_FencesReattachDuringFinalFlush(t *testing.T) {
	proxyURL, ca, _ := startProxy(t)
	addr, svc := startService(t, proxyURL)
	b := newBackend(t, addr, ca)
	_, err := b.Provision(context.Background(), polecatSpec())
	require.NoError(t, err)

	// Shut the session down gracefully on its own connection.
	nc := rawDial(t, addr)
	codec := sockproto.NewCodec(nc)
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeAuth, Token: "t0k"}))
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeHello, ID: "h", ProtoVersion: sockproto.ProtoVersion}))
	_, err = codec.Recv()
	require.NoError(t, err)
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeShutdown, ID: "s",
		Session: "gt-demo-furiosa", Reason: "teardown", GraceSeconds: 60}))
	resp, err := codec.Recv()
	require.NoError(t, err)
	require.Equal(t, sockproto.TypeShutdownComplete, resp.Type, "%s: %s", resp.Code, resp.Msg)

	svc.mu.Lock()
	fenced := svc.sessions["gt-demo-furiosa"].tearingDown
	svc.mu.Unlock()
	assert.True(t, fenced, "a shut-down session must refuse further attaches")
}

// attachStreamTTY attaches asking for a terminal at a given geometry.
func attachStreamTTY(t *testing.T, addr, sessionID string, argv []string, cols, rows int) (net.Conn, *sockproto.Codec) {
	t.Helper()
	nc := rawDial(t, addr)
	codec := sockproto.NewCodec(nc)
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeAuth, Token: "t0k"}))
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeHello, ID: "h", ProtoVersion: sockproto.ProtoVersion}))
	_, err := codec.Recv()
	require.NoError(t, err)
	require.NoError(t, codec.Send(&sockproto.Message{
		Type: sockproto.TypeAttach, ID: "a", Session: sessionID, Argv: argv,
		TTY: true, Cols: cols, Rows: rows,
	}))
	ack, err := codec.Recv()
	require.NoError(t, err)
	require.Equal(t, sockproto.TypeAttachAck, ack.Type, "%s: %s", ack.Code, ack.Msg)
	return nc, codec
}

// TestExecStream_TTYGivesTheAgentATerminal is the reason this increment exists:
// an interactive agent calls setRawMode on startup and throws when stdin is not
// a TTY, so plain pipes do not degrade it — they stop it from starting.
func TestExecStream_TTYGivesTheAgentATerminal(t *testing.T) {
	addr, _ := provisionedService(t)

	_, codec := attachStreamTTY(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", "test -t 0 && test -t 1 && echo HAVE_TTY"}, 100, 30)
	out, _, code := drainStream(t, codec)
	require.Equal(t, 0, code)
	assert.Contains(t, out, "HAVE_TTY", "stdin and stdout must both be a terminal")
}

// TestExecStream_NoTTYWhenNotRequested pins that the pipe path is unchanged for
// a launcher that has no terminal of its own (a scripted attach), where separate
// stdout/stderr is worth keeping.
func TestExecStream_NoTTYWhenNotRequested(t *testing.T) {
	addr, _ := provisionedService(t)

	_, codec := attachStream(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", "test -t 0 || echo NO_TTY; echo to-err >&2"}, nil)
	out, errOut, code := drainStream(t, codec)
	require.Equal(t, 0, code)
	assert.Contains(t, out, "NO_TTY")
	assert.Contains(t, errOut, "to-err", "without a pty the two streams stay separate")
}

// TestExecStream_TTYStartsAtTheLauncherGeometry pins that the size travels with
// the attach: an agent that reads its dimensions once at startup would otherwise
// render to the 80x24 default for the life of the session.
func TestExecStream_TTYStartsAtTheLauncherGeometry(t *testing.T) {
	addr, _ := provisionedService(t)

	_, codec := attachStreamTTY(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", "stty size"}, 133, 47)
	out, _, code := drainStream(t, codec)
	require.Equal(t, 0, code)
	assert.Contains(t, strings.ReplaceAll(out, "\r", ""), "47 133",
		"the pty must be created at the launcher's geometry, not the default")
}

// TestExecStream_ResizeIsApplied pins that a resize frame reaches the terminal —
// tmux resizes panes constantly, and a TUI rendered to stale geometry is
// unusable.
func TestExecStream_ResizeIsApplied(t *testing.T) {
	addr, _ := provisionedService(t)

	conn, codec := attachStreamTTY(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", "trap 'stty size; exit 0' WINCH; while :; do sleep 0.05; done"}, 80, 24)
	time.Sleep(500 * time.Millisecond) // let the trap install

	payload, err := sockproto.MarshalResize(150, 55)
	require.NoError(t, err)
	require.NoError(t, sockproto.WriteFrame(conn, sockproto.FrameResize, payload))

	select {
	case res := <-drainStreamAsync(codec):
		require.NoError(t, res.err)
		assert.Contains(t, strings.ReplaceAll(res.stdout, "\r", ""), "55 150",
			"the agent must see the new geometry")
	case <-time.After(20 * time.Second):
		t.Fatal("the resize never reached the terminal")
	}
}

// TestExecStream_TTYSignalStillReachesTheAgent pins that dropping Setpgid for the
// pty path did not cost signal delivery: a pty child is a SESSION leader, hence
// its own process-group leader, so the negative-pid kill still reaches the tree.
func TestExecStream_TTYSignalStillReachesTheAgent(t *testing.T) {
	addr, _ := provisionedService(t)

	conn, codec := attachStreamTTY(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", "trap 'echo got-int; exit 0' INT; while :; do sleep 0.05; done"}, 80, 24)
	time.Sleep(500 * time.Millisecond) // let the trap install
	require.NoError(t, sockproto.WriteFrame(conn, sockproto.FrameSignal, []byte("SIGINT")))

	select {
	case res := <-drainStreamAsync(codec):
		require.NoError(t, res.err)
		assert.Contains(t, res.stdout, "got-int", "the pane's Ctrl-C must still reach a pty agent")
		assert.Equal(t, 0, res.code)
	case <-time.After(20 * time.Second):
		t.Fatal("signal was not delivered to the pty agent")
	}
}

// TestExecStream_TTYRejectsOutOfRangeGeometry pins the narrowing that three `> 0`
// guards did not catch: cols/rows arrive as ints and reach the ioctl as uint16,
// so 65536 passed every check and produced a ZERO-column terminal.
func TestExecStream_TTYRejectsOutOfRangeGeometry(t *testing.T) {
	addr, _ := provisionedService(t)

	t.Run("attach geometry", func(t *testing.T) {
		nc := rawDial(t, addr)
		codec := sockproto.NewCodec(nc)
		require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeAuth, Token: "t0k"}))
		require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeHello, ID: "h", ProtoVersion: sockproto.ProtoVersion}))
		_, err := codec.Recv()
		require.NoError(t, err)
		require.NoError(t, codec.Send(&sockproto.Message{
			Type: sockproto.TypeAttach, ID: "a", Session: "gt-demo-furiosa",
			Argv: []string{"sh", "-c", "stty size"}, TTY: true, Cols: 65536, Rows: 55,
		}))
		ack, err := codec.Recv()
		require.NoError(t, err)
		require.Equal(t, sockproto.TypeAttachAck, ack.Type)
		_, errOut, code := drainStream(t, codec)
		assert.Equal(t, 126, code, "a 0-column terminal must be refused, not created")
		assert.Contains(t, errOut, "outside 1..65535")
	})

	t.Run("resize frame", func(t *testing.T) {
		conn, codec := attachStreamTTY(t, addr, "gt-demo-furiosa",
			[]string{"sh", "-c", "trap 'stty size; exit 0' WINCH; i=0; while [ $i -lt 60 ]; do sleep 0.1; i=$((i+1)); done; stty size"}, 90, 30)
		time.Sleep(400 * time.Millisecond)

		bogus, err := sockproto.MarshalResize(65536, 55)
		require.NoError(t, err)
		require.NoError(t, sockproto.WriteFrame(conn, sockproto.FrameResize, bogus))

		select {
		case res := <-drainStreamAsync(codec):
			require.NoError(t, res.err)
			// The refused resize must leave the ORIGINAL geometry intact.
			assert.Contains(t, strings.ReplaceAll(res.stdout, "\r", ""), "30 90",
				"a rejected resize must not change the terminal")
		case <-time.After(30 * time.Second):
			t.Fatal("the agent never reported its geometry")
		}
	})
}

// TestExecStream_TTYSetsTERM pins that the agent gets a terminal TYPE as well as
// a terminal: a supervised worker has a stripped environment and TERM is not on
// the wire env allowlist, so without this every termcap consumer inside the
// session treats it as dumb.
func TestExecStream_TTYSetsTERM(t *testing.T) {
	addr, _ := provisionedService(t)

	nc := rawDial(t, addr)
	codec := sockproto.NewCodec(nc)
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeAuth, Token: "t0k"}))
	require.NoError(t, codec.Send(&sockproto.Message{Type: sockproto.TypeHello, ID: "h", ProtoVersion: sockproto.ProtoVersion}))
	_, err := codec.Recv()
	require.NoError(t, err)
	require.NoError(t, codec.Send(&sockproto.Message{
		Type: sockproto.TypeAttach, ID: "a", Session: "gt-demo-furiosa",
		Argv: []string{"sh", "-c", "echo TERM=$TERM"}, TTY: true, Cols: 80, Rows: 24,
		Term: "screen-256color",
	}))
	ack, err := codec.Recv()
	require.NoError(t, err)
	require.Equal(t, sockproto.TypeAttachAck, ack.Type)

	out, _, code := drainStream(t, codec)
	require.Equal(t, 0, code)
	assert.Contains(t, out, "TERM=screen-256color", "the launcher's TERM must reach the agent")
}

// TestTTYTerm pins the fallback and the charset: TERM becomes an env var in the
// agent's environment, so a wire value is validated rather than trusted.
func TestTTYTerm(t *testing.T) {
	assert.Equal(t, "screen-256color", ttyTerm("screen-256color"))
	assert.Equal(t, "xterm-256color", ttyTerm(""), "a launcher that sends none still gets a usable terminal")
	for _, bogus := range []string{"x;rm -rf /", "a b", "x\nY", strings.Repeat("x", 65)} {
		assert.Equal(t, "xterm-256color", ttyTerm(bogus), "%q must not reach the agent's env", bogus)
	}
}

// TestExecStream_TTYHalfCloseDoesNotHangUpTheTerminal pins the invariant a pty
// broke: closing stdin is the right answer to a half-close on a PIPE, but on a
// pty stdin and stdout are the same master, so closing it SIGHUPs the agent and
// truncates its output.
func TestExecStream_TTYHalfCloseDoesNotHangUpTheTerminal(t *testing.T) {
	addr, _ := provisionedService(t)

	conn, codec := attachStreamTTY(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", "sleep 1; echo STILL_ALIVE; exit 7"}, 80, 24)
	// Half-close immediately: with the old behavior the agent took SIGHUP here.
	require.NoError(t, conn.(*net.UnixConn).CloseWrite())

	select {
	case res := <-drainStreamAsync(codec):
		require.NoError(t, res.err)
		assert.Contains(t, res.stdout, "STILL_ALIVE", "a half-close must not hang up the terminal")
		assert.Equal(t, 7, res.code, "the agent's own exit code must survive")
	case <-time.After(30 * time.Second):
		t.Fatal("the session never completed after a half-close")
	}
}

// TestExecStream_TTYExitsWhenADescendantHoldsTheSlave is the regression test for
// the wedge: a pty master is nobody's pipe, so cmd.Wait closes nothing, and a
// descendant outliving the agent kept the output pump readable forever — no exit
// frame, and at MaxSessions=1 the whole worker stuck until restart.
func TestExecStream_TTYExitsWhenADescendantHoldsTheSlave(t *testing.T) {
	prev := ptyDrainGrace
	ptyDrainGrace = 500 * time.Millisecond
	t.Cleanup(func() { ptyDrainGrace = prev })

	addr, svc := provisionedService(t)

	// The agent exits immediately; a grandchild keeps the slave open for a minute.
	_, codec := attachStreamTTY(t, addr, "gt-demo-furiosa",
		[]string{"sh", "-c", "sleep 60 & echo AGENT_DONE; exit 3"}, 80, 24)

	select {
	case res := <-drainStreamAsync(codec):
		require.NoError(t, res.err)
		assert.Equal(t, 3, res.code, "the exit frame must arrive despite the lingering descendant")
		assert.Contains(t, res.stdout, "AGENT_DONE", "output before the exit must still be flushed")
	case <-time.After(30 * time.Second):
		t.Fatal("the session wedged: no exit frame while a descendant held the pty slave")
	}

	// And the session must be attachable again rather than pinned.
	require.Eventually(t, func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		return svc.sessions["gt-demo-furiosa"].execCancel == nil
	}, 10*time.Second, 50*time.Millisecond, "the exec slot must be released")
}

// TestExecCommand_ContainerTTYDisablesDetachKeys pins that a container exec turns
// OFF docker's ctrl-p/ctrl-q sequence, which -t silently enables. The launcher
// forwards raw bytes, so an operator pressing it would make the docker client
// exit 0 while the in-container agent kept running — an orphan that the
// one-agent-per-session fence would then happily let a second launcher double.
func TestExecCommand_ContainerTTYDisablesDetachKeys(t *testing.T) {
	s := &Service{cfg: Config{}, log: slog.Default()}

	cmd, err := s.execCommand(context.Background(),
		&sockproto.Message{Argv: []string{"claude"}, TTY: true, Cols: 80, Rows: 24},
		"/work", "gt-work-demo-furiosa", "")
	require.NoError(t, err)

	joined := strings.Join(cmd.Args, " ")
	assert.Contains(t, joined, "-t", "the agent needs a terminal inside the container")
	assert.Contains(t, joined, "--detach-keys=", "detach must be disabled, or a keystroke orphans the agent")

	// Without a tty there is no detach sequence to disable, and no -t.
	noTTY, err := s.execCommand(context.Background(),
		&sockproto.Message{Argv: []string{"claude"}}, "/work", "gt-work-demo-furiosa", "")
	require.NoError(t, err)
	assert.NotContains(t, strings.Join(noTTY.Args, " "), "--detach-keys")
}
