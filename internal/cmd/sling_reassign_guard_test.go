package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
)

func stubPolecatWorkStatus(t *testing.T, status *git.UncommittedWorkStatus, err error) *int {
	t.Helper()
	calls := 0
	orig := polecatWorkStatusFn
	polecatWorkStatusFn = func(string) (*git.UncommittedWorkStatus, error) {
		calls++
		return status, err
	}
	t.Cleanup(func() { polecatWorkStatusFn = orig })
	return &calls
}

func makePolecatClone(t *testing.T, townRoot, rig, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(townRoot, rig, "polecats", name, rig), 0o755); err != nil {
		t.Fatal(err)
	}
}

func makeLegacyPolecatClone(t *testing.T, townRoot, rig, name string) {
	t.Helper()
	dir := filepath.Join(townRoot, rig, "polecats", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckReassignPreservesWork(t *testing.T) {
	const assignee = "gastown/polecats/toast"

	tests := []struct {
		name      string
		assignee  string
		makeClone bool
		legacy    bool
		status    *git.UncommittedWorkStatus
		statusErr error
		wantErr   string
		wantCalls int
	}{
		{name: "non-polecat holder passes", assignee: "gastown/crew/max", wantCalls: 0},
		{name: "empty holder passes", assignee: "", wantCalls: 0},
		{name: "missing worktree passes", assignee: assignee, wantCalls: 0},
		{
			name: "legacy-layout worktree with unpushed commits refuses", assignee: assignee, legacy: true, wantCalls: 1, wantErr: "2 unpushed commit(s)",
			status: &git.UncommittedWorkStatus{UnpushedCommits: 2},
		},
		{name: "legacy-layout clean worktree passes", assignee: assignee, legacy: true, status: &git.UncommittedWorkStatus{}, wantCalls: 1},
		{name: "clean worktree passes", assignee: assignee, makeClone: true, status: &git.UncommittedWorkStatus{}, wantCalls: 1},
		{
			name: "runtime-only changes pass", assignee: assignee, makeClone: true, wantCalls: 1,
			status: &git.UncommittedWorkStatus{HasUncommittedChanges: true, UntrackedFiles: []string{".claude/settings.json"}},
		},
		{
			name: "uncommitted edits refuse", assignee: assignee, makeClone: true, wantCalls: 1, wantErr: "unpreserved work",
			status: &git.UncommittedWorkStatus{HasUncommittedChanges: true, ModifiedFiles: []string{"internal/cmd/sling.go"}},
		},
		{
			name: "untracked source refuses", assignee: assignee, makeClone: true, wantCalls: 1, wantErr: "unpreserved work",
			status: &git.UncommittedWorkStatus{HasUncommittedChanges: true, UntrackedFiles: []string{"internal/cmd/new.go"}},
		},
		{
			name: "stash refuses", assignee: assignee, makeClone: true, wantCalls: 1, wantErr: "1 stash(es)",
			status: &git.UncommittedWorkStatus{StashCount: 1},
		},
		{
			name: "unpushed commits refuse", assignee: assignee, makeClone: true, wantCalls: 1, wantErr: "2 unpushed commit(s)",
			status: &git.UncommittedWorkStatus{UnpushedCommits: 2},
		},
		{
			name: "status error fails closed", assignee: assignee, makeClone: true, wantCalls: 1, wantErr: "cannot verify",
			statusErr: errors.New("git exploded"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			townRoot := t.TempDir()
			if tt.makeClone {
				makePolecatClone(t, townRoot, "gastown", "toast")
			}
			if tt.legacy {
				makeLegacyPolecatClone(t, townRoot, "gastown", "toast")
			}
			calls := stubPolecatWorkStatus(t, tt.status, tt.statusErr)

			err := checkReassignPreservesWork(townRoot, tt.assignee)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
			if *calls != tt.wantCalls {
				t.Errorf("status checks = %d, want %d", *calls, tt.wantCalls)
			}
		})
	}
}

func TestPolecatClonePathIgnoresLegacyDirWithoutGit(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown", "polecats", "toast"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := polecatClonePath(townRoot, "gastown", "toast")
	if err != nil || got != "" {
		t.Fatalf("polecatClonePath = %q, %v; want no worktree", got, err)
	}
}

func TestSlingTargetIsHolder(t *testing.T) {
	const holder = "gastown/polecats/toast"
	tests := []struct {
		target string
		want   bool
	}{
		{holder, true},
		{holder + "/", true},
		// Shorthand is ambiguous (crew or polecat), so it stays under the guard.
		{"gastown/toast", false},
		// A rig target spawns a fresh polecat, so the holder's worktree is not kept.
		{"gastown", false},
		{"gastown/polecats/nux", false},
		{"gastown/crew/max", false},
		{"deacon/dogs", false},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			if got := slingTargetIsHolder(tt.target, holder); got != tt.want {
				t.Errorf("slingTargetIsHolder(%q) = %v, want %v", tt.target, got, tt.want)
			}
		})
	}
	if slingTargetIsHolder("gastown/polecats/toast", "") {
		t.Error("empty holder must never match")
	}
}

// TestSlingForceRigTargetRefusedBeforeSpawn: the guard must refuse a forced
// re-sling to a rig before resolveTarget spawns a replacement. Refusing after
// the spawn rolls the replacement back through rollbackSlingArtifacts, which
// clears the protected holder's hook and molecule.
func TestSlingForceRigTargetRefusedBeforeSpawn(t *testing.T) {
	townRoot := t.TempDir()

	for _, dir := range []string{
		filepath.Join(townRoot, "mayor", "rig"),
		filepath.Join(townRoot, "gastown", "mayor", "rig"),
		filepath.Join(townRoot, ".beads"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(`{"prefix":"gt-","path":"."}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rigs := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{
		"gastown": {GitURL: "git@github.com:test/gastown.git", AddedAt: time.Now().Truncate(time.Second), BeadsConfig: &config.BeadsConfig{Repo: "local", Prefix: "gt-"}},
	}}
	if err := config.SaveRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"), rigs); err != nil {
		t.Fatalf("SaveRigsConfig: %v", err)
	}
	makePolecatClone(t, townRoot, "gastown", "toast")

	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bdScript := `#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    show) echo '[{"title":"Test issue","status":"hooked","assignee":"gastown/polecats/toast","description":""}]'; exit 0 ;;
  esac
done
exit 0
`
	bdScriptWindows := `@echo off
for %%a in (%*) do (
  if "%%a"=="show" (echo [{"title":"Test issue","status":"hooked","assignee":"gastown/polecats/toast","description":""}]& exit /b 0)
)
exit /b 0
`
	_ = writeBDStub(t, binDir, bdScript, bdScriptWindows)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvGTRole, "mayor")
	t.Setenv("GT_TEST_NO_NUDGE", "1")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor", "rig")); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	prevForce := slingForce
	prevDryRun := slingDryRun
	prevNoConvoy := slingNoConvoy
	prevNoBoot := slingNoBoot
	prevSpawn := spawnPolecatForSling
	prevRollback := rollbackSlingArtifactsFn
	t.Cleanup(func() {
		slingForce = prevForce
		slingDryRun = prevDryRun
		slingNoConvoy = prevNoConvoy
		slingNoBoot = prevNoBoot
		spawnPolecatForSling = prevSpawn
		rollbackSlingArtifactsFn = prevRollback
	})
	slingForce = true
	slingDryRun = false
	slingNoConvoy = true
	slingNoBoot = true

	spawned, rolledBack := false, false
	spawnPolecatForSling = func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		spawned = true
		return &SpawnedPolecatInfo{RigName: rigName, PolecatName: "nux", ClonePath: filepath.Join(townRoot, "fake-polecat")}, nil
	}
	rollbackSlingArtifactsFn = func(*SpawnedPolecatInfo, string, string, string) { rolledBack = true }
	calls := stubPolecatWorkStatus(t, &git.UncommittedWorkStatus{UnpushedCommits: 1}, nil)

	err = runSling(nil, []string{"gt-abc123", "gastown"})

	if err == nil || !strings.Contains(err.Error(), "unpreserved work") {
		t.Fatalf("runSling error = %v, want unpreserved-work refusal", err)
	}
	if spawned {
		t.Error("a replacement polecat was spawned before the guard refused")
	}
	if rolledBack {
		t.Error("rollbackSlingArtifacts ran, which would clear the protected holder's hook")
	}
	if *calls != 1 {
		t.Errorf("work-status checks = %d, want 1", *calls)
	}
}
