package beads

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// installRespawnBD puts a fake bd on PATH holding one existing agent bead whose
// labels live in the returned file, one per line. When interrupted is set, a
// --set-labels write fails after removing the old set, as it can in bd, which
// replaces labels one at a time.
func installRespawnBD(t *testing.T, labels []string, interrupted bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd is a shell script")
	}
	dir := t.TempDir()
	store := filepath.Join(dir, "labels")
	if err := os.WriteFile(store, []byte(strings.Join(labels, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fail := ""
	if interrupted {
		fail = "exit 1"
	}
	script := `#!/bin/sh
store='` + store + `'
while [ "${1#--}" != "$1" ]; do shift; done
case "$1" in
create) exit 1 ;;
show)
  list=$(sed 's/.*/"&"/' "$store" | paste -sd, -)
  echo '[{"id":"gt-gastown-polecat-rust","status":"open","labels":['"$list"']}]' ;;
update)
  replaced=
  for arg in "$@"; do
    value=${arg#*=}
    case "$arg" in
    --set-labels=*)
      [ -n "$replaced" ] || : > "$store"
      replaced=1
      ` + fail + `
      echo "$value" >> "$store" ;;
    --add-label=*)
      grep -qxF -- "$value" "$store" || echo "$value" >> "$store" ;;
    --remove-label=*)
      grep -vxF -- "$value" "$store" > "$store.tmp"
      mv "$store.tmp" "$store" ;;
    esac
  done ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return store
}

func respawnAgentBead(t *testing.T, store string) ([]string, error) {
	t.Helper()
	work := t.TempDir()
	bd := NewWithBeadsDir(work, filepath.Join(work, ".beads"))
	_, err := bd.CreateOrReopenAgentBead("gt-gastown-polecat-rust", "gt-gastown-polecat-rust", &AgentFields{
		RoleType:   "polecat",
		Rig:        "gastown",
		AgentState: "spawning",
	})
	data, readErr := os.ReadFile(store)
	if readErr != nil {
		t.Fatal(readErr)
	}
	got := strings.Fields(string(data))
	slices.Sort(got)
	return got, err
}

func TestRespawnKeepsGtAgentWhenLabelReplaceIsInterrupted(t *testing.T) {
	store := installRespawnBD(t, []string{"gt:agent", "idle:3"}, true)

	got, _ := respawnAgentBead(t, store)
	if !slices.Contains(got, "gt:agent") {
		t.Errorf("gt:agent lost: %v", got)
	}
}

// Respawn clears the previous session's state labels and leaves the rest.
func TestRespawnRemovesOnlySessionStateLabels(t *testing.T) {
	store := installRespawnBD(t, []string{
		"backoff-until:100",
		"done-cp:pushed:branch:100",
		"done-intent:COMPLETED:100",
		"gt:other",
		"heartbeat:100",
		"idle:3",
	}, false)

	got, err := respawnAgentBead(t, store)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"gt:agent", "gt:other"}; !slices.Equal(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
}
