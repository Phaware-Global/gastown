package crew

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

// TestValidateCrewNameRejectsTownRoles pins the reservation of town-level
// role names: a crew member called "assistant" (or mayor/deacon) would collide
// with that role's mail address and directory.
func TestValidateCrewNameRejectsTownRoles(t *testing.T) {
	for _, name := range []string{"assistant", "Assistant", "mayor", "deacon"} {
		if err := validateNewCrewName(name); !errors.Is(err, ErrInvalidCrewName) {
			t.Errorf("validateNewCrewName(%q) = %v, want ErrInvalidCrewName", name, err)
		}
		// Pre-existing members with these names must stay manageable
		// (Get/Stop/Remove use the plain validator).
		if err := validateCrewName(name); err != nil {
			t.Errorf("validateCrewName(%q) = %v, want nil for existing members", name, err)
		}
	}
	if err := validateNewCrewName("assistant_bot"); err != nil {
		t.Errorf("validateNewCrewName(assistant_bot) = %v, want nil", err)
	}
}

// TestStartRefusesReservedCrewName pins the creation path that Start takes
// when a member does not exist (implicit addLocked): a reserved name must be
// refused there too, not only in Add.
func TestStartRefusesReservedCrewName(t *testing.T) {
	rigPath := filepath.Join(t.TempDir(), "test-rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	m := NewManager(&rig.Rig{Name: "test-rig", Path: rigPath}, git.NewGit(rigPath))
	err := m.Start("assistant", StartOptions{})
	if !errors.Is(err, ErrInvalidCrewName) {
		t.Fatalf("Start(assistant) = %v, want ErrInvalidCrewName (implicit create must enforce the reservation)", err)
	}
	if _, err := m.addLocked("mayor", false); !errors.Is(err, ErrInvalidCrewName) {
		t.Errorf("addLocked(mayor) = %v, want ErrInvalidCrewName", err)
	}
	if _, err := os.Stat(filepath.Join(rigPath, "crew", "assistant")); !os.IsNotExist(err) {
		t.Error("Start(assistant) created a crew directory despite the reservation")
	}
}
