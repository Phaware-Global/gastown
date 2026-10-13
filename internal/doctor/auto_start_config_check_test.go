package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeStore(t *testing.T, townRoot, rel, config string) string {
	t.Helper()
	dir := filepath.Join(townRoot, filepath.FromSlash(rel), ".beads")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{"dolt_mode":"server"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if config != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The 2026-10-12 rogues started from deacon/.beads and gastown/mayor/rig/.beads,
// neither of which is a routes.jsonl rig root, so the check must walk the town.
func TestAutoStartConfigCheck_FlagsEveryStore(t *testing.T) {
	townRoot := t.TempDir()
	writeStore(t, townRoot, ".", "prefix: hq\ndolt.auto-start: false\n")
	writeStore(t, townRoot, "deacon", "prefix: hq\n")
	writeStore(t, townRoot, "gastown/mayor/rig", "prefix: gt\ndolt.auto-start: true\n")
	writeStore(t, townRoot, "gastown/crew/x", "prefix: cx\n")
	// A store with no config.yaml is EnsureConfigYAML's to create, not ours.
	writeStore(t, townRoot, "gastown/crew/noconfig", "")
	// Redirect-only .beads (no metadata.json) points at another store; not a store.
	redirectOnly := filepath.Join(townRoot, "gastown", "polecats", "p", "gastown", ".beads")
	if err := os.MkdirAll(redirectOnly, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewAutoStartConfigCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning: %s", result.Status, result.Message)
	}
	want := map[string]bool{
		filepath.Join("deacon", ".beads"):                  true,
		filepath.Join("gastown", "mayor", "rig", ".beads"): true,
		filepath.Join("gastown", "crew", "x", ".beads"):    true,
	}
	if len(result.Details) != len(want) {
		t.Fatalf("details = %v, want %d stores", result.Details, len(want))
	}
	for _, d := range result.Details {
		if !want[d] {
			t.Errorf("unexpected flagged dir %q", d)
		}
	}
}

func TestAutoStartConfigCheck_FixPreservesOtherLines(t *testing.T) {
	townRoot := t.TempDir()
	deacon := writeStore(t, townRoot, "deacon", "prefix: hq\nsync.remote: x\n")
	rig := writeStore(t, townRoot, "gastown/mayor/rig", "prefix: gt\ndolt.auto-start: true\nexport.auto: \"false\"\n")
	noConfig := writeStore(t, townRoot, "gastown/crew/x", "prefix: cx\n")

	check := NewAutoStartConfigCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	if r := check.Run(ctx); r.Status != StatusWarning {
		t.Fatalf("precondition: want warning, got %v", r.Status)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	for dir, wantKept := range map[string]string{deacon: "sync.remote: x", rig: "export.auto: \"false\"", noConfig: "prefix: cx"} {
		data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		got := string(data)
		if strings.Count(got, "dolt.auto-start: false") != 1 || strings.Contains(got, "auto-start: true") {
			t.Errorf("%s config.yaml = %q, want exactly one dolt.auto-start: false", dir, got)
		}
		if !strings.Contains(got, wantKept) {
			t.Errorf("%s config.yaml = %q lost %q", dir, got, wantKept)
		}
	}
	if r := NewAutoStartConfigCheck().Run(ctx); r.Status != StatusOK {
		t.Fatalf("after fix: status = %v (%v)", r.Status, r.Details)
	}
}

func TestAutoStartConfigCheck_FixNeverCreatesConfig(t *testing.T) {
	townRoot := t.TempDir()
	dir := writeStore(t, townRoot, "deacon", "")

	check := NewAutoStartConfigCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	if r := check.Run(ctx); r.Status != StatusOK {
		t.Fatalf("store without config.yaml: status = %v, want OK (not this check's repair)", r.Status)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("Fix created config.yaml (stat err = %v); that would pre-empt EnsureConfigYAML", err)
	}
}

// A symlinked config.yaml must be neither read nor rewritten: the target lives
// outside the store and the repair would otherwise clobber it.
func TestAutoStartConfigCheck_FixRefusesSymlinkedConfig(t *testing.T) {
	townRoot := t.TempDir()
	dir := writeStore(t, townRoot, "deacon", "")
	target := filepath.Join(t.TempDir(), "elsewhere.yaml")
	if err := os.WriteFile(target, []byte("prefix: victim\n"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	check := NewAutoStartConfigCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	if r := check.Run(ctx); r.Status != StatusWarning {
		t.Fatalf("symlinked config.yaml: status = %v, want warning", r.Status)
	}
	if err := check.Fix(ctx); err == nil {
		t.Fatal("Fix on a symlinked config.yaml succeeded, want refusal")
	}
	if got, _ := os.ReadFile(target); string(got) != "prefix: victim\n" {
		t.Fatalf("symlink target modified: %q", got)
	}
	if info, _ := os.Lstat(link); info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced")
	}
}
