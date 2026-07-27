package workerclient

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

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
	p, err := startPTY(cmd, &sockproto.Resize{Cols: 80, Rows: 24}, false)
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
	cp, err := startPTY(catCmd, &sockproto.Resize{Cols: 80, Rows: 24}, false)
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

// TestPTY_NativeKeepsTerminalSemantics pins the role split. A NATIVE pty IS the
// agent's terminal, so the flags that give it MEANING stay as the kernel made
// them: clearing ISIG would remove the last interrupt path (the launcher's own
// raw mode means no local SIGINT is ever raised to forward as a signal frame),
// and clearing ICANON or OPOST breaks EOF and output translation.
//
// Flow control is the exception, covered by TestPTY_NativeDisablesFlowControl:
// it is not a terminal semantic an agent relies on, and it can wedge the worker.
func TestPTY_NativeKeepsTerminalSemantics(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	p, err := startPTY(cmd, &sockproto.Resize{Cols: 80, Rows: 24}, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })

	tio, err := unix.IoctlGetTermios(int(p.master.Fd()), ioctlGet)
	require.NoError(t, err)
	assert.NotZero(t, tio.Lflag&unix.ISIG, "^C must still interrupt: it is the only interrupt path left")
	assert.NotZero(t, tio.Lflag&unix.ICANON, "^D must still mean EOF")
	assert.NotZero(t, tio.Oflag&unix.OPOST, "output translation must survive or every line staircases")
}

// TestPTY_ContainerIsTransparentPlumbing pins the other half: with -t the docker
// client owns the real terminal inside the container, so the outer pty must do
// NOTHING — otherwise it echoes bytes back, double-translates newlines the inner
// tty already produced, lets ^C tear down the client instead of reaching the
// agent, and lets ^S/^O freeze or discard the agent's output in transit.
func TestPTY_ContainerIsTransparentPlumbing(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	p, err := startPTY(cmd, &sockproto.Resize{Cols: 80, Rows: 24}, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })

	tio, err := unix.IoctlGetTermios(int(p.master.Fd()), ioctlGet)
	require.NoError(t, err)
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"ECHO", tio.Lflag&unix.ECHO != 0},
		{"ISIG", tio.Lflag&unix.ISIG != 0},
		{"IEXTEN", tio.Lflag&unix.IEXTEN != 0},
		{"OPOST", tio.Oflag&unix.OPOST != 0},
		{"IXON", tio.Iflag&unix.IXON != 0},
	} {
		assert.False(t, f.set, "%s must be off: this pty is plumbing, not a terminal", f.name)
	}
}

// TestPollable_IsCloseOnExec pins that the duplicated master cannot leak into a
// concurrently forked child. The worker forks constantly (docker, git, the agent
// itself), and a child holding a copy keeps the pty alive — undoing the leak fix
// the dup exists for.
func TestPollable_IsCloseOnExec(t *testing.T) {
	master, slave, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() { _ = slave.Close() })

	dup, err := pollable(master)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dup.Close() })

	flags, err := unix.FcntlInt(dup.Fd(), unix.F_GETFD, 0)
	require.NoError(t, err)
	assert.NotZero(t, flags&unix.FD_CLOEXEC,
		"the master must be close-on-exec, atomically — Dup then CloseOnExec leaves a fork window")
}

// TestPTY_NativeDisablesFlowControl pins the one flag a native pty must NOT
// keep. IXON is how a 1980s terminal asked a host to stop transmitting; over
// this transport it has no job (the stream has its own flow control) and one
// 0x13 byte from the wire stops the pty's output queue — the agent then blocks
// writing and cannot be reaped even by SIGKILL, so the session and, at
// max_sessions: 1, the worker wedge from a single keystroke nothing remote can
// undo once the pane is gone.
func TestPTY_NativeDisablesFlowControl(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	p, err := startPTY(cmd, &sockproto.Resize{Cols: 80, Rows: 24}, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })

	tio, err := unix.IoctlGetTermios(int(p.master.Fd()), ioctlGet)
	require.NoError(t, err)
	assert.Zero(t, tio.Iflag&unix.IXON, "^S must not be able to stall the agent's output")
	assert.Zero(t, tio.Lflag&unix.IEXTEN, "^O discards output just as silently")

	// The semantics that make it a TERMINAL are still untouched.
	assert.NotZero(t, tio.Lflag&unix.ISIG, "^C must still interrupt")
	assert.NotZero(t, tio.Lflag&unix.ICANON, "^D must still mean EOF")
	assert.NotZero(t, tio.Oflag&unix.OPOST, "output translation must survive")
}

// TestDetachFilter_SurvivesOneBytePerFrame pins the shape typed input actually
// takes: the launcher reads its terminal in raw mode (VMIN=1), so a typed
// sequence arrives as separate one-byte frames. A per-frame scan — what an
// earlier version did — never sees it.
func TestDetachFilter_SurvivesOneBytePerFrame(t *testing.T) {
	d := &detachFilter{seq: containerDetachBytes}

	var out []byte
	for _, b := range containerDetachBytes { // three separate frames, as typed
		out = append(out, d.filter([]byte{b})...)
	}
	assert.Empty(t, out, "a sequence split across frames must still be removed")

	// Ordinary input flows, including a partial sequence followed by something
	// else — two of three is a keystroke, not a detach.
	d2 := &detachFilter{seq: containerDetachBytes}
	var got []byte
	for _, chunk := range [][]byte{{'a'}, {0x1c}, {0x1c}, {'b'}, {'c'}} {
		got = append(got, d2.filter(chunk)...)
	}
	assert.Equal(t, []byte{'a', 0x1c, 0x1c, 'b', 'c'}, got)

	// And within one frame, which is what a paste looks like.
	d3 := &detachFilter{seq: containerDetachBytes}
	in := append([]byte("hello"), containerDetachBytes...)
	assert.Equal(t, "helloworld", string(d3.filter(append(in, []byte("world")...))))
}

// TestDetachFilter_HoldsBackOnlyAPrefix pins the cost of cross-frame filtering:
// a trailing partial prefix waits for the next input, and nothing else does.
func TestDetachFilter_HoldsBackOnlyAPrefix(t *testing.T) {
	d := &detachFilter{seq: containerDetachBytes}
	assert.Equal(t, "abc", string(d.filter([]byte("abc"))), "ordinary bytes are never delayed")
	assert.Empty(t, d.filter([]byte{0x1c}), "a possible prefix is held")
	assert.Equal(t, []byte{0x1c, 'z'}, d.filter([]byte{'z'}), "and released once it cannot complete")
}

// TestNewDetachFilter_OnlyForContainerTTY pins the WIRING, which the filter's own
// unit tests cannot see: applying it to native stdin would silently corrupt an
// agent's input while leaving container detach live, and dropping it entirely
// would leave the fence open — both mutations the byte-level tests survive.
func TestNewDetachFilter_OnlyForContainerTTY(t *testing.T) {
	tty := &ptySession{}
	assert.NotNil(t, newDetachFilter(tty, "gt-work-demo-furiosa"), "a container client can be detached from")
	assert.Nil(t, newDetachFilter(tty, ""), "a native pty has no client in the middle")
	assert.Nil(t, newDetachFilter(nil, "gt-work-demo-furiosa"), "no tty means no escape proxy")
	assert.Nil(t, newDetachFilter(nil, ""))
}

// TestWaitReleasing_LeavesAFlushingAgentAlone pins the premise the valve must
// check. handleShutdown cancels on the GRACEFUL path, so a well-behaved agent
// that traps SIGTERM and flushes is the COMMON case at cancellation — firing on
// the timer alone hung it up mid-flush, discarding its last output and turning a
// clean exit into a signal death.
func TestWaitReleasing_LeavesAFlushingAgentAlone(t *testing.T) {
	prev := ptyWaitGrace
	ptyWaitGrace = 200 * time.Millisecond
	t.Cleanup(func() { ptyWaitGrace = prev })

	// Flushes steadily for well past the grace, then exits cleanly.
	cmd := exec.Command("/bin/sh", "-c",
		"i=0; while [ $i -lt 12 ]; do echo flushing-$i; sleep 0.1; i=$((i+1)); done; echo FLUSH-DONE; exit 0")
	p, err := startPTY(cmd, &sockproto.Resize{Cols: 80, Rows: 24}, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	// A pump, as streamExec runs one.
	var mu sync.Mutex
	var out []byte
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := p.Read(buf)
			mu.Lock()
			out = append(out, buf[:n]...)
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled from the start: the graceful-shutdown shape

	// The pump is alive for the whole flush, which is the signal the valve must
	// respect: while something is reading the master, the terminal is draining.
	pumpExited := make(chan struct{})
	err = waitReleasing(ctx, cmd, p, pumpExited, ptyWaitGrace, slog.Default(), "gt-demo-furiosa")
	require.NoError(t, err, "a flushing agent must exit cleanly, not by hangup")

	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, string(out), "FLUSH-DONE",
		"the agent's final output must survive: the valve is for a STALLED terminal")
}

// TestWaitReleasing_ReleasesAnUndrainedTerminal pins the case the valve exists
// for, and does it WITHOUT killing the child first — the earlier version of this
// test did, so cmd.Wait returned immediately and the valve was never consulted:
// it passed with the valve deleted.
//
// Here a chatty child fills the pty's output queue with nobody reading it, then
// is signalled. It cannot be reaped — not even by SIGKILL — until something
// drains or closes the terminal, which is exactly what the valve does.
func TestWaitReleasing_ReleasesAnUndrainedTerminal(t *testing.T) {
	prev := ptyWaitGrace
	ptyWaitGrace = 300 * time.Millisecond
	t.Cleanup(func() { ptyWaitGrace = prev })

	cmd := exec.Command("/bin/sh", "-c", "while :; do echo filling-the-output-queue; done")
	p, err := startPTY(cmd, &sockproto.Resize{Cols: 80, Rows: 24}, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	// NOTHING reads the master: the queue fills and the child blocks writing.
	time.Sleep(500 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, cmd.Process.Signal(syscall.SIGKILL))

	// The pump is already gone (there never was one), which is the valve's
	// premise: no reader remains.
	pumpExited := make(chan struct{})
	close(pumpExited)

	done := make(chan error, 1)
	go func() {
		done <- waitReleasing(ctx, cmd, p, pumpExited, ptyWaitGrace, slog.Default(), "gt-demo-furiosa")
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the valve never released the terminal: the session would wedge with no exit frame")
	}
}

// TestWriteAgentStdin_AppliesTheFilter pins the APPLICATION of the detach
// filter, which its own unit tests cannot see: with the call inlined in
// streamExec, deleting it left every test green because no test drives a
// container+tty session's stdin.
func TestWriteAgentStdin_AppliesTheFilter(t *testing.T) {
	var got bytes.Buffer
	d := &detachFilter{seq: containerDetachBytes}
	for _, b := range containerDetachBytes { // as typed: one byte per frame
		require.NoError(t, writeAgentStdin(&got, d, []byte{b}))
	}
	assert.Empty(t, got.String(), "the sequence must never reach the docker client")

	require.NoError(t, writeAgentStdin(&got, d, []byte("ok")))
	assert.Equal(t, "ok", got.String())

	// Without a filter — the native path — every byte passes through untouched.
	var native bytes.Buffer
	require.NoError(t, writeAgentStdin(&native, nil, containerDetachBytes))
	assert.Equal(t, containerDetachBytes, native.Bytes(), "native stdin must not be filtered")
}

// TestDetachFilter_HeldPrefixIsNotDeliveredAtEOF pins a deliberate loss. The
// held bytes can only ever be a strict prefix of the detach sequence, and 0x1c
// is VQUIT on the default line discipline the container gives the agent — so
// delivering them when input ends would SIGQUIT the agent that the half-close
// path exists to keep alive. A lost trailing ^\ is the better error.
func TestDetachFilter_HeldPrefixIsNotDeliveredAtEOF(t *testing.T) {
	d := &detachFilter{seq: containerDetachBytes}
	assert.Equal(t, "hi", string(d.filter([]byte("hi"))))
	assert.Empty(t, d.filter([]byte{0x1c}), "a possible prefix is held")

	// Nothing in the inbound path delivers it: the only reader of pending is
	// filter itself, resolving it against the NEXT input.
	assert.Equal(t, []byte{0x1c, 'z'}, d.filter([]byte{'z'}),
		"held bytes reach the agent only when later input proves them harmless")
}
