package daemon

import "testing"

func TestDoltOffsiteEnabled(t *testing.T) {
	if !doltOffsiteEnabled(nil) {
		t.Error("nil config should default to offsite enabled")
	}
	if !doltOffsiteEnabled(&DoltBackupConfig{Enabled: true}) {
		t.Error("unset offsite should default to enabled")
	}
	off := false
	if doltOffsiteEnabled(&DoltBackupConfig{Enabled: true, Offsite: &off}) {
		t.Error("offsite=false should disable the offsite sync")
	}
	on := true
	if !doltOffsiteEnabled(&DoltBackupConfig{Enabled: true, Offsite: &on}) {
		t.Error("offsite=true should enable the offsite sync")
	}
}
