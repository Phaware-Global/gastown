package workerclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/sockproto"
	"github.com/steveyegge/gastown/internal/worker"
)

// execWaitDelay bounds the interval between canceling an exec and forcibly
// killing it: long enough for an agent to handle SIGTERM and flush, short
// enough that a wedged agent cannot pin the session (MaxSessions=1 means one
// wedged session is the whole worker).
const execWaitDelay = 10 * time.Second

// execProbeInterval is how often an attached stream probes the launcher's
// liveness. A var so tests can drive the dead-peer path without waiting.
var execProbeInterval = 30 * time.Second

// handleAttach services an exec-stream connection (§4.3, §5): validate the
// attach preamble against a ready session, ack it, then switch this connection
// to binary frames — piping the agent's stdio and returning its real exit code.
//
// The caller must NOT keep using the control codec afterward: this connection
// is an exec stream from the ack onward.
func (s *Service) handleAttach(ctx context.Context, c *connState, m *sockproto.Message) {
	if len(m.Argv) == 0 {
		_ = c.send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Session: m.Session,
			Code: "bad_request", Msg: "attach requires argv"})
		return
	}
	// Re-validate wire env against the shared allowlist before it can reach the
	// agent. The launcher already filtered, but this side must not TRUST that:
	// the env is orchestrator-supplied input, and in native mode the agent runs
	// on the worker host where a wire LD_PRELOAD or PATH would be code
	// execution (sockproto.EnvAllowed documents the reasoning).
	for k := range m.Env {
		if !sockproto.EnvAllowed(k) {
			_ = c.send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Session: m.Session,
				Code: "bad_request",
				Msg: fmt.Sprintf("env %q is not permitted from the wire (allowed: %s); agent credentials come from the worker's agent env file (§7.1/§8)",
					k, sockproto.EnvAllowedDescription())})
			return
		}
	}

	s.mu.Lock()
	sess := s.sessions[m.Session]
	var worktree, container, proxyURL string
	var ready bool
	if sess != nil {
		worktree = sess.worktree
		ready = sess.summary.State == "ready"
		if sess.workEnv != nil {
			container = sess.workEnv.ContainerName()
		}
		// The agent's control-plane URL is the worker's OWN session relay — the
		// only endpoint that carries this polecat's identity to the proxy. The
		// worker sets it; the orchestrator cannot (its own value would name a
		// host the worker may not even be able to reach). In container mode the
		// container already has it from creation (WorkEnv), and docker exec
		// inherits it.
		if sess.relay != nil && container == "" {
			if addr := sess.relay.Addr(); addr != nil {
				proxyURL = "http://" + addr.String()
			}
		}
	}
	s.mu.Unlock()

	if sess == nil || !ready {
		_ = c.send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Session: m.Session,
			Code: "no_session", Msg: "no ready session to attach to"})
		return
	}

	if err := c.send(&sockproto.Message{Type: sockproto.TypeAttachAck, ID: m.ID, Session: m.Session}); err != nil {
		return
	}

	// From here the connection is a frame stream. Read frames from the codec's
	// BUFFERED reader: bytes past the preamble line may already be buffered,
	// and reading the raw conn would drop them.
	if err := s.streamExec(ctx, c, sess, m, worktree, container, proxyURL); err != nil {
		s.log.Warn("exec stream", "session", m.Session, "err", err)
	}
}

// streamExec builds and runs the agent process, piping stdio over frames.
func (s *Service) streamExec(ctx context.Context, c *connState, sess *session, m *sockproto.Message, worktree, container, proxyURL string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Register this exec with the session so a concurrent teardown/shutdown on
	// ANOTHER connection kills the agent instead of deleting its worktree from
	// under a still-running process (§6). Registration also serializes attaches:
	// a second launcher must not start a SECOND agent on the same worktree, and
	// an attach racing a teardown must not start one at all.
	s.mu.Lock()
	switch {
	case sess.tearingDown:
		s.mu.Unlock()
		_ = c.writeExit(125)
		return fmt.Errorf("session %s is being torn down", m.Session)
	case sess.execCancel != nil:
		s.mu.Unlock()
		_ = c.writeFrame(sockproto.FrameStderr, []byte("gt-worker-client: session already has an attached agent\n"))
		_ = c.writeExit(125)
		return fmt.Errorf("session %s already has an attached exec", m.Session)
	}
	sess.execCancel = cancel
	s.mu.Unlock()
	var unregisterOnce sync.Once
	unregister := func() {
		unregisterOnce.Do(func() {
			s.mu.Lock()
			sess.execCancel = nil
			s.mu.Unlock()
		})
	}
	defer unregister()

	cmd, err := s.execCommand(ctx, m, worktree, container, proxyURL)
	if err != nil {
		// Report the failure as a non-zero exit so the launcher (and the pane)
		// terminate rather than hanging on a stream that never produces frames.
		_ = c.writeFrame(sockproto.FrameStderr, []byte("gt-worker-client: "+err.Error()+"\n"))
		_ = c.writeExit(126)
		return err
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	// NOT cmd.StdoutPipe/StderrPipe: those are closed by cmd.Wait the moment the
	// process exits, and os/exec documents it as incorrect to call Wait before
	// the reads finish. Doing so drops whatever is still sitting in the pipe —
	// CI caught exactly that, truncating a 5000-line burst at ~2000. Owning the
	// pipes ourselves decouples reaping from draining: Wait touches neither, and
	// each read end reports EOF once the child's write end is closed.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	defer func() { _ = stdoutR.Close() }()
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutW.Close()
		return fmt.Errorf("stderr pipe: %w", err)
	}
	defer func() { _ = stderrR.Close() }()
	cmd.Stdout, cmd.Stderr = stdoutW, stderrW
	var stdout io.Reader = stdoutR
	var stderr io.Reader = stderrR
	startErr := cmd.Start()
	// The child holds its own dup of each write end now, so drop ours: the read
	// ends must see EOF when the CHILD exits, not when this function returns.
	_ = stdoutW.Close()
	_ = stderrW.Close()
	if startErr != nil {
		_ = c.writeFrame(sockproto.FrameStderr, []byte("gt-worker-client: start agent: "+startErr.Error()+"\n"))
		_ = c.writeExit(126)
		return fmt.Errorf("start agent: %w", startErr)
	}
	s.log.Info("agent started", "session", m.Session, "pid", cmd.Process.Pid, "container", container)

	// Pump the agent's output into frames. Each pump writes through
	// connState's serialized writer, so frames never interleave.
	var pumps sync.WaitGroup
	pump := func(r io.Reader, t sockproto.FrameType) {
		defer pumps.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if werr := c.writeFrame(t, buf[:n]); werr != nil {
					// The launcher stopped draining (or died): cancel so the
					// agent is killed and cmd.Wait can return, instead of the
					// agent blocking forever on a full stdout pipe.
					s.log.Warn("output frame write failed; canceling the attached agent",
						"session", m.Session, "err", werr)
					cancel()
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	pumps.Add(2)
	go pump(stdout, sockproto.FrameStdout)
	go pump(stderr, sockproto.FrameStderr)

	// Read inbound frames: stdin, signals, resize. Ends when the launcher
	// closes the stream (agent stdin then sees EOF).
	inboundDone := make(chan struct{})
	// agentDone marks the agent as already exited, so the teardown-time read
	// error below is expected rather than a lost launcher.
	agentDone := make(chan struct{})
	go func() {
		defer close(inboundDone)
		defer func() { _ = stdin.Close() }()
		for {
			t, payload, err := sockproto.ReadFrame(c.frameReader())
			if err != nil {
				// A clean io.EOF is AMBIGUOUS: a launcher legitimately
				// half-closes its write side to hand the agent stdin EOF (that
				// is how `cat` terminates), so EOF alone must not kill the
				// agent — closing stdin (deferred above) is the whole response,
				// and a truly dead launcher is caught by the liveness prober.
				// A hard error is unambiguous: the connection broke, the pane is
				// gone, so cancel rather than leave an orphaned agent pinning
				// the session with no reattach path.
				if !errors.Is(err, io.EOF) {
					select {
					case <-agentDone:
					default:
						s.log.Warn("exec stream read failed; canceling the attached agent",
							"session", m.Session, "err", err)
						cancel()
					}
				}
				return
			}
			switch t {
			case sockproto.FrameStdin:
				if _, werr := stdin.Write(payload); werr != nil {
					return
				}
			case sockproto.FrameSignal:
				s.signalAgent(cmd, string(payload), container)
			case sockproto.FrameResize:
				// No PTY in this increment; resize is accepted and ignored so a
				// launcher can send it unconditionally.
			default:
				// stdout/stderr/exit are worker→launcher only; ignore.
			}
		}
	}()

	// Liveness prober: an EMPTY stdout frame is a no-op for the launcher (it
	// writes zero bytes) but a real write on the socket, so it detects a dead
	// peer even for an agent that produces no output — the case no pump write
	// would ever catch. Without it, a killed pane would leave the agent running
	// forever, pinning the session.
	proberDone := make(chan struct{})
	go func() {
		defer close(proberDone)
		tick := time.NewTicker(execProbeInterval)
		defer tick.Stop()
		for {
			select {
			case <-agentDone:
				return
			case <-tick.C:
				if err := c.writeFrame(sockproto.FrameStdout, nil); err != nil {
					s.log.Warn("exec stream liveness probe failed; canceling the attached agent",
						"session", m.Session, "err", err)
					cancel()
					return
				}
			}
		}
	}()

	waitErr := cmd.Wait()
	close(agentDone) // stop the prober; later read errors are expected
	<-proberDone     // no prober write may race the terminal exit frame
	pumps.Wait()     // flush all output BEFORE the terminal exit frame

	// The inbound goroutine is parked in a blocking socket read, which ctx
	// cancellation cannot interrupt — expire the READ deadline to unblock it
	// (writes are untouched, so the exit frame below still goes out). Without
	// this, waiting on it would hang the stream forever after the agent exits.
	_ = c.expireReads()
	<-inboundDone
	cancel()

	// Unregister BEFORE the terminal exit frame: a launcher that sees the exit
	// frame may immediately reattach (the pane restarts the agent), and it must
	// not be refused as "already attached" by this finished exec's registration.
	// The agent is already dead, so there is nothing left for a teardown to kill.
	unregister()

	code := exitCode(waitErr)
	if err := c.writeExit(code); err != nil {
		return fmt.Errorf("write exit frame: %w", err)
	}
	s.log.Info("agent exited", "session", m.Session, "code", code)
	return nil
}

// execCommand builds the agent command for the session's exec mode (§5):
// `docker exec` into the prepared work container, or a direct exec on the
// worker for native mode.
func (s *Service) execCommand(ctx context.Context, m *sockproto.Message, worktree, container, proxyURL string) (*exec.Cmd, error) {
	env, err := s.agentEnv(m.Env, proxyURL)
	if err != nil {
		return nil, err
	}

	if container != "" {
		args := []string{"exec", "-i", "-w", "/work"}
		for _, kv := range env {
			args = append(args, "-e", kv)
		}
		// The exec channel is a string interface (§6.2): the argv is rendered
		// as a single shell-quoted command line and run via /bin/sh, which the
		// image contract requires.
		args = append(args, "--", container, "/bin/sh", "-c", shellJoin(m.Argv))
		cmd := exec.CommandContext(ctx, "docker", args...)
		// Canceling kills the `docker exec` CLIENT; the in-container process is
		// reaped indirectly (its stdio pipes close, so its next write takes
		// SIGPIPE). The authoritative kill for container mode is teardown, which
		// removes the work container outright (§6).
		cmd.WaitDelay = execWaitDelay
		return cmd, nil
	}

	// Native: exec directly, argv passed as argv (no shell involved at all).
	cmd := exec.CommandContext(ctx, m.Argv[0], m.Argv[1:]...)
	cmd.Dir = worktree
	cmd.Env = env
	// Own process group so a signal reaches the agent's whole tree.
	setProcessGroup(cmd)
	// Cancellation is a graceful SIGTERM to the whole group — an agent killed
	// with SIGKILL cannot flush its own state — with WaitDelay as the hard
	// bound: after it, Go SIGKILLs the process and closes the pipes, so
	// cmd.Wait and the output pumps always unwind even if the agent ignores
	// SIGTERM.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return signalProcessGroup(cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = execWaitDelay
	return cmd, nil
}

// agentEnv assembles the agent's environment: a minimal base, the worker's
// operator-provisioned agent env file (§8 — the ONLY sanctioned source of
// agent credentials), then the non-secret session env from the wire.
func (s *Service) agentEnv(wireEnv map[string]string, proxyURL string) ([]string, error) {
	env := []string{}
	for _, key := range []string{"HOME", "PATH", "LANG", "TERM", "TMPDIR"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	// Endpoints are worker-local: the wire cannot carry them
	// (sockproto.EnvEndpointKey), so the worker supplies the agent's
	// control-plane URL from its own session relay.
	if proxyURL != "" {
		env = append(env, "GT_PROXY_URL="+proxyURL)
	}
	// Wire env BEFORE the operator's file: os/exec dedups keeping the LAST
	// value, so the file wins for every key it sets. Note the limit of that
	// guarantee — it holds only for keys the file actually sets, which is why
	// anything that could redirect a credential (ANTHROPIC_BASE_URL) is barred
	// from the wire outright in sockproto.EnvAllowed rather than relying on the
	// file to shadow it.
	for k, v := range wireEnv {
		env = append(env, k+"="+v)
	}
	if s.cfg.AgentEnvFile != "" {
		fileEnv, err := readEnvFile(s.cfg.AgentEnvFile)
		if err != nil {
			return nil, fmt.Errorf("agent env file: %w", err)
		}
		env = append(env, fileEnv...)
	}
	return env, nil
}

// signalAgent forwards a named signal to the agent (native) or the container's
// main process (container mode).
func (s *Service) signalAgent(cmd *exec.Cmd, name, container string) {
	sig := parseSignal(name)
	if sig == 0 {
		return
	}
	if container != "" {
		// docker kill needs a canonical name; the wire form may be descriptive.
		_ = exec.Command("docker", "kill", "--signal", signalName(sig), "--", container).Run()
		return
	}
	if cmd.Process == nil {
		return
	}
	// Reaches the agent's whole process group (see setProcessGroup).
	_ = signalProcessGroup(cmd.Process.Pid, sig)
}

// parseSignal maps the signals a launcher may forward. It accepts BOTH the
// canonical names ("SIGINT"/"INT") and Go's descriptive os.Signal.String()
// forms ("interrupt", "terminated", "hangup", "quit") — a launcher built
// against an older gt may still send the latter, and silently dropping them
// would mean the pane's Ctrl-C never reaches the agent.
func parseSignal(name string) syscall.Signal {
	switch strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(name)), "SIG") {
	case "INT", "INTERRUPT":
		return syscall.SIGINT
	case "TERM", "TERMINATED":
		return syscall.SIGTERM
	case "HUP", "HANGUP":
		return syscall.SIGHUP
	case "QUIT":
		return syscall.SIGQUIT
	default:
		return 0
	}
}

// signalName renders a signal for docker kill, which needs a canonical name.
func signalName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	default:
		return ""
	}
}

// exitCode extracts a process exit status; a signal death has no exit status,
// so it is reported as -1 and encoded as 255 on the wire.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ee.Exited() {
			return ee.ExitCode()
		}
		return -1
	}
	return 1
}

// shellJoin renders argv as a single shell command line, quoting every token —
// the same discipline BuildStartupCommand uses (core §6.1.2): config-derived
// parts (model flags, custom agent args, a free-form initial prompt) are
// untrusted DATA to be quoted, never interpolated raw.
func shellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, tok := range argv {
		quoted[i] = "'" + strings.ReplaceAll(tok, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// readEnvFile parses a KEY=VALUE env file (blank lines and #-comments
// skipped). Values are taken verbatim after the first '='.
func readEnvFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "=") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

var _ = worker.CheckpointRefForPolecat // keep the worker import meaningful across builds
