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
		"mol-convoy-feed",
		"mol-gastown-boot",
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

	// The argument is joined into a path, so spellings that findFormulaFile
	// resolves to a role formula must be refused too.
	aliased := []string{
		"./mol-deacon-patrol",
		"x/../mol-dog-reaper",
		"Mol-Deacon-Patrol",
		"MOL-DOG-REAPER",
		"mol-deacon-patrol.formula.toml",
		"/town/.beads/formulas/mol-witness-patrol.formula.toml",
	}
	for _, name := range aliased {
		t.Run("refuses alias "+name, func(t *testing.T) {
			if err := roleFormulaRefusal(name); err == nil {
				t.Fatalf("roleFormulaRefusal(%q) = nil, want a refusal", name)
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
		"mol-sync-workspace",
		"my-custom-formula",
		"./my-custom-formula",
	}
	for _, name := range allowed {
		t.Run("allows "+name, func(t *testing.T) {
			if err := roleFormulaRefusal(name); err != nil {
				t.Errorf("roleFormulaRefusal(%q) = %v, want nil", name, err)
			}
		})
	}
}
