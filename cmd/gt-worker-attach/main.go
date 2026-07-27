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

	"golang.org/x/term"

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

	// A terminal is what an interactive agent needs to start: Claude Code's UI
	// calls setRawMode on stdin and throws without a TTY. So when this pane HAS
	// a terminal, ask the worker for one and hand over the geometry with the
	// attach — an agent that reads its size once at startup must not see 80x24.
	tty := term.IsTerminal(int(os.Stdin.Fd()))
	cols, rows := 0, 0
	if tty {
		if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
			cols, rows = w, h
		}
	}
	if err := codec.Send(&sockproto.Message{
		Type:    sockproto.TypeAttach,
		ID:      "attach",
		Session: session,
		Argv:    argv,
		Env:     sessionEnv(),
		TTY:     tty,
		Cols:    cols,
		Rows:    rows,
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

	// Raw mode: with a remote terminal, every keystroke belongs to the agent —
	// the local line discipline must not buffer lines, echo them, or turn ^C
	// into a local signal. Restored on every exit path, including a panic, or
	// the operator is left with an unusable shell.
	if tty {
		restore, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return 1, fmt.Errorf("putting the terminal in raw mode: %w", err)
		}
		defer term.Restore(int(os.Stdin.Fd()), restore)
	}

	// One writer goroutine owns the outbound half so stdin and signal frames
	// never interleave mid-frame.
	var writeMu sync.Mutex
	writeFrame := func(t sockproto.FrameType, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return sockproto.WriteFrame(conn, t, payload)
	}

	// Window changes follow the pane: a TUI rendered to stale geometry is
	// unusable, and tmux resizes panes constantly.
	if tty {
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				w, h, err := term.GetSize(int(os.Stdin.Fd()))
				if err != nil {
					continue
				}
				payload, err := sockproto.MarshalResize(w, h)
				if err != nil {
					continue
				}
				_ = writeFrame(sockproto.FrameResize, payload)
			}
		}()
	}

	go func() {
		for sig := range sigCh {
			// CANONICAL names on the wire. os.Signal.String() yields
			// descriptive forms ("interrupt", "terminated"), which an older
			// worker would not recognize — and a silently dropped SIGINT means
			// the pane's Ctrl-C never reaches the agent.
			_ = writeFrame(sockproto.FrameSignal, []byte(canonicalSignalName(sig)))
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

// canonicalSignalName maps a caught signal to the canonical wire name the
// worker parses.
func canonicalSignalName(sig os.Signal) string {
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
		return strings.ToUpper(sig.String())
	}
}

// sessionEnv selects the session env to forward, from this process's own
// environment (the tmux pane supplied it), so nothing sensitive is placed in
// argv. The filter is the SHARED wire policy the worker re-validates against
// (sockproto.EnvAllowed), so the two sides cannot drift.
func sessionEnv() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if ok && sockproto.EnvAllowed(k) {
			out[k] = v
		}
	}
	return out
}
