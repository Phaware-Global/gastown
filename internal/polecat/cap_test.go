package polecat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makePolecatDirs(t *testing.T, n int) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "polecats")
	for i := 0; i < n; i++ {
		if err := os.MkdirAll(filepath.Join(dir, "p"+string(rune('a'+i%26))+string(rune('a'+i/26))), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Hidden dirs and plain files never count against the cap.
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCountPolecatDirs(t *testing.T) {
	got, err := CountPolecatDirs(makePolecatDirs(t, 7))
	if err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Errorf("CountPolecatDirs = %d, want 7 (hidden dirs and files excluded)", got)
	}
}

// A spawn attempt at the cap must return an error that names the cap, never nil:
// returning nil is what let "gt sling <rig>" exit 0 having done nothing.
func TestCheckDirCap_AtCapFailsLoudly(t *testing.T) {
	dir := makePolecatDirs(t, MinPolecatDirsPerRig)
	err := CheckDirCap("myrig", dir, MinPolecatDirsPerRig)
	if err == nil {
		t.Fatal("CheckDirCap at the cap returned nil; spawn would silently no-op")
	}
	for _, want := range []string{"myrig", "30 polecat directories", "max 30", "gt polecat stale myrig --cleanup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestCheckDirCap_BelowCapAndMissingDir(t *testing.T) {
	if err := CheckDirCap("myrig", makePolecatDirs(t, MinPolecatDirsPerRig-1), MinPolecatDirsPerRig); err != nil {
		t.Errorf("one below the cap: unexpected error %v", err)
	}
	if err := CheckDirCap("myrig", filepath.Join(t.TempDir(), "absent"), MinPolecatDirsPerRig); err != nil {
		t.Errorf("missing polecats dir: unexpected error %v", err)
	}
}

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
