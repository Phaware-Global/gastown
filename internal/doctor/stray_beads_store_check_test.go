package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildStrayTestTown creates a minimal town with one rig ("gastown", prefix
// "gt-"), a canonical rig store, a refinery worktree with a redirect, and two
// stray embedded stores matching the 2026-07-04 incident shapes:
//   - <refinery worktree>/gastown/.beads/embeddeddolt  (rig name join)
//   - <townRoot>/deacon/gt-/.beads/embeddeddolt        (prefix join)
func buildStrayTestTown(t *testing.T) (townRoot string, strayRig, strayDeacon string) {
	t.Helper()
	townRoot = t.TempDir()

	mkdir := func(parts ...string) string {
		p := filepath.Join(parts...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		return p
	}
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// Town store with routes.
	townBeads := mkdir(townRoot, ".beads")
	write(filepath.Join(townBeads, "config.yaml"), "")
	write(filepath.Join(townBeads, "routes.jsonl"), `{"prefix":"gt-","path":"gastown"}`+"\n")

	// Rig with canonical store (isLikelyRig via .git).
	rigDir := mkdir(townRoot, "gastown")
	mkdir(rigDir, ".git")
	rigBeads := mkdir(rigDir, ".beads")
	write(filepath.Join(rigBeads, "config.yaml"), "")

	// Refinery worktree with the usual redirect pointer.
	refineryRig := mkdir(rigDir, "refinery", "rig")
	wtBeads := mkdir(refineryRig, ".beads")
	write(filepath.Join(wtBeads, "redirect"), "../../.beads")

	// Stray 1: rig-name join inside the refinery worktree.
	strayRig = filepath.Join(refineryRig, "gastown", ".beads")
	mkdir(strayRig, "embeddeddolt", "gt")

	// Stray 2: prefix join under deacon.
	strayDeacon = filepath.Join(townRoot, "deacon", "gt-", ".beads")
	mkdir(strayDeacon, "embeddeddolt", "deacon")

	return townRoot, strayRig, strayDeacon
}

func TestStrayBeadsStoreCheck_DetectsStrays(t *testing.T) {
	townRoot, strayRig, strayDeacon := buildStrayTestTown(t)

	check := NewStrayBeadsStoreCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusWarning {
		t.Fatalf("expected warning, got %v (%s)", result.Status, result.Message)
	}
	found := strings.Join(result.Details, "\n")
	for _, want := range []string{strayRig, strayDeacon} {
		if !strings.Contains(found, want) {
			t.Errorf("expected stray %s in details, got:\n%s", want, found)
		}
	}
	if len(check.strays) != 2 {
		t.Errorf("expected exactly 2 strays, got %d: %v", len(check.strays), check.strays)
	}
}

func TestStrayBeadsStoreCheck_CleanTownPasses(t *testing.T) {
	townRoot, strayRig, strayDeacon := buildStrayTestTown(t)
	// Remove the strays — canonical stores and redirects must not be flagged.
	for _, s := range []string{strayRig, strayDeacon} {
		if err := os.RemoveAll(filepath.Dir(s)); err != nil {
			t.Fatal(err)
		}
	}

	check := NewStrayBeadsStoreCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})
	if result.Status != StatusOK {
		t.Fatalf("expected OK on clean town, got %v: %s %v", result.Status, result.Message, result.Details)
	}
}

func TestStrayBeadsStoreCheck_FixQuarantinesAndPlantsRedirect(t *testing.T) {
	townRoot, strayRig, strayDeacon := buildStrayTestTown(t)

	check := NewStrayBeadsStoreCheck()
	if result := check.Run(&CheckContext{TownRoot: townRoot}); result.Status != StatusWarning {
		t.Fatalf("precondition: expected warning, got %v", result.Status)
	}
	if err := check.Fix(&CheckContext{TownRoot: townRoot}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	for _, stray := range []string{strayRig, strayDeacon} {
		// Original embedded store must be quarantined (renamed away).
		if dirExists(filepath.Join(stray, "embeddeddolt")) {
			t.Errorf("%s: embedded store still active after fix", stray)
		}
		parent := filepath.Dir(stray)
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatalf("read %s: %v", parent, err)
		}
		quarantined := false
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".beads.quarantined-") {
				quarantined = true
			}
		}
		if !quarantined {
			t.Errorf("%s: no quarantined copy found in %s", stray, parent)
		}

		// A redirect must be planted pointing at a canonical store.
		data, err := os.ReadFile(filepath.Join(stray, "redirect"))
		if err != nil {
			t.Fatalf("%s: expected planted redirect: %v", stray, err)
		}
		target := filepath.Join(parent, strings.TrimSpace(string(data)))
		if !isCanonicalStore(target) {
			t.Errorf("%s: planted redirect %q does not resolve to a canonical store", stray, strings.TrimSpace(string(data)))
		}
	}

	// Re-run: town must now be clean.
	check2 := NewStrayBeadsStoreCheck()
	if result := check2.Run(&CheckContext{TownRoot: townRoot}); result.Status != StatusOK {
		t.Errorf("expected OK after fix, got %v: %v", result.Status, result.Details)
	}
}
