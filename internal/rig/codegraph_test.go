package rig

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func boolPtr(b bool) *bool { return &b }

func writeTownSettings(t *testing.T, townRoot string, s *config.TownSettings) {
	t.Helper()
	settingsDir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.TownSettingsPath(townRoot), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func writeRigSettings(t *testing.T, rigPath string, s *config.RigSettings) {
	t.Helper()
	settingsDir := filepath.Join(rigPath, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.RigSettingsPath(rigPath), data, 0644); err != nil {
		t.Fatal(err)
	}
}

// writeFakeMiseCodegraph plants an executable codegraph at the mise Node install
// path <miseData>/installs/node/<ver>/bin/codegraph and returns its path.
func writeFakeMiseCodegraph(t *testing.T, miseData, nodeVer, script string) string {
	t.Helper()
	binDir := filepath.Join(miseData, "installs", "node", nodeVer, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, "codegraph")
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

// isolatePATH points PATH at an empty dir so exec.LookPath can't find a real
// codegraph installed on the dev box.
func isolatePATH(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))
}

func TestEnsureCodegraphIndex_DisabledByTown(t *testing.T) {
	tmp := t.TempDir()
	townRoot := filepath.Join(tmp, "town")
	rigPath := filepath.Join(townRoot, "rig")
	worktree := filepath.Join(tmp, "worktree")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}

	writeTownSettings(t, townRoot, &config.TownSettings{
		CodeGraph: &config.CodeGraphConfig{Enabled: boolPtr(false)},
	})

	// Must not fail even though codegraph is not installed.
	if err := EnsureCodegraphIndex(worktree, townRoot, rigPath); err != nil {
		t.Errorf("expected nil error when disabled, got: %v", err)
	}
}

func TestEnsureCodegraphIndex_DisabledByRig(t *testing.T) {
	tmp := t.TempDir()
	townRoot := filepath.Join(tmp, "town")
	rigPath := filepath.Join(townRoot, "rig")
	worktree := filepath.Join(tmp, "worktree")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}

	// Town enables, rig overrides to disabled.
	writeTownSettings(t, townRoot, &config.TownSettings{
		CodeGraph: &config.CodeGraphConfig{Enabled: boolPtr(true)},
	})
	writeRigSettings(t, rigPath, &config.RigSettings{
		CodeGraph: &config.CodeGraphConfig{Enabled: boolPtr(false)},
	})

	if err := EnsureCodegraphIndex(worktree, townRoot, rigPath); err != nil {
		t.Errorf("expected nil error when disabled by rig, got: %v", err)
	}
}

func TestEnsureCodegraphIndex_BinaryMissing(t *testing.T) {
	tmp := t.TempDir()
	townRoot := filepath.Join(tmp, "town")
	rigPath := filepath.Join(townRoot, "rig")
	worktree := filepath.Join(tmp, "worktree")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}

	// Enabled but codegraph resolvable nowhere — the background path skips silently.
	isolatePATH(t)
	t.Setenv("MISE_DATA_DIR", filepath.Join(tmp, "empty-mise")) // no installs

	if err := EnsureCodegraphIndex(worktree, townRoot, rigPath); err != nil {
		t.Errorf("expected nil error when binary missing, got: %v", err)
	}
}

func TestIsMiseNodeInstallBin(t *testing.T) {
	cases := map[string]bool{
		"/home/u/.local/share/mise/installs/node/24.14.1/bin/codegraph": true,
		"/home/u/.local/share/mise/shims/codegraph":                     false, // the mise shim (Node-pin sensitive)
		"/usr/local/bin/codegraph":                                      false,
	}
	for p, want := range cases {
		if got := isMiseNodeInstallBin(p); got != want {
			t.Errorf("isMiseNodeInstallBin(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestResolveCodegraphExe_FindsHighestMiseInstall(t *testing.T) {
	tmp := t.TempDir()
	miseData := filepath.Join(tmp, "mise")
	writeFakeMiseCodegraph(t, miseData, "22.0.0", "#!/bin/sh\nexit 0\n")
	hi := writeFakeMiseCodegraph(t, miseData, "24.0.0", "#!/bin/sh\nexit 0\n")
	// A mise alias dir ("lts") must NOT win over a concrete version, even though
	// it sorts lexically higher than any numeric version.
	writeFakeMiseCodegraph(t, miseData, "lts", "#!/bin/sh\nexit 0\n")

	isolatePATH(t) // no PATH hit → must fall back to the mise-install scan
	t.Setenv("MISE_DATA_DIR", miseData)

	if got := resolveCodegraphExe(); got != hi {
		t.Errorf("resolveCodegraphExe = %q, want highest concrete-version install %q", got, hi)
	}
}

func TestResolveCodegraphExe_NoneFound(t *testing.T) {
	tmp := t.TempDir()
	isolatePATH(t)
	t.Setenv("MISE_DATA_DIR", filepath.Join(tmp, "empty-mise"))

	if got := resolveCodegraphExe(); got != "" {
		t.Errorf("resolveCodegraphExe = %q, want empty", got)
	}
}

func TestResolveCodegraphExe_AcceptsNonShimPATHHit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake codegraph is a shell script")
	}
	// A standalone/global install (not under mise) on PATH must be accepted.
	tmp := t.TempDir()
	globalBin := filepath.Join(tmp, "usr", "local", "bin")
	if err := os.MkdirAll(globalBin, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(globalBin, "codegraph")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", globalBin)
	t.Setenv("MISE_DATA_DIR", filepath.Join(tmp, "empty-mise"))

	if got := resolveCodegraphExe(); got != exe {
		t.Errorf("resolveCodegraphExe = %q, want the global install %q", got, exe)
	}
}

func TestResolveCodegraphExe_RejectsShimPATHHit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake codegraph is a shell script")
	}
	// A mise shim on PATH must be rejected (it delegates to mise, which refuses
	// under a Node pin); with no real install to fall back to, resolution fails.
	tmp := t.TempDir()
	shimDir := filepath.Join(tmp, "mise", "shims")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shimDir, "codegraph"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir)
	t.Setenv("MISE_DATA_DIR", filepath.Join(tmp, "empty-mise"))

	if got := resolveCodegraphExe(); got != "" {
		t.Errorf("resolveCodegraphExe = %q, want empty (shim rejected, no install fallback)", got)
	}
}

func TestCodegraphSubcommand(t *testing.T) {
	wt := t.TempDir()
	if got := codegraphSubcommand(wt); got != "init" {
		t.Errorf("fresh worktree: got %q, want init", got)
	}
	if err := os.MkdirAll(filepath.Join(wt, ".codegraph"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := codegraphSubcommand(wt); got != "index" {
		t.Errorf("initialized worktree: got %q, want index", got)
	}
}

func TestEnsureCodegraphIndexSync_UnavailableSurfaces(t *testing.T) {
	tmp := t.TempDir()
	townRoot := filepath.Join(tmp, "town")
	rigPath := filepath.Join(townRoot, "rig")
	worktree := filepath.Join(tmp, "worktree")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}
	writeTownSettings(t, townRoot, &config.TownSettings{
		CodeGraph: &config.CodeGraphConfig{Enabled: boolPtr(true)},
	})

	isolatePATH(t)
	t.Setenv("MISE_DATA_DIR", filepath.Join(tmp, "empty-mise"))

	// Unlike the background path, the synchronous path must surface the gap so the
	// reviewer knows the index is absent rather than reviewing blind.
	if err := EnsureCodegraphIndexSync(worktree, townRoot, rigPath); !errors.Is(err, ErrCodegraphUnavailable) {
		t.Errorf("expected ErrCodegraphUnavailable, got: %v", err)
	}
}

func TestEnsureCodegraphIndexSync_RunsResolvedBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake codegraph is a shell script")
	}
	tmp := t.TempDir()
	townRoot := filepath.Join(tmp, "town")
	rigPath := filepath.Join(townRoot, "rig")
	worktree := filepath.Join(tmp, "worktree")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}
	writeTownSettings(t, townRoot, &config.TownSettings{
		CodeGraph: &config.CodeGraphConfig{Enabled: boolPtr(true)},
	})

	// A fake codegraph (no sibling node) that records the subcommand it was run
	// with, proving the resolver reached a mise-install binary and ran it.
	miseData := filepath.Join(tmp, "mise")
	marker := filepath.Join(tmp, "invoked")
	writeFakeMiseCodegraph(t, miseData, "24.0.0", "#!/bin/sh\nprintf '%s' \"$1\" > "+marker+"\nexit 0\n")

	isolatePATH(t)
	t.Setenv("MISE_DATA_DIR", miseData)

	if err := EnsureCodegraphIndexSync(worktree, townRoot, rigPath); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("codegraph was not invoked: %v", err)
	}
	if string(got) != "init" {
		t.Errorf("subcommand = %q, want init (fresh worktree)", string(got))
	}
}

func TestEnsureCodegraphIndexSync_DisabledReturnsSentinel(t *testing.T) {
	tmp := t.TempDir()
	townRoot := filepath.Join(tmp, "town")
	rigPath := filepath.Join(townRoot, "rig")
	worktree := filepath.Join(tmp, "worktree")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}
	writeTownSettings(t, townRoot, &config.TownSettings{
		CodeGraph: &config.CodeGraphConfig{Enabled: boolPtr(false)},
	})

	// Disabled must be distinguishable from a successful build so the reviewer
	// reports "disabled" rather than a false "index refreshed".
	if err := EnsureCodegraphIndexSync(worktree, townRoot, rigPath); !errors.Is(err, ErrCodegraphDisabled) {
		t.Errorf("expected ErrCodegraphDisabled when indexing is off, got: %v", err)
	}
}

func TestIsSensitiveEnv(t *testing.T) {
	cases := map[string]bool{
		"GITHUB_TOKEN":             true,
		"GT_REVIEWER_GITHUB_TOKEN": true,
		"AWS_SECRET_ACCESS_KEY":    true,
		"MY_PASSWORD":              true,
		"NPM_CONFIG_CREDENTIAL":    true,
		"SSH_PRIVATE_KEY":          true,
		"PATH":                     false,
		"HOME":                     false,
		"NODE_ENV":                 false,
		"MISE_DATA_DIR":            false,
	}
	for name, want := range cases {
		if got := isSensitiveEnv(name); got != want {
			t.Errorf("isSensitiveEnv(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestCodegraphEnv_StripsSecretsAndSetsPATH(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret")
	t.Setenv("HOME", "/home/reviewer")
	t.Setenv("PATH", "/usr/bin:/bin")

	env := codegraphEnv("/opt/mise/node/24/bin")

	var sawHome, sawToken, sawPath bool
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "HOME="):
			sawHome = true
		case strings.HasPrefix(kv, "GITHUB_TOKEN="):
			sawToken = true
		case strings.HasPrefix(kv, "PATH="):
			sawPath = true
			if !strings.HasPrefix(kv, "PATH=/opt/mise/node/24/bin"+string(os.PathListSeparator)) {
				t.Errorf("PATH not install-bin-first: %q", kv)
			}
		}
	}
	if sawToken {
		t.Error("GITHUB_TOKEN must be stripped from the codegraph environment")
	}
	if !sawHome {
		t.Error("HOME must be preserved in the codegraph environment")
	}
	if !sawPath {
		t.Error("PATH must be set in the codegraph environment")
	}
}
