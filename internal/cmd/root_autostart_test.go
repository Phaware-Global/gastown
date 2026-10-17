package cmd

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// A raw exec.Command child must see BEADS_DOLT_AUTO_START=0 even when gt was
// launched with the variable unset or set to something permissive.
func TestPinBdNoAutoStart_ChildrenInherit(t *testing.T) {
	for _, initial := range []string{"", "1", "true"} {
		t.Run("initial="+initial, func(t *testing.T) {
			if initial == "" {
				t.Setenv("BEADS_DOLT_AUTO_START", "x")
				os.Unsetenv("BEADS_DOLT_AUTO_START")
			} else {
				t.Setenv("BEADS_DOLT_AUTO_START", initial)
			}
			pinBdNoAutoStart()
			out, err := exec.Command("sh", "-c", `printf %s "$BEADS_DOLT_AUTO_START"`).Output()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(out)); got != "0" {
				t.Fatalf("child saw BEADS_DOLT_AUTO_START=%q, want 0", got)
			}
		})
	}
}

// The pin must happen on the real entry point: a test that calls
// pinBdNoAutoStart directly keeps passing if Execute() stops calling it.
func TestExecute_PinsBdNoAutoStartForChildren(t *testing.T) {
	for _, initial := range []string{"", "1", "true"} {
		t.Run("initial="+initial, func(t *testing.T) {
			if initial == "" {
				t.Setenv("BEADS_DOLT_AUTO_START", "x")
				os.Unsetenv("BEADS_DOLT_AUTO_START")
			} else {
				t.Setenv("BEADS_DOLT_AUTO_START", initial)
			}
			// persistentPreRun exits on an unsigned dev build; a test binary is one.
			prevBuilt := BuiltProperly
			BuiltProperly = "1"
			rootCmd.SetArgs([]string{"version"})
			t.Cleanup(func() {
				BuiltProperly = prevBuilt
				rootCmd.SetArgs(nil)
			})

			if code := Execute(); code != 0 {
				t.Fatalf("Execute() = %d, want 0", code)
			}
			out, err := exec.Command("sh", "-c", `printf %s "$BEADS_DOLT_AUTO_START"`).Output()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(out)); got != "0" {
				t.Fatalf("child of Execute() saw BEADS_DOLT_AUTO_START=%q, want 0", got)
			}
		})
	}
}
