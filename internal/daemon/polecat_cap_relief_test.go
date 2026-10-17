package daemon

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
