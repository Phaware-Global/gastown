package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The stranded scan runs every 30s in the daemon, so its bd call count must not
// grow with convoys × rigs: sling contexts are read once per scan, and the
// per-rig agent-bead lists (worker display info the scan never uses) are not
// read at all.
func TestFindStrandedConvoys_ReadsSlingContextsOncePerScan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping convoy test on Windows")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	// EvalSymlinks: macOS temp dirs are symlinked, and the scan resolves the town from cwd.
	townRoot, err := filepath.EvalSymlinks(townRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{
		"mayor",
		".beads",
		"rig1/polecats", "rig1/mayor/rig/.beads",
		"rig2/polecats", "rig2/mayor/rig/.beads",
	} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test"}`), 0644); err != nil {
		t.Fatal(err)
	}
	routes := `{"prefix":"r1-","path":"rig1/mayor/rig"}` + "\n" + `{"prefix":"r2-","path":"rig2/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(binDir, "bd.log")

	// Three open convoys, each tracking one open, unassigned bead.
	script := `#!/bin/sh
echo "$*" >> "` + logPath + `"
i=0
for arg in "$@"; do
  case "$arg" in
    --*) ;;
    *) eval "pos$i=\"$arg\""; i=$((i+1)) ;;
  esac
done
case "$pos0" in
  list)
    case "$*" in
      *gt:sling-context*|*gt:agent*) echo '[]' ;;
      *) echo '[{"id":"hq-cv-a","title":"A","status":"open","issue_type":"convoy"},{"id":"hq-cv-b","title":"B","status":"open","issue_type":"convoy"},{"id":"hq-cv-c","title":"C","status":"open","issue_type":"convoy"}]' ;;
    esac
    ;;
  sql)
    case "$*" in
      *"issue_id = 'hq-cv-a'"*) echo '[{"depends_on_id":"r1-a1"}]' ;;
      *"issue_id = 'hq-cv-b'"*) echo '[{"depends_on_id":"r2-b1"}]' ;;
      *"issue_id = 'hq-cv-c'"*) echo '[{"depends_on_id":"r1-c1"}]' ;;
      *) echo '[]' ;;
    esac
    ;;
  show)
    echo '[{"id":"'"$pos1"'","title":"task","status":"open","issue_type":"task","assignee":"","blocked_by":[],"blocked_by_count":0,"dependencies":[]}]'
    ;;
  *)
    echo '[]'
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(townRoot)

	stranded, err := findStrandedConvoys(townRoot)
	if err != nil {
		t.Fatalf("findStrandedConvoys: %v", err)
	}
	if len(stranded) != 3 {
		t.Fatalf("got %d stranded convoys, want 3: %+v", len(stranded), stranded)
	}
	for _, s := range stranded {
		if !s.Feedable || s.ReadyCount != 1 {
			t.Errorf("convoy %s = %+v, want feedable with 1 ready issue", s.ID, s)
		}
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading bd.log: %v", err)
	}
	var slingLists, agentLists int
	for _, line := range strings.Split(string(logData), "\n") {
		// The wisp query for sling contexts also names the label; count bd list only.
		if strings.Contains(line, "list --label=gt:sling-context") {
			slingLists++
		}
		if strings.Contains(line, "list --label=gt:agent") {
			agentLists++
		}
	}
	// One list per beads dir (town + two rigs), not one per dir per convoy.
	if want := len(beadsSearchDirs(townRoot)); slingLists != want {
		t.Errorf("sling-context lists = %d, want %d (one per beads dir for the whole scan)\n%s", slingLists, want, logData)
	}
	if agentLists != 0 {
		t.Errorf("agent-bead lists = %d, want 0: the scan does not use worker info\n%s", agentLists, logData)
	}
}

// A rig whose sling contexts cannot be read is not a rig with no contexts: its
// beads are held as scheduled, while beads in readable rigs are unaffected.
func TestScheduledSnapshot_UnreadableRigFailsClosedForItsBeadsOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping scheduler test on Windows")
	}

	binDir := t.TempDir()
	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"mayor", ".beads", "rig1/mayor/rig/.beads", "rig2/mayor/rig/.beads"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test"}`), 0644); err != nil {
		t.Fatal(err)
	}
	routes := `{"prefix":"hq-","path":"."}` + "\n" + `{"prefix":"r1-","path":"rig1/mayor/rig"}` + "\n" + `{"prefix":"r2-","path":"rig2/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatal(err)
	}

	// bd fails for every call made from rig2's beads dir and returns no
	// contexts elsewhere.
	script := `#!/bin/sh
case "$PWD" in
  */rig2/*) echo "database unavailable" >&2; exit 1 ;;
esac
echo '[]'
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(townRoot)

	got := areScheduled([]string{"r1-a", "r2-b", "hq-c"})
	if !got["r2-b"] {
		t.Errorf("r2-b not treated as scheduled although rig2's sling contexts were unreadable: %v", got)
	}
	if got["r1-a"] || got["hq-c"] {
		t.Errorf("beads outside the unreadable rig treated as scheduled: %v", got)
	}
}
