package formula

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Agent beads as `bd list --json` prints them.
const (
	// structured: type=agent, role recorded in both description and metadata.
	structuredAgentBead = `{"id":"gt-demo-%ROLE%","title":"%ROLE% for demo","description":"%ROLE% for demo\n\nrole_type: %ROLE%\nrig: demo\n","status":"open","priority":2,"issue_type":"agent","metadata":{"rig":"demo","role_type":"%ROLE%"},"labels":["gt:agent"],"no_history":true,"dependency_count":0,"dependent_count":0,"comment_count":0}`
	// description-only: type=task, no metadata key at all.
	descriptionAgentBead = `{"id":"gt-desc-%ROLE%","title":"demo/%ROLE% agent","description":"role_type: %ROLE%\nrig: demo\n","status":"open","priority":4,"issue_type":"task","labels":["gt:agent","idle:5"],"dependency_count":0,"dependent_count":0,"comment_count":0}`
	// metadata-only: the role lives in metadata and nowhere in the description.
	metadataAgentBead = `{"id":"gt-meta-%ROLE%","title":"%ROLE% for demo","description":"%ROLE% for demo - monitors the rig","status":"open","priority":2,"issue_type":"agent","metadata":{"rig":"demo","role_type":"%ROLE%"},"labels":["gt:agent"],"dependency_count":0,"dependent_count":0,"comment_count":0}`
	// unrelated beads that must never match: another rig (one whose name
	// extends ours), another role, and a closed bead.
	unrelatedAgentBeads = `{"id":"gt-other-%ROLE%","description":"role_type: %ROLE%\nrig: demo_ios\n","status":"open","issue_type":"agent","metadata":{"rig":"demo_ios","role_type":"%ROLE%"},"labels":["gt:agent"]},` +
		`{"id":"gt-demo-polecat-nux","description":"role_type: polecat\nrig: demo\n","status":"open","issue_type":"agent","metadata":{"rig":"demo","role_type":"polecat"},"labels":["gt:agent"]},` +
		`{"id":"gt-old-%ROLE%","description":"role_type: %ROLE%\nrig: demo\n","status":"closed","issue_type":"agent","metadata":null,"labels":["gt:agent"]}`
)

// agentLookupCommand returns the agent-bead lookup shell line from a patrol
// formula, with the rig placeholder filled in.
func agentLookupCommand(t *testing.T, formulaName string) string {
	t.Helper()
	content, err := formulasFS.ReadFile("formulas/" + formulaName)
	if err != nil {
		t.Fatalf("reading %s: %v", formulaName, err)
	}
	f, err := Parse(content)
	if err != nil {
		t.Fatalf("parsing %s: %v", formulaName, err)
	}
	for _, step := range f.Steps {
		for _, line := range strings.Split(step.Description, "\n") {
			if strings.HasPrefix(line, "bd list --label=gt:agent") {
				return strings.NewReplacer("<YOUR_RIG>", "demo", "{{rig}}", "demo").Replace(line)
			}
		}
	}
	t.Fatalf("%s: no agent-bead lookup found", formulaName)
	return ""
}

// runAgentLookup runs a lookup line against a stub bd that lists the given beads.
func runAgentLookup(t *testing.T, command, beadsJSON string) (stdout, stderr string, err error) {
	t.Helper()
	dir := t.TempDir()
	fixture := filepath.Join(dir, "beads.json")
	if err := os.WriteFile(fixture, []byte(beadsJSON), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	// Like bd, the stub caps results unless --limit=0 and hides type=agent
	// records unless --include-infra.
	stub := `#!/bin/sh
case " $* " in *" --include-infra "*) f=. ;; *) f='map(select(.issue_type != "agent"))' ;; esac
case " $* " in *" --limit=0 "*) ;; *) f="$f | .[:50]" ;; esac
exec jq "$f" "$BEADS_FIXTURE"
`
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte(stub), 0o700); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BEADS_FIXTURE="+fixture)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return strings.TrimSpace(out.String()), errOut.String(), err
}

func TestPatrolAgentBeadLookupResolution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("lookup is a POSIX shell pipeline")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}

	patrols := map[string]string{
		"refinery": "mol-refinery-patrol.formula.toml",
		"witness":  "mol-witness-patrol.formula.toml",
	}
	for role, formulaName := range patrols {
		t.Run(role, func(t *testing.T) {
			command := agentLookupCommand(t, formulaName)
			fill := func(beads ...string) string {
				return strings.ReplaceAll("["+strings.Join(beads, ",")+"]", "%ROLE%", role)
			}

			single := []struct{ name, bead, wantID string }{
				{"structured", structuredAgentBead, "gt-demo-" + role},
				{"description only", descriptionAgentBead, "gt-desc-" + role},
				{"metadata only", metadataAgentBead, "gt-meta-" + role},
			}
			for _, tc := range single {
				t.Run(tc.name, func(t *testing.T) {
					stdout, stderr, err := runAgentLookup(t, command, fill(unrelatedAgentBeads, tc.bead))
					if err != nil {
						t.Fatalf("lookup failed: %v\n%s", err, stderr)
					}
					if stdout != tc.wantID {
						t.Errorf("lookup = %q, want %q", stdout, tc.wantID)
					}
				})
			}

			t.Run("past the default list limit", func(t *testing.T) {
				filler := `{"id":"gt-filler","description":"","status":"open","issue_type":"agent","labels":["gt:agent"]}`
				beads := strings.Repeat(filler+",", 60) + structuredAgentBead
				stdout, stderr, err := runAgentLookup(t, command, fill(beads))
				if err != nil || stdout != "gt-demo-"+role {
					t.Errorf("lookup = %q, err = %v, want gt-demo-%s\n%s", stdout, err, role, stderr)
				}
			})

			t.Run("ambiguous", func(t *testing.T) {
				stdout, stderr, err := runAgentLookup(t, command, fill(metadataAgentBead, descriptionAgentBead))
				if err == nil {
					t.Fatalf("two matching beads must fail the lookup, got %q", stdout)
				}
				if stdout != "" {
					t.Errorf("ambiguous lookup must not print an ID, got %q", stdout)
				}
				for _, id := range []string{"gt-desc-" + role, "gt-meta-" + role} {
					if !strings.Contains(stderr, id) {
						t.Errorf("error does not name %s: %s", id, stderr)
					}
				}
			})

			t.Run("no match", func(t *testing.T) {
				stdout, stderr, err := runAgentLookup(t, command, fill(unrelatedAgentBeads))
				if err == nil {
					t.Fatalf("zero matching beads must fail the lookup, got %q", stdout)
				}
				if !strings.Contains(stderr, "found 0") {
					t.Errorf("error does not report the count: %s", stderr)
				}
			})
		})
	}
}
