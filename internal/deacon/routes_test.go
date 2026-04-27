package deacon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestSyncBeadsRoutes(t *testing.T) {
	dir := t.TempDir()
	townRoot := filepath.Join(dir, "town")
	deaconDir := filepath.Join(townRoot, "deacon")

	hqBeadsDir := filepath.Join(townRoot, ".beads")
	deaconBeadsDir := filepath.Join(deaconDir, ".beads")

	if err := os.MkdirAll(hqBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(deaconBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write town-level routes including both town-level and rig-level prefixes.
	hqRoutes := []beads.Route{
		{Prefix: "hq-", Path: "."},
		{Prefix: "hq-cv-", Path: "."},
		{Prefix: "gt-", Path: "gastown/mayor/rig"},
		{Prefix: "deacon-", Path: "deacon"},
	}
	if err := beads.WriteRoutes(hqBeadsDir, hqRoutes); err != nil {
		t.Fatal(err)
	}

	if err := SyncBeadsRoutes(townRoot, deaconDir); err != nil {
		t.Fatalf("SyncBeadsRoutes: %v", err)
	}

	got, err := beads.LoadRoutes(deaconBeadsDir)
	if err != nil {
		t.Fatalf("LoadRoutes: %v", err)
	}

	byPrefix := make(map[string]string)
	for _, r := range got {
		byPrefix[r.Prefix] = r.Path
	}

	// Town-level routes should become ".." (one level up = townRoot).
	if p := byPrefix["hq-"]; p != ".." {
		t.Errorf("hq- path = %q, want %q", p, "..")
	}
	if p := byPrefix["hq-cv-"]; p != ".." {
		t.Errorf("hq-cv- path = %q, want %q", p, "..")
	}

	// Rig-level routes should be prefixed with "../".
	if p := byPrefix["gt-"]; p != "../gastown/mayor/rig" {
		t.Errorf("gt- path = %q, want %q", p, "../gastown/mayor/rig")
	}
	if p := byPrefix["deacon-"]; p != "../deacon" {
		t.Errorf("deacon- path = %q, want %q", p, "../deacon")
	}
}

func TestSyncBeadsRoutes_NoHQRoutes(t *testing.T) {
	dir := t.TempDir()
	townRoot := filepath.Join(dir, "town")
	deaconDir := filepath.Join(townRoot, "deacon")

	hqBeadsDir := filepath.Join(townRoot, ".beads")
	deaconBeadsDir := filepath.Join(deaconDir, ".beads")

	if err := os.MkdirAll(hqBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(deaconBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// No HQ routes file — should not create deacon routes file either.
	if err := SyncBeadsRoutes(townRoot, deaconDir); err != nil {
		t.Fatalf("SyncBeadsRoutes with no HQ routes: %v", err)
	}

	// Deacon routes should remain empty (no file written).
	got, err := beads.LoadRoutes(deaconBeadsDir)
	if err != nil {
		t.Fatalf("LoadRoutes: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no routes, got %d", len(got))
	}
}

// TestSyncBeadsRoutes_RouteResolution verifies the transformed path resolves to
// the correct .beads directory when used via ResolveBeadsDirForID.
func TestSyncBeadsRoutes_RouteResolution(t *testing.T) {
	dir := t.TempDir()
	townRoot := filepath.Join(dir, "town")
	deaconDir := filepath.Join(townRoot, "deacon")

	hqBeadsDir := filepath.Join(townRoot, ".beads")
	deaconBeadsDir := filepath.Join(deaconDir, ".beads")

	if err := os.MkdirAll(hqBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(deaconBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	hqRoutes := []beads.Route{
		{Prefix: "hq-", Path: "."},
	}
	if err := beads.WriteRoutes(hqBeadsDir, hqRoutes); err != nil {
		t.Fatal(err)
	}

	if err := SyncBeadsRoutes(townRoot, deaconDir); err != nil {
		t.Fatalf("SyncBeadsRoutes: %v", err)
	}

	// Simulate resolving hq-wisp-xxx from the deacon's .beads context.
	// ResolveBeadsDirForID uses: filepath.Join(filepath.Dir(deaconBeadsDir), route.Path)
	// = filepath.Join(deaconDir, "..") = townRoot, then resolves .beads from there.
	resolved := beads.ResolveBeadsDirForID(deaconBeadsDir, "hq-wisp-test")
	want := hqBeadsDir
	if resolved != want {
		t.Errorf("ResolveBeadsDirForID(deaconBeadsDir, hq-wisp-test) = %q, want %q", resolved, want)
	}
}
