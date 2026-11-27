package formula

import (
	"strings"
	"testing"
)

// Unbounded full-suite runs exhaust the host's CPU and disk.
func TestPolecatFormulasBoundFullSuiteTestRuns(t *testing.T) {
	for _, name := range []string{
		"mol-polecat-work",
		"mol-polecat-work-monorepo",
		"mol-polecat-conflict-resolve",
	} {
		data, err := formulasFS.ReadFile("formulas/" + name + ".formula.toml")
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		text := string(data)
		if !strings.Contains(text, "go test -p 2 ./...") {
			t.Errorf("%s does not tell the polecat to run go test -p 2", name)
		}
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "go test ./...") {
				t.Errorf("%s has an unbounded full-suite command: %q", name, line)
			}
		}
	}
}
