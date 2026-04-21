package cmd

import "testing"

// TestShouldOverrideConvoyStrategyForPRRig pins the G22 override logic.
// The rule: when the bead says "direct" or "local" but the rig is
// pr-mode, force the pr-path so the work goes through PR review +
// refinery merge instead of silently closing the bead without merging.
//
// Rig mergeStrategy values other than "pr" preserve existing behavior:
// the bead's convoy strategy is honored as-is. Under pr-mode rigs, the
// dispatcher stamping "mr" on beads is fine (the default mr path runs
// pr-aware under G11) — only the two shortcut strategies (direct,
// local) need to be vetoed.
func TestShouldOverrideConvoyStrategyForPRRig(t *testing.T) {
	tests := []struct {
		name         string
		beadStrategy string
		rigStrategy  string
		wantOverride bool
	}{
		// Positive: bead wants the silent-close shortcut under a pr-mode rig.
		{"direct convoy on pr rig — override", "direct", "pr", true},
		{"local convoy on pr rig — override", "local", "pr", true},

		// Negative: bead's mr/empty strategy on pr rig is fine — the default
		// mr path handles pr-mode correctly via G11.
		{"mr convoy on pr rig — no override", "mr", "pr", false},
		{"empty convoy strategy on pr rig — no override", "", "pr", false},

		// Negative: when the rig is NOT pr-mode, the bead's strategy is
		// honored regardless. This preserves mr/direct rig behavior.
		{"direct convoy on mr rig — no override", "direct", "mr", false},
		{"direct convoy on direct rig — no override", "direct", "direct", false},
		{"local convoy on mr rig — no override", "local", "mr", false},
		{"local convoy on direct rig — no override", "local", "direct", false},
		{"mr convoy on mr rig — no override", "mr", "mr", false},

		// Negative: empty rig strategy defaults to mr behavior; not pr.
		{"direct convoy on empty-strategy rig — no override", "direct", "", false},

		// Negative: unknown rig strategy doesn't trigger the override.
		{"direct convoy on unknown-strategy rig — no override", "direct", "whatever", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldForcePRPath(tc.beadStrategy, tc.rigStrategy)
			if got != tc.wantOverride {
				t.Errorf("shouldForcePRPath(%q, %q) = %v; want %v",
					tc.beadStrategy, tc.rigStrategy, got, tc.wantOverride)
			}
		})
	}
}
