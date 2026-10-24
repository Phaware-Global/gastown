package beads

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	bdExecLine      = regexp.MustCompile(`(\w+)\s*:?=\s*exec\.Command(?:Context)?\((?:[\w.]+,\s*)?"bd"`)
	bdChainLine     = regexp.MustCompile(`BdCmd\(`)
	rawStderrAssign = regexp.MustCompile(`^\s*(\w+)\.Stderr\s*=\s*os\.Stderr\b`)
	stderrString    = regexp.MustCompile(`\b[sS]tderr\.String\(\)`)
)

// How far back from a use to look for the bd command it belongs to.
const bdLookback = 30

// bd tells whoever reads its stderr to run "bd dolt start", which binds the
// shared Dolt port to an empty store (see DoltStartGuidance). Every place gt
// relays bd's stderr must go through the rewrite, so a new call site that skips
// it fails here instead of reaching an agent.
func TestBdStderrIsNeverRelayedRaw(t *testing.T) {
	root := ".." // internal/
	var violations []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			switch {
			case rawStderrAssign.MatchString(line):
				v := rawStderrAssign.FindStringSubmatch(line)[1]
				if assignedFromBd(lines, i, v) {
					violations = append(violations, loc(path, i, line, "bd stderr sent straight to os.Stderr: use RunRelayed/OutputRelayed"))
				}
			case strings.Contains(line, ".Stderr(os.Stderr)") && chainsBd(lines, i):
				violations = append(violations, loc(path, i, line, "BdCmd stderr sent straight to os.Stderr: leave the default"))
			case stderrString.MatchString(line):
				if !strings.Contains(line, "RedirectDoltStartHint") && !stderrLineExempt(line) && nearBd(lines, i) {
					violations = append(violations, loc(path, i, line, "bd stderr formatted raw: wrap in RedirectDoltStartHint"))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Errorf("bd stderr reaches the reader without the bd-dolt-start rewrite:\n%s", strings.Join(violations, "\n"))
	}
}

// stderrLineExempt covers uses that never show the text to anyone: substring
// probes, and wrapError, which applies the rewrite itself.
func stderrLineExempt(line string) bool {
	return strings.Contains(line, ".Contains(") ||
		strings.Contains(line, "wrapError(") ||
		strings.Contains(line, "RecordBDCall(")
}

func assignedFromBd(lines []string, at int, v string) bool {
	for j := at - 1; j >= 0 && j >= at-bdLookback; j-- {
		if m := bdExecLine.FindStringSubmatch(lines[j]); m != nil && m[1] == v {
			return true
		}
	}
	return false
}

func chainsBd(lines []string, at int) bool {
	for j := at; j >= 0 && j >= at-3; j-- {
		if bdChainLine.MatchString(lines[j]) {
			return true
		}
	}
	return false
}

func nearBd(lines []string, at int) bool {
	for j := at; j >= 0 && j >= at-bdLookback; j-- {
		if bdExecLine.MatchString(lines[j]) || bdChainLine.MatchString(lines[j]) {
			return true
		}
	}
	return false
}

func loc(path string, i int, line, why string) string {
	return fmt.Sprintf("%s:%d: %s\n\t%s", path, i+1, why, strings.TrimSpace(line))
}
