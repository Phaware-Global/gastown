package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// helper to create a plugin directory with a plugin.md and optional extra files.
func createTestPlugin(t *testing.T, dir, name, content string, extras map[string]string) {
	t.Helper()
	pluginDir := filepath.Join(dir, name)
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	for fname, fcontent := range extras {
		if err := os.WriteFile(filepath.Join(pluginDir, fname), []byte(fcontent), 0755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSyncPlugins_CopiesNew(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	createTestPlugin(t, srcDir, "my-plugin", "+++\nname = \"my-plugin\"\n+++\ndo stuff", nil)

	result, err := SyncPlugins(srcDir, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Copied) != 1 || result.Copied[0] != "my-plugin" {
		t.Errorf("expected 1 copied plugin, got %v", result.Copied)
	}

	// Verify file exists at target
	if _, err := os.Stat(filepath.Join(dstDir, "my-plugin", "plugin.md")); err != nil {
		t.Errorf("plugin.md not copied: %v", err)
	}
}

func TestSyncPlugins_SkipsUpToDate(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	content := "+++\nname = \"my-plugin\"\n+++\ndo stuff"
	createTestPlugin(t, srcDir, "my-plugin", content, nil)
	createTestPlugin(t, dstDir, "my-plugin", content, nil)

	result, err := SyncPlugins(srcDir, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skipped) != 1 {
		t.Errorf("expected 1 skipped plugin, got %v", result.Skipped)
	}
	if len(result.Copied) != 0 {
		t.Errorf("expected 0 copied, got %v", result.Copied)
	}
}

func TestSyncPlugins_UpdatesChanged(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	createTestPlugin(t, srcDir, "my-plugin", "+++\nname = \"my-plugin\"\n+++\nv2 instructions", nil)
	createTestPlugin(t, dstDir, "my-plugin", "+++\nname = \"my-plugin\"\n+++\nv1 instructions", nil)

	result, err := SyncPlugins(srcDir, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Copied) != 1 {
		t.Errorf("expected 1 copied plugin, got %v", result.Copied)
	}

	// Verify target has new content
	data, err := os.ReadFile(filepath.Join(dstDir, "my-plugin", "plugin.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "+++\nname = \"my-plugin\"\n+++\nv2 instructions" {
		t.Errorf("content not updated: %s", data)
	}
}

func TestSyncPlugins_CopiesExtraFiles(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	createTestPlugin(t, srcDir, "my-plugin", "+++\nname = \"my-plugin\"\n+++\nstuff",
		map[string]string{"run.sh": "#!/bin/bash\necho hi"})

	result, err := SyncPlugins(srcDir, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Copied) != 1 {
		t.Errorf("expected 1 copied, got %v", result.Copied)
	}

	// Verify run.sh was copied
	data, err := os.ReadFile(filepath.Join(dstDir, "my-plugin", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "#!/bin/bash\necho hi" {
		t.Errorf("run.sh content wrong: %s", data)
	}

	// Verify executable permission preserved (skip on Windows where permission bits aren't meaningful)
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(dstDir, "my-plugin", "run.sh"))
		if info.Mode()&0111 == 0 {
			t.Error("run.sh lost executable permission")
		}
	}
}

func TestSyncPlugins_CleanRemovesExtra(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	createTestPlugin(t, srcDir, "keep-me", "+++\nname = \"keep-me\"\n+++\nkeep", nil)
	createTestPlugin(t, dstDir, "keep-me", "+++\nname = \"keep-me\"\n+++\nkeep", nil)
	createTestPlugin(t, dstDir, "old-plugin", "+++\nname = \"old-plugin\"\n+++\nold", nil)

	result, err := SyncPlugins(srcDir, dstDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != "old-plugin" {
		t.Errorf("expected old-plugin removed, got %v", result.Removed)
	}

	// Verify old plugin was removed
	if _, err := os.Stat(filepath.Join(dstDir, "old-plugin")); !os.IsNotExist(err) {
		t.Error("old-plugin should have been removed")
	}
}

func TestSyncPlugins_NoCleanKeepsExtra(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	createTestPlugin(t, srcDir, "new-plugin", "+++\nname = \"new-plugin\"\n+++\nnew", nil)
	createTestPlugin(t, dstDir, "old-plugin", "+++\nname = \"old-plugin\"\n+++\nold", nil)

	result, err := SyncPlugins(srcDir, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 0 {
		t.Errorf("expected 0 removed (clean=false), got %v", result.Removed)
	}

	// Verify old plugin still exists
	if _, err := os.Stat(filepath.Join(dstDir, "old-plugin", "plugin.md")); err != nil {
		t.Error("old-plugin should still exist when clean=false")
	}
}

func TestSyncPlugins_IgnoresNonPluginDirs(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	// Create a directory without plugin.md — should be ignored
	notPlugin := filepath.Join(srcDir, "not-a-plugin")
	if err := os.MkdirAll(notPlugin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notPlugin, "README.md"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	result, err := SyncPlugins(srcDir, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Copied) != 0 {
		t.Errorf("expected 0 copied (no valid plugins), got %v", result.Copied)
	}
}

func TestDetectDrift_NoDrift(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	content := "+++\nname = \"stable\"\n+++\nstuff"
	createTestPlugin(t, srcDir, "stable", content, nil)
	createTestPlugin(t, dstDir, "stable", content, nil)

	report, err := DetectDrift(srcDir, dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if report.HasDrift() {
		t.Error("expected no drift")
	}
}

func TestDetectDrift_ContentDiffers(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	createTestPlugin(t, srcDir, "changed", "+++\nname = \"changed\"\n+++\nv2", nil)
	createTestPlugin(t, dstDir, "changed", "+++\nname = \"changed\"\n+++\nv1", nil)

	report, err := DetectDrift(srcDir, dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if !report.HasDrift() {
		t.Error("expected drift")
	}
	if len(report.Drifted) != 1 || report.Drifted[0].Name != "changed" {
		t.Errorf("expected changed in drifted, got %v", report.Drifted)
	}
}

func TestDetectDrift_MissingFromTarget(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	createTestPlugin(t, srcDir, "new-one", "+++\nname = \"new-one\"\n+++\nnew", nil)

	report, err := DetectDrift(srcDir, dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if !report.HasDrift() {
		t.Error("expected drift")
	}
	if len(report.Missing) != 1 || report.Missing[0] != "new-one" {
		t.Errorf("expected new-one missing, got %v", report.Missing)
	}
}

func TestDetectDrift_ExtraInTarget(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	createTestPlugin(t, dstDir, "orphan", "+++\nname = \"orphan\"\n+++\nold", nil)

	report, err := DetectDrift(srcDir, dstDir)
	if err != nil {
		t.Fatal(err)
	}
	// Extra plugins are not drift (no HasDrift), but are reported
	if len(report.Extra) != 1 || report.Extra[0] != "orphan" {
		t.Errorf("expected orphan in extra, got %v", report.Extra)
	}
}

// runGitTest runs a git command, failing the test on error. If dir is
// non-empty, it runs with that directory via `-C` (for commands like
// `commit`/`checkout` that operate on an existing repo); if empty, args must
// be self-contained (e.g. `init --bare <path>`, `clone <src> <dst>`).
func runGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// setupGastownCheckout creates a bare "origin" repo plus a working checkout
// of it, with a go.mod declaring the gastown module and a plugins/ directory
// committed and pushed to origin/main. It returns the checkout dir.
func setupGastownCheckout(t *testing.T) (checkoutDir string) {
	t.Helper()
	originDir := t.TempDir()
	runGitTest(t, "", "init", "--bare", "--initial-branch=main", originDir)

	checkoutDir = t.TempDir()
	runGitTest(t, "", "init", "--initial-branch=main", checkoutDir)
	runGitTest(t, checkoutDir, "remote", "add", "origin", originDir)
	if err := os.WriteFile(filepath.Join(checkoutDir, "go.mod"), []byte("module github.com/steveyegge/gastown\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	createTestPlugin(t, filepath.Join(checkoutDir, "plugins"), "some-plugin",
		"+++\nname = \"some-plugin\"\n+++\noriginal", map[string]string{"run.sh": "#!/bin/bash\necho v1"})
	runGitTest(t, checkoutDir, "add", "-A")
	runGitTest(t, checkoutDir, "commit", "-m", "initial")
	runGitTest(t, checkoutDir, "push", "origin", "main")
	return checkoutDir
}

func TestFindGastownGitDir_ResolvesCandidateUnderTownRoot(t *testing.T) {
	townRoot := t.TempDir()
	// Isolate from the real gastown checkout this test runs inside of: the
	// cwd-walk-up in FindGastownGitDir would otherwise find it first.
	t.Chdir(townRoot)
	rigDir := filepath.Join(townRoot, "gastown", "mayor", "rig")
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, "go.mod"), []byte("module github.com/steveyegge/gastown\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	got, err := FindGastownGitDir(townRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got != rigDir {
		t.Errorf("expected %s, got %s", rigDir, got)
	}
}

func TestFindGastownGitDir_NoCandidateFound(t *testing.T) {
	townRoot := t.TempDir()
	t.Chdir(townRoot)
	if _, err := FindGastownGitDir(townRoot); err == nil {
		t.Error("expected error when no gastown checkout exists")
	}
}

// TestSyncFromOrigin_IgnoresStaleWorkingTree is the regression test for
// gt-2ea1: a checkout whose working tree sits on an unrelated, stale branch
// must still deploy the true origin/main content, because SyncFromOrigin
// reads the tree from git's object store rather than the working tree.
func TestSyncFromOrigin_IgnoresStaleWorkingTree(t *testing.T) {
	checkoutDir := setupGastownCheckout(t)

	// Move the checkout onto a detached, stale branch with an old plugin
	// version — simulating a long-lived rig checkout nobody has pulled.
	runGitTest(t, checkoutDir, "checkout", "-b", "stale-local-branch")
	pluginFile := filepath.Join(checkoutDir, "plugins", "some-plugin", "run.sh")
	if err := os.WriteFile(pluginFile, []byte("#!/bin/bash\necho STALE"), 0755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, checkoutDir, "commit", "-am", "stale local-only change")

	// Meanwhile origin/main moved forward with a real fix.
	runGitTest(t, checkoutDir, "checkout", "main")
	if err := os.WriteFile(pluginFile, []byte("#!/bin/bash\necho v2-fixed"), 0755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, checkoutDir, "commit", "-am", "v2 fix")
	runGitTest(t, checkoutDir, "push", "origin", "main")

	// Leave the working tree parked on the stale branch, as a drifted rig
	// checkout would be.
	runGitTest(t, checkoutDir, "checkout", "stale-local-branch")

	targetDir := t.TempDir()
	result, err := SyncFromOrigin(checkoutDir, targetDir, false)
	if err != nil {
		t.Fatalf("SyncFromOrigin failed: %v", err)
	}
	if len(result.Copied) != 1 || result.Copied[0] != "some-plugin" {
		t.Errorf("expected some-plugin copied, got %+v", result)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "some-plugin", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "#!/bin/bash\necho v2-fixed" {
		t.Errorf("expected origin/main content, got working-tree content: %q", data)
	}
}

func TestSyncFromOrigin_MissingPluginsDir(t *testing.T) {
	originDir := t.TempDir()
	runGitTest(t, "", "init", "--bare", "--initial-branch=main", originDir)
	checkoutDir := t.TempDir()
	runGitTest(t, "", "init", "--initial-branch=main", checkoutDir)
	runGitTest(t, checkoutDir, "remote", "add", "origin", originDir)
	if err := os.WriteFile(filepath.Join(checkoutDir, "README.md"), []byte("no plugins here"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, checkoutDir, "add", "-A")
	runGitTest(t, checkoutDir, "commit", "-m", "no plugins")
	runGitTest(t, checkoutDir, "push", "origin", "main")

	if _, err := SyncFromOrigin(checkoutDir, t.TempDir(), false); err == nil {
		t.Error("expected error when origin/main has no plugins/ directory")
	}
}
