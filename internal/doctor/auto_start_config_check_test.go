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
	// metadata.json but no config.yaml: bd falls back to its defaults, which
	// auto-start, so it must be reported (see the Missing tests for the repair).
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
		filepath.Join("deacon", ".beads"):                      true,
		filepath.Join("gastown", "mayor", "rig", ".beads"):     true,
		filepath.Join("gastown", "crew", "x", ".beads"):        true,
		filepath.Join("gastown", "crew", "noconfig", ".beads"): true,
	}
	if len(result.Details) != len(want) {
		t.Fatalf("details = %v, want %d stores", result.Details, len(want))
	}
	for _, d := range result.Details {
		dir, _, _ := strings.Cut(d, " (") // details carry a "(config.yaml missing)" style note
		if !want[dir] {
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

func writeMetadata(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

// A store with metadata.json but no config.yaml must not pass (bd would
// auto-start), but the check must not create the file either: a new config
// needs a prefix, and the metadata-derived one is wrong for canonical rig
// stores. It is reported as needing manual repair; other stores still get fixed.
func TestAutoStartConfigCheck_MissingConfigIsReportedAndNeverCreated(t *testing.T) {
	townRoot := t.TempDir()
	noConfig := writeStore(t, townRoot, "gastown/crew/x", "")
	writeMetadata(t, noConfig, `{"dolt_mode":"server","dolt_database":"gt"}`)
	other := writeStore(t, townRoot, "z-other", "prefix: z\n")

	check := NewAutoStartConfigCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	r := check.Run(ctx)
	if r.Status != StatusWarning {
		t.Fatalf("missing config.yaml: status = %v, want warning", r.Status)
	}
	var found bool
	for _, d := range r.Details {
		if strings.HasPrefix(d, filepath.Join("gastown", "crew", "x", ".beads")) {
			found = true
			if !strings.Contains(d, "set dolt.auto-start: false manually") {
				t.Errorf("detail %q does not tell the operator to set it manually", d)
			}
		}
	}
	if !found {
		t.Fatalf("config-less store not reported: %v", r.Details)
	}
	if !check.CanFix() {
		t.Fatal("CanFix = false though another store is repairable")
	}

	err := check.Fix(ctx)
	if err == nil || !strings.Contains(err.Error(), filepath.Join("gastown", "crew", "x")) {
		t.Fatalf("Fix error = %v, want it to name the config-less store", err)
	}
	if _, statErr := os.Stat(filepath.Join(noConfig, "config.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("Fix created config.yaml (stat err = %v)", statErr)
	}
	data, _ := os.ReadFile(filepath.Join(other, "config.yaml"))
	if !strings.Contains(string(data), "dolt.auto-start: false") {
		t.Fatalf("other store not repaired: %q", data)
	}
}

func TestAutoStartConfigCheck_OnlyMissingConfigIsNotFixable(t *testing.T) {
	townRoot := t.TempDir()
	dir := writeStore(t, townRoot, "deacon", "")
	writeMetadata(t, dir, `{"dolt_mode":"server","dolt_database":"deacon"}`)

	check := NewAutoStartConfigCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	if r := check.Run(ctx); r.Status != StatusWarning {
		t.Fatalf("status = %v, want warning", r.Status)
	}
	if check.CanFix() {
		t.Fatal("only a config-less store: CanFix = true, want false")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("config.yaml created (stat err = %v)", err)
	}
}

// One refused store must not stop the rest being repaired, and the refusal is
// reported by Fix. The refused store alone makes the check unfixable.
func TestAutoStartConfigCheck_RefusedStoreDoesNotStopOthers(t *testing.T) {
	townRoot := t.TempDir()
	refused := writeStore(t, townRoot, "a-refused", "")
	target := filepath.Join(t.TempDir(), "elsewhere.yaml")
	if err := os.WriteFile(target, []byte("prefix: victim\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(refused, "config.yaml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	fixable := writeStore(t, townRoot, "z-fixable", "prefix: z\n")

	check := NewAutoStartConfigCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	check.Run(ctx)

	err := check.Fix(ctx)
	if err == nil || !strings.Contains(err.Error(), "a-refused") {
		t.Fatalf("Fix error = %v, want it to name the refused store", err)
	}
	data, _ := os.ReadFile(filepath.Join(fixable, "config.yaml"))
	if !strings.Contains(string(data), "dolt.auto-start: false") {
		t.Fatalf("remaining store not repaired: %q", data)
	}
}

func TestAutoStartConfigCheck_OnlyRefusedStoreIsNotFixable(t *testing.T) {
	townRoot := t.TempDir()
	dir := writeStore(t, townRoot, "deacon", "")
	target := filepath.Join(t.TempDir(), "elsewhere.yaml")
	if err := os.WriteFile(target, []byte("prefix: victim\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "config.yaml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	check := NewAutoStartConfigCheck()
	check.Run(&CheckContext{TownRoot: townRoot})
	if check.CanFix() {
		t.Fatal("symlinked config.yaml marked fixable")
	}
}
