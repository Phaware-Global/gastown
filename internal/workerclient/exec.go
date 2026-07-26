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

	"github.com/steveyegge/gastown/internal/sockproto"
	"github.com/steveyegge/gastown/internal/worker"
)

// secretEnvSubstrings identify env keys that must never be accepted from the
// wire. The launcher already filters to a non-secret allowlist (core §7.4), but
// the worker re-checks: an env var arriving over the control plane is
// orchestrator-supplied input, and a worker's own operator-provisioned agent
// env file (§8) is the ONLY sanctioned source of agent credentials. A wire
// value must never shadow or inject one.
var secretEnvSubstrings = []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "API_KEY", "APIKEY", "_KEY", "PRIVATE"}

// looksSecret reports whether an env key must be refused from the wire.
func looksSecret(key string) bool {
	upper := strings.ToUpper(key)
	for _, frag := range secretEnvSubstrings {
		if strings.Contains(upper, frag) {
			return true
		}
	}
	return false
}

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
	// Reject wire-supplied secret env before it can reach the agent.
	for k := range m.Env {
		if looksSecret(k) {
			_ = c.send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Session: m.Session,
				Code: "bad_request",
				Msg:  fmt.Sprintf("env %q looks like a secret; agent credentials must come from the worker's agent env file (§7.1/§8), not the wire", k)})
			return
		}
	}

	s.mu.Lock()
	sess := s.sessions[m.Session]
	var worktree, container string
	var ready bool
	if sess != nil {
		worktree = sess.worktree
		ready = sess.summary.State == "ready"
		if sess.workEnv != nil {
			container = sess.workEnv.ContainerName()
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
	if err := s.streamExec(ctx, c, sess, m, worktree, container); err != nil {
		s.log.Warn("exec stream", "session", m.Session, "err", err)
	}
}

// streamExec builds and runs the agent process, piping stdio over frames.
func (s *Service) streamExec(ctx context.Context, c *connState, sess *session, m *sockproto.Message, worktree, container string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd, err := s.execCommand(ctx, m, worktree, container)
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
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = c.writeFrame(sockproto.FrameStderr, []byte("gt-worker-client: start agent: "+err.Error()+"\n"))
		_ = c.writeExit(126)
		return fmt.Errorf("start agent: %w", err)
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
	go func() {
		defer close(inboundDone)
		defer stdin.Close()
		for {
			t, payload, err := sockproto.ReadFrame(c.frameReader())
			if err != nil {
				return // EOF or protocol error: stop feeding stdin
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

	waitErr := cmd.Wait()
	pumps.Wait() // flush all output BEFORE the terminal exit frame

	// The inbound goroutine is parked in a blocking socket read, which ctx
	// cancellation cannot interrupt — expire the READ deadline to unblock it
	// (writes are untouched, so the exit frame below still goes out). Without
	// this, waiting on it would hang the stream forever after the agent exits.
	_ = c.expireReads()
	<-inboundDone
	cancel()

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
func (s *Service) execCommand(ctx context.Context, m *sockproto.Message, worktree, container string) (*exec.Cmd, error) {
	env, err := s.agentEnv(m.Env)
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
		return exec.CommandContext(ctx, "docker", args...), nil
	}

	// Native: exec directly, argv passed as argv (no shell involved at all).
	cmd := exec.CommandContext(ctx, m.Argv[0], m.Argv[1:]...)
	cmd.Dir = worktree
	cmd.Env = env
	// Own process group so a signal reaches the agent's whole tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

// agentEnv assembles the agent's environment: a minimal base, the worker's
// operator-provisioned agent env file (§8 — the ONLY sanctioned source of
// agent credentials), then the non-secret session env from the wire.
func (s *Service) agentEnv(wireEnv map[string]string) ([]string, error) {
	env := []string{}
	for _, key := range []string{"HOME", "PATH", "LANG", "TERM", "TMPDIR"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	if s.cfg.AgentEnvFile != "" {
		fileEnv, err := readEnvFile(s.cfg.AgentEnvFile)
		if err != nil {
			return nil, fmt.Errorf("agent env file: %w", err)
		}
		env = append(env, fileEnv...)
	}
	for k, v := range wireEnv {
		env = append(env, k+"="+v)
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
		_ = exec.Command("docker", "kill", "--signal", strings.ToUpper(name), "--", container).Run()
		return
	}
	if cmd.Process == nil {
		return
	}
	// Negative PID: signal the whole process group (Setpgid above).
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

// parseSignal maps the small set of signals a launcher may forward.
func parseSignal(name string) syscall.Signal {
	switch strings.ToUpper(strings.TrimPrefix(strings.ToUpper(name), "SIG")) {
	case "INT":
		return syscall.SIGINT
	case "TERM":
		return syscall.SIGTERM
	case "HUP":
		return syscall.SIGHUP
	case "QUIT":
		return syscall.SIGQUIT
	default:
		return 0
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
