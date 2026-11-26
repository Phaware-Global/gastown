package mail

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// agentQueryTown makes a town with one rig route and a stub bd that logs the
// directory and arguments of each call, then prints listJSON for `bd list`.
func agentQueryTown(t *testing.T, listJSON string) (townRoot, rigRoot, callLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test uses a shell bd stub")
	}
	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	rigRoot = filepath.Join(townRoot, "demo")
	for _, dir := range []string{filepath.Join(townRoot, ".beads"), filepath.Join(rigRoot, ".beads")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	// Both an hq- route and a second route that also resolves to the town.
	routes := `{"prefix":"hq-","path":"."}
{"prefix":"town-","path":"."}
{"prefix":"de-","path":"demo"}
`
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0o600); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	binDir := t.TempDir()
	callLog = filepath.Join(binDir, "calls.log")
	fixture := filepath.Join(binDir, "list.json")
	if err := os.WriteFile(fixture, []byte(listJSON), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	stub := `#!/bin/sh
printf '%s|%s\n' "$(pwd -P)" "$*" >> "$BD_CALL_LOG"
case "$1" in
  list) cat "$BD_LIST_FIXTURE" ;;
  *) echo '[]' ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(stub), 0o700); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_CALL_LOG", callLog)
	t.Setenv("BD_LIST_FIXTURE", fixture)
	return townRoot, rigRoot, callLog
}

// bd list hides type=agent records unless --include-infra is passed. Rig
// queries need them; on the town database the flag is too slow for a query
// that runs on every send.
func TestAgentQueriesIncludeInfraOnlyForRigs(t *testing.T) {
	townRoot, rigRoot, callLog := agentQueryTown(t, `[]`)

	// An unknown recipient walks every agent query on the send path.
	_ = NewRouterWithTownRoot(townRoot, townRoot).validateRecipient("demo/nobody")

	data, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	calls := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		dir, args, _ := strings.Cut(line, "|")
		fields := strings.Fields(args)
		if len(fields) == 0 || fields[0] != "list" {
			continue
		}
		calls[dir]++
		hasFlag := slices.Contains(fields, "--include-infra")
		switch dir {
		case townRoot:
			if hasFlag {
				t.Errorf("town query passes --include-infra: %s", args)
			}
		case rigRoot:
			if !hasFlag {
				t.Errorf("rig query lacks --include-infra: %s", args)
			}
		default:
			t.Errorf("unexpected query directory %s: %s", dir, args)
		}
	}
	if calls[townRoot] == 0 || calls[rigRoot] == 0 {
		t.Fatalf("expected both town and rig queries, got %v", calls)
	}
}

func TestRigGroupResolutionMatchesRigExactly(t *testing.T) {
	townRoot, _, _ := agentQueryTown(t, `[
{"id":"gt-demo-witness","description":"role_type: witness\nrig: demo\n","status":"open","issue_type":"agent","labels":["gt:agent"]},
{"id":"gt-demo_ios-witness","description":"role_type: witness\nrig: demo_ios\n","status":"open","issue_type":"agent","labels":["gt:agent"]}]`)
	r := NewRouterWithTownRoot(townRoot, townRoot)

	resolvers := map[string]func() ([]string, error){
		"by rig":  func() ([]string, error) { return r.resolveAgentsByRig("demo") },
		"by role": func() ([]string, error) { return r.resolveAgentsByRole("witness", "demo") },
	}
	for name, resolve := range resolvers {
		t.Run(name, func(t *testing.T) {
			got, err := resolve()
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if want := []string{"demo/witness"}; !slices.Equal(got, want) {
				t.Errorf("addresses = %v, want %v", got, want)
			}
		})
	}
}
