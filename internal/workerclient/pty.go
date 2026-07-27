package workerclient

import (
	"fmt"
	"io"
	"os"
	"os/exec"

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
}

// startPTY starts cmd attached to a new pty and returns its master side.
func startPTY(cmd *exec.Cmd, initial *sockproto.Resize) (*ptySession, error) {
	var size *pty.Winsize
	if initial != nil && initial.Cols > 0 && initial.Rows > 0 {
		size = &pty.Winsize{Cols: uint16(initial.Cols), Rows: uint16(initial.Rows)}
	}
	// StartWithSize sets the size BEFORE the child runs, so an agent that reads
	// its dimensions once at startup sees the pane's real geometry rather than
	// the 80x24 default followed by a resize it may never re-read.
	master, err := pty.StartWithSize(cmd, sizeOrDefault(size))
	if err != nil {
		return nil, fmt.Errorf("allocate pty: %w", err)
	}
	return &ptySession{master: master}, nil
}

// sizeOrDefault supplies a sane initial geometry when the launcher did not send
// one (a non-tty pane, or an older launcher).
func sizeOrDefault(size *pty.Winsize) *pty.Winsize {
	if size != nil {
		return size
	}
	return &pty.Winsize{Cols: 120, Rows: 40}
}

// resize applies a new window size and signals the foreground process group,
// which is what makes a running TUI redraw.
func (p *ptySession) resize(r sockproto.Resize) error {
	if r.Cols <= 0 || r.Rows <= 0 {
		return fmt.Errorf("invalid window size %dx%d", r.Cols, r.Rows)
	}
	return pty.Setsize(p.master, &pty.Winsize{Cols: uint16(r.Cols), Rows: uint16(r.Rows)})
}

func (p *ptySession) Read(b []byte) (int, error)  { return p.master.Read(b) }
func (p *ptySession) Write(b []byte) (int, error) { return p.master.Write(b) }
func (p *ptySession) Close() error                { return p.master.Close() }

var _ io.ReadWriteCloser = (*ptySession)(nil)
