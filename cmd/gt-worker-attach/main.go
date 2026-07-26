// gt-worker-attach is the blocking-pane launcher for the socket execution
// provider (docs/design/remote-polecat-execution-socket.md §5): the process the
// orchestrator's tmux pane runs in place of the agent. It opens an exec stream
// to gt-worker-client, sends the agent argv plus the NON-SECRET session env,
// pipes stdio over §4.3 frames, forwards signals, and exits with the agent's
// real remote exit code.
//
// It is deliberately thin: no agent logic, no credentials. The session env it
// forwards comes from its OWN environment — the tmux pane already has it — so
// tokens never appear in argv where `ps` would expose them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/steveyegge/gastown/internal/socket"
	"github.com/steveyegge/gastown/internal/sockproto"
)

func main() {
	var (
		address = flag.String("address", "", "worker control address: host:port or unix:///path/to.sock (required)")
		session = flag.String("session", "", "session id to attach to (required)")
		token   = flag.String("token", "", "pre-shared token for a unix worker (or GT_WORKER_TOKEN)")
		worker  = flag.String("worker-name", "", "enrolled worker name to pin for TCP mTLS (or GT_WORKER_NAME)")
	)
	flag.Parse()

	argv := flag.Args()
	if *address == "" || *session == "" || len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gt-worker-attach --address <addr> --session <id> -- <agent argv...>")
		os.Exit(2)
	}
	if *token == "" {
		*token = os.Getenv("GT_WORKER_TOKEN")
	}
	if *worker == "" {
		*worker = os.Getenv("GT_WORKER_NAME")
	}

	code, err := run(*address, *session, *token, *worker, argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gt-worker-attach: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// run dials the worker, attaches, and pumps the stream. It returns the agent's
// remote exit code.
func run(address, session, token, workerName string, argv []string) (int, error) {
	// Signals are forwarded to the remote agent rather than killing the
	// launcher: the pane's Ctrl-C must reach the agent, exactly as it would
	// locally. The stream ends on the agent's exit frame.
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigCh)

	conn, codec, err := socket.DialExecStream(context.Background(), socket.ExecStreamOptions{
		Address:    address,
		Token:      token,
		WorkerName: workerName,
	})
	if err != nil {
		return 1, err
	}
	defer conn.Close()

	if err := codec.Send(&sockproto.Message{
		Type:    sockproto.TypeAttach,
		ID:      "attach",
		Session: session,
		Argv:    argv,
		Env:     sessionEnv(),
	}); err != nil {
		return 1, fmt.Errorf("sending attach: %w", err)
	}
	ack, err := codec.Recv()
	if err != nil {
		return 1, fmt.Errorf("awaiting attach_ack: %w", err)
	}
	if ack.Type == sockproto.TypeError {
		return 1, fmt.Errorf("worker refused attach: %s: %s", ack.Code, ack.Msg)
	}
	if ack.Type != sockproto.TypeAttachAck {
		return 1, fmt.Errorf("expected attach_ack, got %q", ack.Type)
	}

	// One writer goroutine owns the outbound half so stdin and signal frames
	// never interleave mid-frame.
	var writeMu sync.Mutex
	writeFrame := func(t sockproto.FrameType, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return sockproto.WriteFrame(conn, t, payload)
	}

	go func() {
		for sig := range sigCh {
			_ = writeFrame(sockproto.FrameSignal, []byte(sig.String()))
		}
	}()

	// Local stdin → stdin frames. Not waited on: a blocked read on a tty must
	// never delay exit once the agent is gone.
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if werr := writeFrame(sockproto.FrameStdin, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Inbound frames → local stdio, until the terminal exit frame.
	for {
		t, payload, err := sockproto.ReadFrame(codec.Reader())
		if err != nil {
			if errors.Is(err, io.EOF) {
				// The stream ended without an exit frame: the agent's status is
				// unknown, so report a distinct failure rather than a silent 0.
				return 1, fmt.Errorf("exec stream closed before the agent reported an exit code")
			}
			return 1, err
		}
		switch t {
		case sockproto.FrameStdout:
			_, _ = os.Stdout.Write(payload)
		case sockproto.FrameStderr:
			_, _ = os.Stderr.Write(payload)
		case sockproto.FrameExit:
			code, err := sockproto.ExitCodeFromFrame(payload)
			if err != nil {
				return 1, err
			}
			return code, nil
		default:
			// stdin/resize/signal are launcher→worker only; ignore.
		}
	}
}

// forwardedEnvPrefixes are the session-env families the agent needs (core
// §7.4): gastown's own session vars plus the relay pointers.
var forwardedEnvPrefixes = []string{"GT_", "BD_", "CLAUDE", "ANTHROPIC_MODEL", "ANTHROPIC_BASE_URL", "ANTHROPIC_DEFAULT_"}

// launcherOnlyEnv are vars that configure THIS process and must not be
// forwarded to the agent.
var launcherOnlyEnv = map[string]bool{
	"GT_WORKER_TOKEN": true,
	"GT_WORKER_NAME":  true,
}

// secretEnvSubstrings mark keys that must never ride the wire. The worker
// re-checks this independently; agent credentials come from the worker's own
// agent env file (§8), never from here.
var secretEnvSubstrings = []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "API_KEY", "APIKEY", "_KEY", "PRIVATE"}

// sessionEnv selects the non-secret session env to forward, from this
// process's own environment (the tmux pane supplied it), so nothing sensitive
// is placed in argv.
func sessionEnv() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || launcherOnlyEnv[k] || isSecretKey(k) {
			continue
		}
		for _, p := range forwardedEnvPrefixes {
			if strings.HasPrefix(k, p) {
				out[k] = v
				break
			}
		}
	}
	return out
}

// isSecretKey reports whether a key must be withheld from the wire.
func isSecretKey(k string) bool {
	upper := strings.ToUpper(k)
	for _, frag := range secretEnvSubstrings {
		if strings.Contains(upper, frag) {
			return true
		}
	}
	return false
}
