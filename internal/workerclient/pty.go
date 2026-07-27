package workerclient

import (
	"fmt"
	"golang.org/x/term"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"

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

// Default geometry when the launcher supplies none (a non-tty pane, an older
// launcher, or a dimension that failed validation).
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
func dimension(v, fallback int) (uint16, error) {
	if v == 0 {
		return uint16(fallback), nil
	}
	if v < 0 || v > math.MaxUint16 {
		return 0, fmt.Errorf("terminal dimension %d is outside 1..%d", v, math.MaxUint16)
	}
	return uint16(v), nil
}

// winsize validates a geometry pair.
func winsize(cols, rows int) (*pty.Winsize, error) {
	c, err := dimension(cols, defaultCols)
	if err != nil {
		return nil, err
	}
	r, err := dimension(rows, defaultRows)
	if err != nil {
		return nil, err
	}
	return &pty.Winsize{Cols: c, Rows: r}, nil
}

// startPTY starts cmd attached to a new pty and returns its master side.
func startPTY(cmd *exec.Cmd, initial *sockproto.Resize) (*ptySession, error) {
	cols, rows := 0, 0
	if initial != nil {
		cols, rows = initial.Cols, initial.Rows
	}
	size, err := winsize(cols, rows)
	if err != nil {
		return nil, err
	}
	// StartWithSize sets the size BEFORE the child runs, so an agent that reads
	// its dimensions once at startup sees the pane's real geometry rather than
	// the 80x24 default followed by a resize it may never re-read.
	master, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return nil, fmt.Errorf("allocate pty: %w", err)
	}
	p := &ptySession{master: master}

	// Put the line discipline in raw mode immediately. A fresh pty slave has
	// ECHO and ISIG on, so anything the launcher sends before the child
	// configures its own terminal would be echoed back into the output stream —
	// and a 0x03 in that window would raise SIGINT against this pty's
	// foreground group rather than reaching the agent. An interactive agent sets
	// raw mode itself; doing it here first only closes the startup window.
	if _, err := term.MakeRaw(int(master.Fd())); err != nil {
		// Not fatal: the agent still gets a terminal, it just may echo briefly.
		slog.Default().Warn("could not put the pty in raw mode", "err", err)
	}
	return p, nil
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
