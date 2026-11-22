package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func writeRigSettings(t *testing.T, rigPath, body string) {
	t.Helper()
	path := config.RigSettingsPath(rigPath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

// gt done reads the host worktree, where a remote polecat's work never is. It
// must refuse rather than complete a bead from a worktree that holds no work.
func TestRemotePolecatDoneError(t *testing.T) {
	t.Run("a rig on a remote execution backend is refused", func(t *testing.T) {
		rig := t.TempDir()
		writeRigSettings(t, rig, `{"type":"rig-settings","version":1,"execution":{"backend":"socket","socket":{"address":"worker.example:9899"}}}`)
		err := remotePolecatDoneError(rig, "demo", ExitCompleted)
		if err == nil {
			t.Fatal("expected gt done to be refused for a remote rig")
		}
		for _, want := range []string{"demo", "socket", "refs/remote-work/"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should mention %q, got: %v", want, err)
			}
		}
	})
	// Only COMPLETED reads the worktree for work. ESCALATED and DEFERRED
	// report a stuck or parked polecat, and a remote one must be able to.
	t.Run("a remote rig can still escalate or defer", func(t *testing.T) {
		rig := t.TempDir()
		writeRigSettings(t, rig, `{"type":"rig-settings","version":1,"execution":{"backend":"socket","socket":{"address":"worker.example:9899"}}}`)
		for _, exit := range []string{ExitEscalated, ExitDeferred} {
			if err := remotePolecatDoneError(rig, "demo", exit); err != nil {
				t.Errorf("%s must not be refused for a remote rig: %v", exit, err)
			}
		}
	})
	t.Run("a local rig is not affected", func(t *testing.T) {
		rig := t.TempDir()
		writeRigSettings(t, rig, `{"type":"rig-settings","version":1}`)
		if err := remotePolecatDoneError(rig, "demo", ExitCompleted); err != nil {
			t.Fatalf("local rig: %v", err)
		}
	})
	t.Run("a rig with no settings file is local", func(t *testing.T) {
		if err := remotePolecatDoneError(t.TempDir(), "demo", ExitCompleted); err != nil {
			t.Fatalf("no settings: %v", err)
		}
	})
}
