package polecat

import "testing"

func TestEffectivePolecatDirCap(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		want       int
	}{
		{"negative uses floor", -1, MinPolecatDirsPerRig},
		{"zero uses floor", 0, MinPolecatDirsPerRig},
		{"default below floor uses floor", 10, MinPolecatDirsPerRig},
		{"one below floor uses floor", MinPolecatDirsPerRig - 1, MinPolecatDirsPerRig},
		{"floor remains floor", MinPolecatDirsPerRig, MinPolecatDirsPerRig},
		{"above floor is honored", 45, 45},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectivePolecatDirCap(tt.configured); got != tt.want {
				t.Errorf("EffectivePolecatDirCap(%d) = %d, want %d", tt.configured, got, tt.want)
			}
		})
	}
}
