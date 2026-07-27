package workerclient

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/steveyegge/gastown/internal/sockproto"
)

// PTY support for the exec stream (§4.3).
//
// Interactive agents REQUIRE a terminal, so this is not a comfort feature.
// Claude Code's UI is Ink, whose input handling calls setRawMode on startup and
// throws when stdin is not a TTY ("Raw mode is not supported on the current
// process.stdin, which Ink uses as input stream by default") — verified in the
// shipped binary. Handing an agent plain pipes therefore does not degrade it, it
// prevents it from starting at all.
//
// A PTY changes two things the caller must know:
//   - stdout and stderr are ONE stream. A terminal has a single output; the
//     kernel cannot tell the two apart once both are the pty slave, so
//     everything arrives as FrameStdout.
//   - The window size matters. An agent that renders a TUI to 80x24 when the
//     pane is 200x50 is unusable, so FrameResize is honored here rather than
//     accepted-and-ignored.

// ptySession is a started command attached to a pseudo-terminal.
type ptySession struct {
	master *os.File // read agent output, write agent input, set the size

	closeOnce sync.Once
	closeErr  error
}

// Default geometry when the launcher supplies none (a non-tty pane, or an older
// launcher). A dimension that is present but out of range is CLAMPED on the
// attach path — see winsize — rather than failing the session.
const (
	defaultCols = 120
	defaultRows = 40
)

// dimension validates ONE wire-supplied terminal dimension against the type it
// will actually be stored in.
//
// The guard has to be against uint16, not against `> 0`: cols and rows arrive as
// Go ints and reach the ioctl as uint16, so 65536 passes a positivity check and
// truncates to a ZERO-column terminal — a TUI that divides by the column count
// then crashes or spins. Validating per dimension also means a launcher that
// reports 200x0 keeps its known 200 instead of having both replaced.
func dimension(v, fallback int) (uint16, bool) {
	if v <= 0 || v > math.MaxUint16 {
		return uint16(fallback), v != 0
	}
	return uint16(v), false
}

// winsize resolves a geometry pair for the ATTACH path, clamping rather than
// refusing: a launcher that miscomputes one dimension should get a usable
// terminal, not lose the session. (A RESIZE is different — there the peer is
// asserting something specific, so a bad value is an error it should see.)
//
// The guard is against uint16, not against `> 0`: these reach the ioctl as
// uint16, so 65536 would otherwise pass a positivity check and truncate to a
// ZERO-column terminal that no TUI can lay out.
func winsize(cols, rows int) (*pty.Winsize, []string) {
	var clamped []string
	c, badC := dimension(cols, defaultCols)
	if badC {
		clamped = append(clamped, fmt.Sprintf("cols=%d", cols))
	}
	r, badR := dimension(rows, defaultRows)
	if badR {
		clamped = append(clamped, fmt.Sprintf("rows=%d", rows))
	}
	return &pty.Winsize{Cols: c, Rows: r}, clamped
}

// startPTY starts cmd attached to a new pty and returns its master side.
func startPTY(cmd *exec.Cmd, initial *sockproto.Resize, plumbing bool) (*ptySession, error) {
	cols, rows := 0, 0
	if initial != nil {
		cols, rows = initial.Cols, initial.Rows
	}
	size, clamped := winsize(cols, rows)
	if len(clamped) > 0 {
		slog.Default().Warn("clamping an out-of-range terminal dimension to the default",
			"clamped", clamped, "using", fmt.Sprintf("%dx%d", size.Cols, size.Rows))
	}
	// StartWithSize sets the size BEFORE the child runs, so an agent that reads
	// its dimensions once at startup sees the pane's real geometry rather than
	// the 80x24 default followed by a resize it may never re-read.
	master, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return nil, fmt.Errorf("allocate pty: %w", err)
	}
	// Make the master POLLABLE before anything reads it. creack/pty opens it
	// blocking, so Go serves reads with a plain syscall on a thread — and a read
	// parked there is uninterruptible: neither Close nor SetReadDeadline returns
	// it (both verified by probe). A pump parked that way leaks a goroutine, an
	// OS thread and the fd for the life of the process, once per wedged session.
	// Re-wrapping the fd non-blocking hands it to the runtime poller, where Close
	// does unblock the reader.
	master, err = pollable(master)
	if err != nil {
		// The child is ALREADY RUNNING — StartWithSize forked and exec'd it — so
		// returning here without killing it would leave a live agent holding the
		// worktree while the session slot is released: the orphan the
		// one-agent-per-session fence exists to prevent.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = master.Close()
		return nil, fmt.Errorf("make pty master pollable: %w", err)
	}

	// The line discipline depends on what this pty IS.
	//
	// NATIVE: it is the agent's terminal, so leave it exactly as the kernel made
	// it. A terminal is where ^C interrupts, ^D ends input, ^S pauses output and
	// \n becomes \r\n; an interactive agent turns off what it wants to handle
	// itself. Reaching in here is how an earlier version broke output (clearing
	// OPOST staircased every line) and EOF (clearing ICANON), and clearing ISIG
	// would remove the last interrupt path a non-interactive program has — the
	// launcher's own raw mode means no local SIGINT is ever raised to forward.
	//
	// CONTAINER: it is PLUMBING to the docker client, which allocates the real
	// terminal inside the container with -t. Here the outer discipline must do
	// nothing at all: it would echo bytes back, double-translate newlines the
	// inner tty already produced, let ^C tear down the client instead of
	// reaching the agent, and let ^S/^O freeze or discard the agent's output on
	// the way through. Raw is exactly "transparent".
	if plumbing {
		if _, err := term.MakeRaw(int(master.Fd())); err != nil {
			slog.Default().Warn("could not make the container pty transparent", "err", err)
		}
	}
	return &ptySession{master: master}, nil
}

// pollable re-wraps a blocking fd as a non-blocking one owned by Go's runtime
// poller, so a blocked Read can be unblocked by Close.
func pollable(f *os.File) (*os.File, error) {
	// F_DUPFD_CLOEXEC, not Dup+CloseOnExec: the two-step form leaves a window in
	// which a concurrently forking child (this worker execs docker, git and the
	// agent itself from other goroutines) inherits the pty master — and an
	// unrelated long-lived child holding a copy keeps the pty alive, undoing the
	// very leak fix this function exists for.
	dup, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return f, err
	}
	if err := unix.SetNonblock(dup, true); err != nil {
		_ = unix.Close(dup)
		return f, err
	}
	g := os.NewFile(uintptr(dup), f.Name())
	if err := f.Close(); err != nil {
		_ = g.Close()
		return f, err
	}
	return g, nil
}

// resize applies a new window size, which is what makes a running TUI redraw.
func (p *ptySession) resize(r sockproto.Resize) error {
	// A resize must never silently substitute a default: unlike the initial
	// geometry, a bad value here means the peer is wrong and should be told.
	if r.Cols <= 0 || r.Cols > math.MaxUint16 || r.Rows <= 0 || r.Rows > math.MaxUint16 {
		return fmt.Errorf("invalid window size %dx%d (each dimension must be 1..%d)", r.Cols, r.Rows, math.MaxUint16)
	}
	return pty.Setsize(p.master, &pty.Winsize{Cols: uint16(r.Cols), Rows: uint16(r.Rows)})
}

func (p *ptySession) Read(b []byte) (int, error)  { return p.master.Read(b) }
func (p *ptySession) Write(b []byte) (int, error) { return p.master.Write(b) }

// Close hangs up the line. Idempotent, because it is called both from the
// drain-bound after the agent exits and from streamExec's defer.
func (p *ptySession) Close() error {
	p.closeOnce.Do(func() { p.closeErr = p.master.Close() })
	return p.closeErr
}

var _ io.ReadWriteCloser = (*ptySession)(nil)
