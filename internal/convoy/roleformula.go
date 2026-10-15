package convoy

import (
	"fmt"
	"path/filepath"
	"strings"
)

// roleOnlyFormulas are infrastructure formulas executed by a specific role
// (deacon, dogs, witness, ...), in addition to the mol-*-patrol and mol-dog-*
// families matched by IsRoleFormula.
var roleOnlyFormulas = map[string]bool{
	"mol-session-gc":      true,
	"mol-boot-triage":     true,
	"mol-convoy-cleanup":  true,
	"mol-dep-propagate":   true,
	"mol-orphan-scan":     true,
	"mol-shutdown-dance":  true,
	"mol-town-shutdown":   true,
	"mol-polecat-lease":   true,
	"mol-digest-generate": true,
}

// CanonicalFormulaName reduces a formula name, relative path, or formula file
// path to the lowercase bare name it resolves to, so role-formula checks see the
// same identity the formula loader will.
func CanonicalFormulaName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	name = filepath.Base(filepath.Clean(name))
	for _, ext := range []string{".formula.toml", ".formula.json"} {
		name = strings.TrimSuffix(name, ext)
	}
	return name
}

// IsRoleFormula reports whether name (in any spelling CanonicalFormulaName
// accepts) is a role or infrastructure formula, whose steps only the owning
// role can execute.
func IsRoleFormula(name string) bool {
	name = CanonicalFormulaName(name)
	return roleOnlyFormulas[name] ||
		(strings.HasPrefix(name, "mol-") && strings.HasSuffix(name, "-patrol")) ||
		strings.HasPrefix(name, "mol-dog-")
}

const workflowDescriptionPrefix = "Workflow:"

// WorkflowFormulaName returns the formula a workflow convoy was created from,
// read from the "Workflow: <name>" first line `gt formula run` writes into the
// convoy description, or "" if description has none.
func WorkflowFormulaName(description string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(description), "\n")
	rest, ok := strings.CutPrefix(strings.TrimSpace(first), workflowDescriptionPrefix)
	if !ok {
		return ""
	}
	return strings.TrimSpace(rest)
}

// RoleWorkflowBlock returns why the convoy feeder must not hand any step of a
// convoy with this description to a rig polecat, or "" if it may. The feeder
// sends everything through `gt sling <id> <rig>`, which attaches
// mol-polecat-work; a role formula's steps wrapped in that formula can only
// stall the convoy (gt-p1n2). Ordinary workflow formulas are not blocked: later
// waves are dispatched here as their dependencies close.
func RoleWorkflowBlock(description string) string {
	name := WorkflowFormulaName(description)
	if name == "" || !IsRoleFormula(name) {
		return ""
	}
	return fmt.Sprintf("workflow formula %q is a role formula; its steps run on the owning role", name)
}
