package beads

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// listAgentBeadsQueries runs ListAgentBeads against a stub bd and returns the
// issue-list and sql calls it made. sqlReply is the shell that answers "bd sql".
func listAgentBeadsQueries(t *testing.T, town bool, sqlReply string) (lists, probes []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test uses a shell bd stub")
	}
	binDir := t.TempDir()
	log := filepath.Join(binDir, "calls.log")
	stub := `#!/bin/sh
printf '%s\n' "$*" | tr '\n' ' ' >> "` + log + `"
echo >> "` + log + `"
case " $* " in
  *" mol "*) echo '{"wisps":[]}' ;;
  *" sql "*) ` + sqlReply + ` ;;
  *" list "*) echo '[{"id":"hq-mayor","status":"open","issue_type":"task","labels":["gt:agent"]}]' ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(stub), 0o700); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "mayor"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "mayor", "town.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	dir := root
	if !town {
		dir = filepath.Join(root, "demo")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	agents, err := NewIsolated(dir).ListAgentBeads()
	if err != nil {
		t.Fatalf("ListAgentBeads: %v", err)
	}
	if agents["hq-mayor"] == nil {
		t.Fatalf("agents = %v, want hq-mayor", agents)
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	for _, call := range strings.Split(string(data), "\n") {
		fields := " " + call + " "
		switch {
		case strings.Contains(fields, " mol "):
		case strings.Contains(fields, " sql "):
			probes = append(probes, call)
		case strings.Contains(fields, " list "):
			lists = append(lists, call)
		}
	}
	return lists, probes
}

// --include-infra makes bd search every wisp, which is slow on the town
// database, so the town query passes it only when it would add records.
func TestListAgentBeadsTownQueryShape(t *testing.T) {
	cases := []struct {
		name      string
		sqlReply  string
		wantInfra bool
	}{
		{"nothing hidden from a plain list", `echo '[{"hidden":0}]'`, false},
		{"records only the flag reaches", `echo '[{"hidden":2}]'`, true},
		{"probe fails", `echo boom >&2; exit 1`, true},
		{"probe returns no rows", `echo '[]'`, true},
		{"probe returns no count", `echo '[{}]'`, true},
		{"probe returns text", `echo 'No results'`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lists, probes := listAgentBeadsQueries(t, true, tc.sqlReply)
			if len(probes) != 1 {
				t.Fatalf("sql probes = %q, want one", probes)
			}
			if len(lists) != 1 {
				t.Fatalf("list calls = %q, want one", lists)
			}
			if got := strings.Contains(lists[0], "--include-infra"); got != tc.wantInfra {
				t.Errorf("--include-infra = %v, want %v: %s", got, tc.wantInfra, lists[0])
			}
			for _, flag := range []string{"--label=gt:agent", "--limit=0"} {
				if !strings.Contains(lists[0], flag) {
					t.Errorf("list call lacks %s: %s", flag, lists[0])
				}
			}
		})
	}
}

// Rig databases hold type=agent records, so their query keeps the flag.
func TestListAgentBeadsRigQueryKeepsIncludeInfra(t *testing.T) {
	lists, probes := listAgentBeadsQueries(t, false, `echo '[{"hidden":0}]'`)
	if len(probes) != 0 {
		t.Errorf("sql probes = %q, want none", probes)
	}
	if len(lists) != 1 || !strings.Contains(lists[0], "--include-infra") {
		t.Errorf("list calls = %q, want one with --include-infra", lists)
	}
}
