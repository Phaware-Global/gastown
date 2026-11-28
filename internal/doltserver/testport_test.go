package doltserver

import "testing"

// Code under test connects to its configured port directly, which the guard's
// bd and gt wrappers never see, so the default must not be the shared server's.
func TestDefaultConfigUnderGuardAvoidsProductionPort(t *testing.T) {
	if got := DefaultConfig(t.TempDir()).Port; got == DefaultPort {
		t.Errorf("DefaultConfig port = %d, the shared server's port", got)
	}
}
