package daemon

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/polecat"
)

func TestShouldReapForCapRelief(t *testing.T) {
	floor := polecat.MinPolecatDirsPerRig // 30
	cases := []struct {
		name     string
		dirCount int
		cap      int
		want     bool
	}{
		// Floor cap (30): relief threshold is 30 - 5 = 25.
		{"floor: zero", 0, floor, false},
		{"floor: just below threshold", 24, floor, false},
		{"floor: at threshold", 25, floor, true},
		{"floor: at cap", 30, floor, true},
		// High-cap rig (max_polecats=100): threshold is 95 — a floor-based gate would
		// wrongly reap at 25 and loop; the parameterized cap must not.
		{"high cap: 25 dirs is well below its cap", 25, 100, false},
		{"high cap: just below its threshold", 94, 100, false},
		{"high cap: at its threshold", 95, 100, true},
	}
	for _, c := range cases {
		if got := shouldReapForCapRelief(c.dirCount, c.cap); got != c.want {
			t.Errorf("%s: shouldReapForCapRelief(%d, %d) = %v, want %v", c.name, c.dirCount, c.cap, got, c.want)
		}
	}
}

// The cleanup walks every polecat dir and takes far longer than the 30s per-rig
// worker ctx; it used to be killed by it every heartbeat and reclaim nothing. It must
// run in the background (the heartbeat does not wait on it) and be single-flighted
// (a slow cleanup is not stacked behind itself on the next heartbeat).
func TestReapRigStalePolecatDirsNearCap_RunsInBackgroundSingleFlight(t *testing.T) {
	townRoot := t.TempDir()
	polecatsDir := filepath.Join(townRoot, "myrig", "polecats")
	for i := 0; i < polecat.MinPolecatDirsPerRig; i++ {
		if err := os.MkdirAll(filepath.Join(polecatsDir, fmt.Sprintf("p%02d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	calls := filepath.Join(townRoot, "calls.log")
	release := filepath.Join(townRoot, "release")
	fakeGT := filepath.Join(townRoot, "fake-gt")
	script := "#!/bin/sh\necho \"$@\" >> " + calls + "\nwhile [ ! -e " + release + " ]; do sleep 0.05; done\n"
	if err := os.WriteFile(fakeGT, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(io.Discard, "", 0),
		gtPath: fakeGT,
	}

	start := time.Now()
	d.reapRigStalePolecatDirsNearCap("myrig")
	d.reapRigStalePolecatDirsNearCap("myrig") // heartbeat N+1 while the first is still running
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("reapRigStalePolecatDirsNearCap blocked for %v waiting on the cleanup", elapsed)
	}

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("cleanup to start", func() bool { _, err := os.Stat(calls); return err == nil })

	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor("in-flight slot to clear", func() bool { _, busy := d.capReliefInFlight.Load("myrig"); return !busy })

	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "polecat stale myrig --cleanup"); got != 1 {
		t.Errorf("cleanup launched %d times across two overlapping heartbeats, want 1; log:\n%s", got, data)
	}
}

// Shutdown cancels the cleanup's context. gt must be asked to stop (SIGTERM) so it
// can finish the nuke in progress, not SIGKILLed mid-nuke, and the goroutine must be
// tracked so shutdown can wait for it.
func TestRunCapReliefCleanup_CancelSendsSIGTERM(t *testing.T) {
	townRoot := t.TempDir()
	started := filepath.Join(townRoot, "started")
	termed := filepath.Join(townRoot, "termed")
	fakeGT := filepath.Join(townRoot, "fake-gt")
	script := "#!/bin/sh\ntrap 'touch " + termed + "; exit 0' TERM\ntouch " + started + "\nwhile true; do sleep 0.05; done\n"
	if err := os.WriteFile(fakeGT, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(io.Discard, "", 0),
		gtPath: fakeGT,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.capReliefWG.Add(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer d.capReliefWG.Done()
		d.runCapReliefCleanup(ctx, "myrig")
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for fake gt to start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	wgDone := make(chan struct{})
	go func() { d.capReliefWG.Wait(); close(wgDone) }()
	select {
	case <-wgDone:
	case <-time.After(15 * time.Second):
		t.Fatal("cleanup did not exit after cancel")
	}
	<-done
	if _, err := os.Stat(termed); err != nil {
		t.Error("gt was not sent SIGTERM on cancel (killed outright instead)")
	}
}

// A cleanup exits 0 even when it reclaims nothing, so the log entry must carry what
// it actually did, minus the per-polecat listing.
func TestCapReliefOutcome(t *testing.T) {
	out := "Detecting stale polecats in myrig...\n\n○ alpha (stale)\n    Reason: no active session\n\n" +
		"Summary: 1 stale, 4 active\n\nCleaning up 1 stale polecat(s)...\nNuking alpha...\n  failed (worktree removal failed: busy)\n"
	got := capReliefOutcome(out)
	want := "Summary: 1 stale, 4 active\n\nCleaning up 1 stale polecat(s)...\nNuking alpha...\n  failed (worktree removal failed: busy)"
	if got != want {
		t.Errorf("capReliefOutcome = %q, want %q", got, want)
	}

	if got := capReliefOutcome("No polecats found.\n"); got != "No polecats found." {
		t.Errorf("no Summary line: got %q, want the whole output", got)
	}

	long := "listing\nSummary: 20 stale, 0 active\n" + strings.Repeat("Nuking x...\n  ✓ deleted worktree\n", 200) + "\n✓ Nuked 20 stale polecat(s).\n"
	got = capReliefOutcome(long)
	if !strings.HasPrefix(got, "Summary: 20 stale, 0 active\n... (") || !strings.HasSuffix(got, "✓ Nuked 20 stale polecat(s).") {
		t.Errorf("long output must keep the Summary line and the final tally, got:\n%s", got)
	}
	if len(got) > capReliefOutcomeTailBytes+100 || !utf8.ValidString(got) {
		t.Errorf("long output: %d bytes, valid UTF-8 = %v", len(got), utf8.ValidString(got))
	}
}
