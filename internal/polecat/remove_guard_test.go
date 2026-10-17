package polecat

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

// The guard is how a staleness cleanup re-verifies its decision atomically with the
// removal: it must run with the per-polecat lock held, and a guard error must abort
// the removal before anything is touched.
func TestRemoveWithGuard_GuardErrorAbortsUnderLock(t *testing.T) {
	root := t.TempDir()
	m := NewManager(&rig.Rig{Name: "test-rig", Path: root}, git.NewGit(root), nil)
	dir := filepath.Join(root, "polecats", "p1", "test-rig")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	errReused := errors.New("reused since detection")
	lockHeld := false
	err := m.RemoveWithGuard("p1", true, true, false, func() error {
		other := flock.New(filepath.Join(root, ".runtime", "locks", "polecat-p1.lock"))
		got, lerr := other.TryLock()
		if lerr == nil && got {
			_ = other.Unlock()
		}
		lockHeld = lerr == nil && !got
		return errReused
	})
	if !errors.Is(err, errReused) {
		t.Fatalf("RemoveWithGuard = %v, want the guard's error", err)
	}
	if !lockHeld {
		t.Error("guard ran without the per-polecat lock held, so a reuse can slip in before the removal")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "work.txt")); statErr != nil {
		t.Errorf("worktree was modified despite the guard aborting: %v", statErr)
	}
}
