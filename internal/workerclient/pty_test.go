package workerclient

import (
	"os"
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

// TestPTYClose_DoesNotReliablyInterruptABlockedRead records WHY the wait is
// bounded rather than relying on Close. This is an observation about the
// platform, not a requirement on it: if a future Go or OS does interrupt the
// read, the bounded wait simply returns sooner.
func TestPTYClose_DoesNotReliablyInterruptABlockedRead(t *testing.T) {
	master, slave, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() { _ = slave.Close() }) // the "lingering descendant"

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
		t.Log("Close DID interrupt the blocked read on this platform; the bounded wait then returns immediately")
	case <-time.After(2 * time.Second):
		t.Log("Close did NOT interrupt the blocked read — this is the case the bounded wait exists for")
	}
	assert.NoError(t, p.Close(), "Close must be idempotent")
}

// TestPTYWinsize_RejectsWhatTruncates pins the narrowing the `> 0` guards missed:
// these values reach a uint16 ioctl field, so 65536 became a ZERO-column
// terminal that no TUI can lay out.
func TestPTYWinsize_RejectsWhatTruncates(t *testing.T) {
	for _, tc := range []struct{ cols, rows int }{
		{65536, 55}, {55, 65536}, {100000, 100000}, {-1, 24}, {80, -1},
	} {
		_, err := winsize(tc.cols, tc.rows)
		require.Error(t, err, "%dx%d must be refused", tc.cols, tc.rows)
		assert.Contains(t, err.Error(), "outside 1..65535")
	}

	// A dimension the launcher could not determine falls back per dimension,
	// rather than discarding the one it DID know.
	got, err := winsize(200, 0)
	require.NoError(t, err)
	assert.Equal(t, uint16(200), got.Cols, "a known width must survive an unknown height")
	assert.Equal(t, uint16(defaultRows), got.Rows)

	got, err = winsize(0, 0)
	require.NoError(t, err)
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
