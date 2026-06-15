package reviewer

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func writePerspective(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, "settings", "review", "perspectives")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestResolvePerspective_BuiltinDefaults(t *testing.T) {
	for _, name := range []string{"adversarial", "security"} {
		rp, err := ResolvePerspective("", "", name)
		if err != nil {
			t.Fatalf("ResolvePerspective(%q): %v", name, err)
		}
		if rp.Source != PerspectiveSourceBuiltin {
			t.Errorf("%s source = %q, want builtin", name, rp.Source)
		}
		if rp.Content == "" {
			t.Errorf("%s content empty", name)
		}
	}
}

func TestResolvePerspective_RigOverridesTownOverridesBuiltin(t *testing.T) {
	town := t.TempDir()
	rig := t.TempDir()

	// builtin only
	rp, err := ResolvePerspective(town, rig, "adversarial")
	if err != nil {
		t.Fatal(err)
	}
	if rp.Source != PerspectiveSourceBuiltin {
		t.Fatalf("expected builtin, got %s", rp.Source)
	}

	// town shadows builtin
	writePerspective(t, town, "adversarial", "TOWN adversarial")
	rp, err = ResolvePerspective(town, rig, "adversarial")
	if err != nil {
		t.Fatal(err)
	}
	if rp.Source != PerspectiveSourceTown || rp.Content != "TOWN adversarial" {
		t.Fatalf("expected town override, got %s / %q", rp.Source, rp.Content)
	}

	// rig shadows town
	writePerspective(t, rig, "adversarial", "RIG adversarial")
	rp, err = ResolvePerspective(town, rig, "adversarial")
	if err != nil {
		t.Fatal(err)
	}
	if rp.Source != PerspectiveSourceRig || rp.Content != "RIG adversarial" {
		t.Fatalf("expected rig override, got %s / %q", rp.Source, rp.Content)
	}
}

func TestResolvePerspective_UnknownErrors(t *testing.T) {
	if _, err := ResolvePerspective("", "", "does-not-exist"); err == nil {
		t.Error("expected error for unknown perspective")
	}
	if _, err := ResolvePerspective("", "", "  "); err == nil {
		t.Error("expected error for blank name")
	}
}

func TestResolvePerspective_RejectsPathTraversal(t *testing.T) {
	town := t.TempDir()
	rig := t.TempDir()
	for _, name := range []string{
		"../../etc/passwd",
		"..",
		"foo/bar",
		`foo\bar`,
		"../adversarial",
		"sub/../adversarial",
	} {
		if _, err := ResolvePerspective(town, rig, name); err == nil {
			t.Errorf("ResolvePerspective(%q) should reject path-traversal name", name)
		}
	}
}

func TestResolvePerspective_SurfacesRealReadErrors(t *testing.T) {
	rig := t.TempDir()
	// Create the perspective path as a DIRECTORY so os.ReadFile fails with a
	// non-IsNotExist error; ResolvePerspective must surface it rather than
	// silently falling through to the built-in default.
	dir := filepath.Join(rig, "settings", "review", "perspectives", "adversarial.md")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := ResolvePerspective("", rig, "adversarial")
	if err == nil {
		t.Fatal("expected a real read error to surface, got nil (silently fell through)")
	}
}

func TestResolvePerspectives_FailSilent(t *testing.T) {
	// Strict: unknown is a hard error.
	if _, _, err := ResolvePerspectives("", "", []string{"adversarial", "nope"}, false); err == nil {
		t.Error("strict mode should error on unknown perspective")
	}
	// Silent: unknown is skipped and reported.
	resolved, skipped, err := ResolvePerspectives("", "", []string{"adversarial", "nope", "security"}, true)
	if err != nil {
		t.Fatalf("silent mode errored: %v", err)
	}
	if len(resolved) != 2 {
		t.Errorf("resolved %d, want 2", len(resolved))
	}
	if len(skipped) != 1 || skipped[0] != "nope" {
		t.Errorf("skipped = %v, want [nope]", skipped)
	}
	// Even in fail-silent mode, a path-traversal name (validation error, not
	// "not found") must surface rather than be silently skipped.
	if _, _, err := ResolvePerspectives("", "", []string{"adversarial", "../evil"}, true); err == nil {
		t.Error("fail-silent mode must still surface path-traversal names")
	}
}

func TestBuiltinPerspectiveNames(t *testing.T) {
	got := BuiltinPerspectiveNames()
	sort.Strings(got)
	want := []string{"adversarial", "security"}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("BuiltinPerspectiveNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("BuiltinPerspectiveNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
