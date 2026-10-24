package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// mockBdForRoleWorkflowTest installs a fake bd with two open convoys: hq-wf-role,
// a role-formula workflow convoy, and hq-cv-plain, an ordinary one. Each tracks
// one gt- bead whose status is trackedStatus. closeLog records bd close calls.
func mockBdForRoleWorkflowTest(t *testing.T, trackedStatus string) (townRoot, closeLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipping convoy test on Windows")
	}

	binDir := t.TempDir()
	townRoot = t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(`{"prefix":"gt-","path":"gastown/mayor/rig"}`+"\n"), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	closeLog = filepath.Join(binDir, "bd-close.log")

	script := `#!/bin/sh
i=0
for arg in "$@"; do
  case "$arg" in
    --*) ;;
    *) eval "pos$i=\"$arg\""; i=$((i+1)) ;;
  esac
done
STATUS="` + trackedStatus + `"
case "$pos0" in
  list)
    echo '[{"id":"hq-wf-role","title":"Patrol","status":"open","issue_type":"convoy","description":"Workflow: mol-deacon-patrol"},{"id":"hq-cv-plain","title":"Plain","status":"open","issue_type":"convoy","description":"Convoy: fix"}]'
    ;;
  sql)
    case "$*" in
      *"issue_id = 'hq-wf-role'"*) echo '[{"depends_on_id":"gt-role1"}]' ;;
      *"issue_id = 'hq-cv-plain'"*) echo '[{"depends_on_id":"gt-plain1"}]' ;;
      *) echo '[]' ;;
    esac
    ;;
  dep)
    case "$pos2" in
      hq-wf-role) echo '[{"id":"gt-role1","title":"Patrol step","status":"'"$STATUS"'","issue_type":"task","assignee":"","dependency_type":"tracks"}]' ;;
      hq-cv-plain) echo '[{"id":"gt-plain1","title":"Plain task","status":"'"$STATUS"'","issue_type":"task","assignee":"","dependency_type":"tracks"}]' ;;
      *) echo '[]' ;;
    esac
    ;;
  show)
    case "$pos1" in
      hq-wf-role) echo '[{"id":"hq-wf-role","title":"Patrol","status":"open","issue_type":"convoy","description":"Workflow: mol-deacon-patrol"}]' ;;
      hq-cv-plain) echo '[{"id":"hq-cv-plain","title":"Plain","status":"open","issue_type":"convoy","description":"Convoy: fix"}]' ;;
      *) echo '[{"id":"'"$pos1"'","title":"step","status":"'"$STATUS"'","issue_type":"task","assignee":"","blocked_by":[],"blocked_by_count":0,"dependencies":[]}]' ;;
    esac
    ;;
  close)
    echo "$@" >> "` + closeLog + `"
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return townRoot, closeLog
}

func strandedByID(stranded []strandedConvoyInfo) map[string]strandedConvoyInfo {
	m := map[string]strandedConvoyInfo{}
	for _, s := range stranded {
		m[s.ID] = s
	}
	return m
}

// A role-formula workflow convoy stays in the stranded output, marked not
// feedable, so the daemon's completion sweep still sees it; only its steps are
// withheld from the feeder.
func TestFindStrandedConvoys_RoleWorkflowConvoyKeptAsNotFeedable(t *testing.T) {
	townRoot, _ := mockBdForRoleWorkflowTest(t, "open")

	stranded, err := findStrandedConvoys(townRoot)
	if err != nil {
		t.Fatalf("findStrandedConvoys: %v", err)
	}
	byID := strandedByID(stranded)

	role, ok := byID["hq-wf-role"]
	if !ok {
		t.Fatalf("role-workflow convoy missing from stranded output: %+v", stranded)
	}
	if role.Feedable || role.ReadyCount != 0 || len(role.ReadyIssues) != 0 {
		t.Errorf("role convoy = %+v, want feedable=false, no ready issues", role)
	}
	if role.TrackedCount != 1 {
		t.Errorf("role convoy TrackedCount = %d, want 1", role.TrackedCount)
	}
	if role.NotFeedableReason == "" {
		t.Error("role convoy has no not_feedable_reason")
	}

	plain, ok := byID["hq-cv-plain"]
	if !ok {
		t.Fatal("ordinary convoy missing from stranded output")
	}
	if !plain.Feedable || plain.ReadyCount != 1 {
		t.Errorf("ordinary convoy = %+v, want feedable=true, 1 ready", plain)
	}

	out, err := json.Marshal(stranded)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"feedable":false`) || !strings.Contains(string(out), `"feedable":true`) {
		t.Errorf("JSON %s lacks feedable true/false markers", out)
	}
}

// The completion sweep closes a role-workflow convoy whose steps are all closed,
// and the stranded scan never offers its steps for feeding.
func TestRoleWorkflowConvoy_CompletedByTheSweepAndNeverFed(t *testing.T) {
	townRoot, closeLog := mockBdForRoleWorkflowTest(t, "closed")
	recordConvoyGT(t) // the sweep closes a convoy, which notifies

	stranded, err := findStrandedConvoys(townRoot)
	if err != nil {
		t.Fatalf("findStrandedConvoys: %v", err)
	}
	role, ok := strandedByID(stranded)["hq-wf-role"]
	if !ok {
		t.Fatalf("all-closed role convoy missing from stranded output, so the sweep would never see it: %+v", stranded)
	}
	if role.Feedable || role.ReadyCount != 0 || role.TrackedCount != 1 {
		t.Errorf("role convoy = %+v, want feedable=false, 0 ready, 1 tracked (the sweep's completion shape)", role)
	}

	if err := checkSingleConvoy(townRoot, "hq-wf-role", false); err != nil {
		t.Fatalf("checkSingleConvoy: %v", err)
	}
	data, err := os.ReadFile(closeLog)
	if err != nil {
		t.Fatalf("bd close was never called for the completed role convoy: %v", err)
	}
	if !strings.Contains(string(data), "hq-wf-role") {
		t.Errorf("bd close log %q does not name hq-wf-role", data)
	}
}
