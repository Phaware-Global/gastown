//go:build !windows

package util

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestSetGracefulProcessGroup_TermFirst: cancellation sends SIGTERM, so a
// script that traps it gets to clean up.
func TestSetGracefulProcessGroup_TermFirst(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleaned-up")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "bash", "-c", `trap 'touch "$0"; exit 0' TERM; sleep 30 & wait`, marker)
	SetGracefulProcessGroup(cmd, 5*time.Second)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // let bash install the trap
	cancel()
	_ = cmd.Wait()

	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the TERM trap did not run: %v", err)
	}
}

// TestSetGracefulProcessGroup_KillsAfterGrace: a group that ignores SIGTERM
// is SIGKILLed once the grace period ends.
func TestSetGracefulProcessGroup_KillsAfterGrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "bash", "-c", `trap '' TERM; sleep 30 & wait`)
	SetGracefulProcessGroup(cmd, time.Second)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	cancel()
	_ = cmd.Wait()

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Wait returned %v after cancel; SIGKILL should follow the 1s grace", elapsed)
	}
}
