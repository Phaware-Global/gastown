package beads

import (
	"path/filepath"
	"strings"
	"testing"
)

func testEnvValue(env []string, key string) (string, bool) {
	for _, entry := range env {
		if v, ok := strings.CutPrefix(entry, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

// A relative beads dir is the artifact of joining a rig name or prefix onto
// an empty town root; pinning it makes bd bootstrap a stray embedded store at
// <subprocess cwd>/<relative path> and strand writes (2026-07-04 silent
// write-loss incident). The env builders must never emit a relative pin.
func TestBuildPinnedBDEnvRejectsRelativeBeadsDir(t *testing.T) {
	for _, dir := range []string{
		"heartworks_android/.beads", // rig-name join onto empty town root
		"gt-/.beads",                // prefix join onto empty town root
		".beads",
	} {
		env := BuildPinnedBDEnv(nil, dir)
		if v, ok := testEnvValue(env, "BEADS_DIR"); ok {
			t.Errorf("BuildPinnedBDEnv(%q) pinned BEADS_DIR=%q; relative dirs must not be pinned", dir, v)
		}
	}
}

func TestBuildPinnedBDEnvKeepsAbsoluteBeadsDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".beads")
	env := BuildPinnedBDEnv(nil, dir)
	if v, ok := testEnvValue(env, "BEADS_DIR"); !ok || v != dir {
		t.Errorf("BuildPinnedBDEnv(%q): BEADS_DIR=%q (present=%v), want the absolute dir preserved", dir, v, ok)
	}
}

func TestSanitizeBeadsDir(t *testing.T) {
	abs := filepath.Join(t.TempDir(), ".beads")
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{abs, abs},
		{"heartworks_android/.beads", ""},
		{"gt-/.beads", ""},
	}
	for _, tt := range tests {
		if got := sanitizeBeadsDir(tt.in); got != tt.want {
			t.Errorf("sanitizeBeadsDir(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
