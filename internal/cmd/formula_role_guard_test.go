package cmd

import (
	"strings"
	"testing"
)

// `gt formula run` fans a workflow's steps out to rig polecats under
// mol-polecat-work. Role and infrastructure formulas must be refused with an
// error that points at the command that actually runs them (gt-p1n2).
func TestRoleFormulaRefusal(t *testing.T) {
	refused := []string{
		"mol-deacon-patrol",
		"mol-witness-patrol",
		"mol-refinery-patrol",
		"mol-pr-feedback-patrol",
		"mol-dog-reaper",
		"mol-dog-backup",
		"mol-dog-doctor",
		"mol-session-gc",
		"mol-boot-triage",
		"mol-convoy-cleanup",
		"mol-dep-propagate",
		"mol-orphan-scan",
		"mol-shutdown-dance",
		"mol-town-shutdown",
		"mol-polecat-lease",
		"mol-digest-generate",
	}
	for _, name := range refused {
		t.Run("refuses "+name, func(t *testing.T) {
			err := roleFormulaRefusal(name)
			if err == nil {
				t.Fatalf("roleFormulaRefusal(%q) = nil, want a refusal", name)
			}
			if !strings.Contains(err.Error(), "gt sling "+name) {
				t.Errorf("refusal for %q does not name the right command: %v", name, err)
			}
		})
	}

	allowed := []string{
		"code-review",
		"security-audit",
		"design",
		"rule-of-five",
		"mol-polecat-work",
		"mol-polecat-code-review",
		"mol-idea-to-plan",
		"mol-plan-review",
		"mol-prd-review",
		"mol-gastown-boot",
		"mol-sync-workspace",
		"mol-convoy-feed",
		"my-custom-formula",
	}
	for _, name := range allowed {
		t.Run("allows "+name, func(t *testing.T) {
			if err := roleFormulaRefusal(name); err != nil {
				t.Errorf("roleFormulaRefusal(%q) = %v, want nil", name, err)
			}
		})
	}
}
