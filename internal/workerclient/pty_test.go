package workerclient

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/sockproto"
)

// TestWaitOrTimeout_DoesNotWaitForeverOnAStuckPump pins the mechanism behind the
// exit-frame fix.
//
// The bug: a pty master is nobody's pipe, so cmd.Wait closes nothing, and on
// Linux a descendant outliving the agent keeps the master readable — the output
// pump never returns, so an unbounded pumps.Wait meant no exit frame and, at
// MaxSessions=1, a worker wedged until restart.
//
// My first fix was to close the master and keep waiting. That is NOT sufficient,
// which the next test demonstrates: a read already blocked in the syscall is not
// necessarily interrupted by Close. So the wait is bounded instead, and the
// invariant is that the exit frame is always written.
func TestWaitOrTimeout_DoesNotWaitForeverOnAStuckPump(t *testing.T) {
	var stuck sync.WaitGroup
	stuck.Add(1) // never Done: stands in for a pump blocked on a live pty slave

	start := time.Now()
	assert.False(t, waitOrTimeout(&stuck, 200*time.Millisecond),
		"a stuck pump must time out rather than block the exit frame")
	assert.Less(t, time.Since(start), 3*time.Second)

	var quick sync.WaitGroup
	quick.Add(1)
	go func() { time.Sleep(20 * time.Millisecond); quick.Done() }()
	assert.True(t, waitOrTimeout(&quick, 5*time.Second),
		"a pump that drains must be waited for, so output is not truncated")
}

// TestPTYClose_UnblocksAParkedPump pins the property the leak fix rests on: a
// pump blocked reading the master must be released by Close.
//
// It is NOT true of the fd creack/pty returns — that one is blocking, so Go
// serves the read with a syscall on a thread and neither Close nor
// SetReadDeadline interrupts it, leaking a goroutine, an OS thread and the fd
// per wedged session. startPTY therefore re-wraps it non-blocking, which hands
// it to the runtime poller; this test pins that re-wrap.
func TestPTYClose_UnblocksAParkedPump(t *testing.T) {
	master, slave, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() { _ = slave.Close() }) // the "lingering descendant"

	master, err = pollable(master)
	require.NoError(t, err)
	p := &ptySession{master: master}
	readReturned := make(chan error, 1)
	go func() {
		buf := make([]byte, 32)
		_, err := p.Read(buf)
		readReturned <- err
	}()
	select {
	case err := <-readReturned:
		t.Fatalf("the read returned early (%v) — the fixture is not reproducing a blocked pump", err)
	case <-time.After(300 * time.Millisecond):
	}

	require.NoError(t, p.Close())
	select {
	case <-readReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not unblock the pump: the fd, its goroutine and an OS thread would leak per wedged session")
	}
	assert.NoError(t, p.Close(), "Close must be idempotent")
}

// TestPTY_PreservesOutputProcessingAndCanonicalInput pins the two line-discipline
// flags an earlier "just use raw mode" fix silently destroyed.
//
// OPOST: without it the pty stops translating \n to \r\n, so every line of
// agent output staircases down the pane — including Ink's, since libuv's raw
// mode ORs ONLCR into the CURRENT termios and cannot restore OPOST.
// ICANON: without it ^D is delivered as a literal byte, so a pty session has no
// EOF mechanism at all — which is exactly what the half-close fix promises.
func TestPTY_PreservesOutputProcessingAndCanonicalInput(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "printf 'one\\ntwo\\n'")
	p, err := startPTY(cmd, &sockproto.Resize{Cols: 80, Rows: 24})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	var out []byte
	buf := make([]byte, 256)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !bytes.Contains(out, []byte("two")) {
		n, err := p.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			break
		}
	}
	_ = cmd.Wait()
	assert.Contains(t, string(out), "one\r\ntwo\r\n",
		"OPOST must survive: without it the agent's output staircases")

	// ICANON: ^D must end a reader, not arrive as a byte.
	catCmd := exec.Command("cat")
	cp, err := startPTY(catCmd, &sockproto.Resize{Cols: 80, Rows: 24})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cp.Close() })
	_, err = cp.Write([]byte("hello\n"))
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)
	_, err = cp.Write([]byte{4}) // ^D
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- catCmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("^D did not end the reader: a pty session would have no way to signal EOF")
	}
}

// TestPTYWinsize_ClampsRatherThanFailingTheAttach pins the narrowing the `> 0`
// guards missed — these reach a uint16 ioctl field, so 65536 became a
// ZERO-column terminal — and that the ATTACH path degrades rather than killing
// the session over one miscomputed dimension.
func TestPTYWinsize_ClampsRatherThanFailingTheAttach(t *testing.T) {
	for _, tc := range []struct{ cols, rows int }{
		{65536, 55}, {55, 65536}, {100000, 100000}, {-1, 24}, {80, -1},
	} {
		got, clamped := winsize(tc.cols, tc.rows)
		assert.NotEmpty(t, clamped, "%dx%d must be reported as clamped", tc.cols, tc.rows)
		assert.NotZero(t, got.Cols, "a zero-column terminal must never be produced")
		assert.NotZero(t, got.Rows)
	}

	// A dimension the launcher could not determine falls back per dimension,
	// rather than discarding the one it DID know.
	got, clamped := winsize(200, 0)
	assert.Empty(t, clamped, "an absent dimension is not a bad one")
	assert.Equal(t, uint16(200), got.Cols, "a known width must survive an unknown height")
	assert.Equal(t, uint16(defaultRows), got.Rows)

	got, _ = winsize(0, 0)
	assert.Equal(t, uint16(defaultCols), got.Cols)
	assert.Equal(t, uint16(defaultRows), got.Rows)
}

// TestPTYResize_RefusesRatherThanSubstituting pins that a bad resize is an error
// rather than a silent default: unlike the initial geometry, a bad value here
// means the peer is wrong and must be told.
func TestPTYResize_RefusesRatherThanSubstituting(t *testing.T) {
	master, slave, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() { _ = slave.Close() })
	p := &ptySession{master: master}
	t.Cleanup(func() { _ = p.Close() })

	require.NoError(t, p.resize(sockproto.Resize{Cols: 100, Rows: 30}))
	before, err := pty.GetsizeFull(master)
	require.NoError(t, err)

	require.Error(t, p.resize(sockproto.Resize{Cols: 65536, Rows: 30}))
	require.Error(t, p.resize(sockproto.Resize{Cols: 0, Rows: 30}))

	after, err := pty.GetsizeFull(master)
	require.NoError(t, err)
	assert.Equal(t, before.Cols, after.Cols, "a refused resize must leave the terminal alone")
	assert.Equal(t, before.Rows, after.Rows)
}

var _ = os.Getenv

// TestPTY_CrossCompiles is a guard against the defect that shipped in this PR's
// first pty commit: the termios ioctl constants are platform-specific (BSD uses
// TIOCGETA/TIOCSETA, Linux TCGETS/TCSETS), and naming the darwin ones inline
// made gt-worker-client fail to build for linux — the platform `make dist`
// exists to ship it to.
//
// A unit test cannot catch a build break for another GOOS, so this runs the
// compiler. It is skipped in -short mode because it costs a build.
func TestPTY_CrossCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("costs a cross-compile")
	}
	root, err := filepath.Abs("../..")
	require.NoError(t, err)

	for _, target := range []struct{ goos, goarch string }{
		{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "arm64"},
	} {
		t.Run(target.goos+"-"+target.goarch, func(t *testing.T) {
			cmd := exec.Command("go", "build", "-o", os.DevNull, "./cmd/gt-worker-client")
			cmd.Dir = root
			cmd.Env = append(os.Environ(),
				"CGO_ENABLED=0", "GOOS="+target.goos, "GOARCH="+target.goarch)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "the worker must build for %s/%s:\n%s", target.goos, target.goarch, out)
		})
	}
}
