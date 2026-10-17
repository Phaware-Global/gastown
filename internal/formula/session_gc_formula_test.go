package formula

import (
	"strings"
	"testing"
)

// TestSessionGCFormulaNeverDeletesOpenWisps pins the mol-session-gc contract:
// it must not promise age-based deletion of wisps (the formula text previously
// described "Wisps > 1 hour old" as a cleanup target, which is what gt doctor
// --fix then did to every open merge-request wisp in town).
func TestSessionGCFormulaNeverDeletesOpenWisps(t *testing.T) {
	content, err := formulasFS.ReadFile("formulas/mol-session-gc.formula.toml")
	if err != nil {
		t.Fatalf("read formula: %v", err)
	}
	text := string(content)

	for _, banned := range []string{
		"Wisps > 24 hours old",
		"Wisps > 1 hour old",
		"Never delete wisps < 1 hour old",
	} {
		if strings.Contains(text, banned) {
			t.Errorf("mol-session-gc still describes age-based wisp deletion: %q", banned)
		}
	}
	if !strings.Contains(text, "Never delete a non-closed wisp") {
		t.Error("mol-session-gc must state that non-closed wisps are never deleted")
	}
}
