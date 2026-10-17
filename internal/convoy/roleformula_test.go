package convoy

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// embeddedFormulaRoles classifies every formula shipped in internal/formula/formulas:
// true if a non-polecat role (deacon, dogs, witness, refinery, boot, mayor) runs
// it, so the convoy feeder must never hand its steps to a polecat; false if a
// polecat may run it. A new formula must be added here, which forces the
// question of who runs it.
var embeddedFormulaRoles = map[string]bool{
	// Role formulas.
	"mol-boot-triage":        true, // Boot, the daemon's Deacon watchdog
	"mol-convoy-cleanup":     true, // dogs
	"mol-convoy-feed":        true, // dogs, when the Deacon finds a stranded convoy
	"mol-deacon-patrol":      true, // Deacon
	"mol-dep-propagate":      true, // dogs
	"mol-digest-generate":    true, // dogs
	"mol-dog-backup":         true,
	"mol-dog-checkpoint":     true,
	"mol-dog-compactor":      true,
	"mol-dog-doctor":         true,
	"mol-dog-jsonl":          true,
	"mol-dog-phantom-db":     true,
	"mol-dog-reaper":         true,
	"mol-dog-stale-db":       true,
	"mol-gastown-boot":       true, // the Mayor's boot-up lifecycle
	"mol-orphan-scan":        true, // dogs
	"mol-polecat-lease":      true, // the Witness's per-polecat lease
	"mol-pr-feedback-patrol": true, // a standing patrol; it slings polecats but is not run by one
	"mol-refinery-patrol":    true, // Refinery
	"mol-session-gc":         true, // dogs
	"mol-shutdown-dance":     true, // dogs
	"mol-town-shutdown":      true,
	"mol-witness-patrol":     true, // Witness

	// Polecat-runnable formulas.
	"beads-release":                 false, // the toml offers "assign to a polecat"
	"code-review":                   false,
	"design":                        false,
	"gastown-release":               false, // crew or polecat
	"mol-plan-review":               false,
	"mol-polecat-code-review":       false,
	"mol-polecat-conflict-resolve":  false,
	"mol-polecat-review-pr":         false,
	"mol-polecat-work":              false,
	"mol-polecat-work-monorepo":     false,
	"mol-polecat-work-monorepo-tdd": false,
	"mol-prd-review":                false,
	"rule-of-five":                  false,
	"security-audit":                false,
	"shiny":                         false,
	"shiny-enterprise":              false,
	"shiny-secure":                  false,
	"tdd-cycle":                     false,
	"towers-of-hanoi":               false,
	"towers-of-hanoi-10":            false,
	"towers-of-hanoi-7":             false,
	"towers-of-hanoi-9":             false,

	// Owner unclear; classified polecat-runnable, not guessed.
	// TODO: mol-idea-to-plan is run by an orchestrating crew worker, which is
	// not a polecat; confirm whether its steps may be fed to polecats.
	"mol-idea-to-plan": false,
	// TODO: mol-sync-workspace is broadcast to every agent, the refinery
	// included; confirm whether it is a role formula.
	"mol-sync-workspace": false,
}

func TestIsRoleFormula_ClassifiesEveryEmbeddedFormula(t *testing.T) {
	dir := filepath.Join("..", "formula", "formulas")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read embedded formulas dir: %v", err)
	}

	seen := map[string]bool{}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".formula.toml")
		if !ok {
			continue
		}
		seen[name] = true
		wantRole, classified := embeddedFormulaRoles[name]
		if !classified {
			t.Errorf("formula %q is not in embeddedFormulaRoles: decide who runs it and add it", name)
			continue
		}
		if got := IsRoleFormula(name); got != wantRole {
			t.Errorf("IsRoleFormula(%q) = %v, want %v", name, got, wantRole)
		}
	}

	var stale []string
	for name := range embeddedFormulaRoles {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("embeddedFormulaRoles names formulas that are not embedded: %v", stale)
	}
}
