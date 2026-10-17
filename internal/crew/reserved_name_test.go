package crew

import (
	"errors"
	"testing"
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
