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
		if err := validateCrewName(name); !errors.Is(err, ErrInvalidCrewName) {
			t.Errorf("validateCrewName(%q) = %v, want ErrInvalidCrewName", name, err)
		}
	}
	if err := validateCrewName("assistant_bot"); err != nil {
		t.Errorf("validateCrewName(assistant_bot) = %v, want nil", err)
	}
}
