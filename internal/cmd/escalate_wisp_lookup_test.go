package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func escalationIDSet(issues []*beads.Issue) map[string]bool {
	ids := make(map[string]bool, len(issues))
	for _, is := range issues {
		ids[is.ID] = true
	}
	return ids
}

// escalate list --all must show wisp escalations, open and closed.
func TestCollectEscalations_AllSeesWispEscalations_RealStore(t *testing.T) {
	bd := setupEscalationTestStore(t)

	open, err := bd.CreateEscalationBead("open escalation", fingerprintedEscalation(""))
	if err != nil {
		t.Fatalf("create open escalation: %v", err)
	}
	closed, err := bd.CreateEscalationBead("closed escalation", fingerprintedEscalation(""))
	if err != nil {
		t.Fatalf("create closed escalation: %v", err)
	}
	if err := bd.CloseEscalation(closed.ID, "test/agent", "resolved"); err != nil {
		t.Fatalf("close escalation: %v", err)
	}

	all, err := collectEscalations(bd, true)
	if err != nil {
		t.Fatalf("collectEscalations(all): %v", err)
	}
	got := escalationIDSet(all)
	if !got[open.ID] || !got[closed.ID] {
		t.Fatalf("--all listed %v, want open %s and closed %s", got, open.ID, closed.ID)
	}

	openOnly, err := collectEscalations(bd, false)
	if err != nil {
		t.Fatalf("collectEscalations(open): %v", err)
	}
	if got := escalationIDSet(openOnly); !got[open.ID] || got[closed.ID] {
		t.Fatalf("open list = %v, want only %s", got, open.ID)
	}
}

// failWispQuery puts a bd wrapper first on PATH that fails every `bd query`,
// which is how the wisp table is read, and passes everything else through.
func failWispQuery(t *testing.T) {
	t.Helper()
	realBd, err := exec.LookPath("bd")
	if err != nil {
		t.Fatalf("bd not on PATH: %v", err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$a\" = query ]; then echo 'simulated wisp query failure' >&2; exit 1; fi\n" +
		"done\n" +
		"exec " + realBd + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatalf("write bd wrapper: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A failed duplicate lookup must not block creating the escalation.
func TestCreateEscalationUnlessDuplicate_LookupFailureFailsOpen_RealStore(t *testing.T) {
	bd := setupEscalationTestStore(t)
	failWispQuery(t)

	var created *beads.Issue
	var existing *beads.Issue
	var err error
	stderr := captureStderr(t, func() {
		created, existing, err = createEscalationUnlessDuplicate(bd, "lookup down", fingerprintedEscalation(escalationFingerprintLabel("test:lookup-down")))
	})
	if err != nil {
		t.Fatalf("escalation must be created when the lookup fails, got error: %v", err)
	}
	if existing != nil || created == nil {
		t.Fatalf("want a created escalation, got created=%v existing=%v", created, existing)
	}
	if !strings.Contains(stderr, "warning") {
		t.Fatalf("stderr = %q, want a warning about the failed duplicate lookup", stderr)
	}
}

// The list path shows what it could read and reports the failed half.
func TestCollectEscalations_PartialLookupWarns_RealStore(t *testing.T) {
	bd := setupEscalationTestStore(t)
	failWispQuery(t)

	var issues []*beads.Issue
	var err error
	stderr := captureStderr(t, func() {
		issues, err = collectEscalations(bd, false)
	})
	if err != nil {
		t.Fatalf("collectEscalations must not fail when only the wisp query fails: %v", err)
	}
	_ = issues
	if !strings.Contains(stderr, "warning") {
		t.Fatalf("stderr = %q, want a warning about the failed wisp lookup", stderr)
	}
}
