package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/tmux"
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

// setupHookedBeadTown builds a town whose bead gt-abc123 is hooked to
// gastown/polecats/toast, which has a worktree, and chdirs into it.
func setupHookedBeadTown(t *testing.T) string {
	t.Helper()
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
	return townRoot
}

// TestSlingForceRigTargetRefusedBeforeSpawn: the guard must refuse a forced
// re-sling to a rig before resolveTarget spawns a replacement. Refusing after
// the spawn rolls the replacement back through rollbackSlingArtifacts, which
// clears the protected holder's hook and molecule.
func TestSlingForceRigTargetRefusedBeforeSpawn(t *testing.T) {
	townRoot := setupHookedBeadTown(t)
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

	err := runSling(nil, []string{"gt-abc123", "gastown"})

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

// TestSlingDeadHolderTargetStillGuarded: targeting the holder only keeps its
// worktree while its session is alive. When the session is dead,
// resolveTarget spawns a fresh polecat and the force block then shuts the
// holder down, so the guard must still run.
func TestSlingDeadHolderTargetStillGuarded(t *testing.T) {
	townRoot := setupHookedBeadTown(t)

	prevForce := slingForce
	prevDryRun := slingDryRun
	prevNoConvoy := slingNoConvoy
	prevNoBoot := slingNoBoot
	prevSpawn := spawnPolecatForSling
	prevResolve := resolveTargetAgentFn
	prevAlive := isHookedAgentConfirmedAliveFn
	t.Cleanup(func() {
		slingForce = prevForce
		slingDryRun = prevDryRun
		slingNoConvoy = prevNoConvoy
		slingNoBoot = prevNoBoot
		spawnPolecatForSling = prevSpawn
		resolveTargetAgentFn = prevResolve
		isHookedAgentConfirmedAliveFn = prevAlive
	})
	slingForce = false
	slingDryRun = false
	slingNoConvoy = true
	slingNoBoot = true

	isHookedAgentConfirmedAliveFn = func(string) bool { return false }
	resolveTargetAgentFn = func(string) (string, string, string, error) {
		return "", "", "", errors.New("simulated dead session")
	}
	spawned := false
	spawnPolecatForSling = func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		spawned = true
		return &SpawnedPolecatInfo{RigName: rigName, PolecatName: "nux", ClonePath: filepath.Join(townRoot, "fake-polecat")}, nil
	}
	calls := stubPolecatWorkStatus(t, &git.UncommittedWorkStatus{UnpushedCommits: 1}, nil)

	err := runSling(nil, []string{"gt-abc123", "gastown/polecats/toast"})

	if err == nil || !strings.Contains(err.Error(), "unpreserved work") {
		t.Fatalf("runSling error = %v, want unpreserved-work refusal", err)
	}
	if spawned {
		t.Error("a replacement polecat was spawned for a dead holder with unpushed work")
	}
	if *calls != 1 {
		t.Errorf("work-status checks = %d, want 1", *calls)
	}
}

// TestSlingUnknownLivenessHolderTargetStillGuarded: a tmux error that is not
// "session not found" leaves the holder's liveness unknown. resolveTarget
// fails on the same error and spawns a replacement, so the guard must run
// instead of treating the holder as live.
func TestSlingUnknownLivenessHolderTargetStillGuarded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux stub is a shell script")
	}
	townRoot := setupHookedBeadTown(t)

	tmuxStub := "#!/bin/sh\necho 'unexpected tmux failure' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(townRoot, "bin", "tmux"), []byte(tmuxStub), 0o755); err != nil {
		t.Fatal(err)
	}
	if alive, err := tmux.NewTmux().HasSession("gt-gastown-toast"); err == nil || alive {
		t.Fatalf("stub must make liveness unknown, got alive=%v err=%v", alive, err)
	}

	prevForce := slingForce
	prevDryRun := slingDryRun
	prevNoConvoy := slingNoConvoy
	prevNoBoot := slingNoBoot
	prevSpawn := spawnPolecatForSling
	prevResolve := resolveTargetAgentFn
	t.Cleanup(func() {
		slingForce = prevForce
		slingDryRun = prevDryRun
		slingNoConvoy = prevNoConvoy
		slingNoBoot = prevNoBoot
		spawnPolecatForSling = prevSpawn
		resolveTargetAgentFn = prevResolve
	})
	slingForce = true
	slingDryRun = false
	slingNoConvoy = true
	slingNoBoot = true

	resolveTargetAgentFn = func(string) (string, string, string, error) {
		return "", "", "", errors.New("simulated tmux failure")
	}
	spawned := false
	spawnPolecatForSling = func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		spawned = true
		return &SpawnedPolecatInfo{RigName: rigName, PolecatName: "nux", ClonePath: filepath.Join(townRoot, "fake-polecat")}, nil
	}
	calls := stubPolecatWorkStatus(t, &git.UncommittedWorkStatus{UnpushedCommits: 1}, nil)

	err := runSling(nil, []string{"gt-abc123", "gastown/polecats/toast"})

	if err == nil || !strings.Contains(err.Error(), "unpreserved work") {
		t.Fatalf("runSling error = %v, want unpreserved-work refusal", err)
	}
	if spawned {
		t.Error("a replacement polecat was spawned for a holder of unknown liveness")
	}
	if *calls != 1 {
		t.Errorf("work-status checks = %d, want 1", *calls)
	}
}

// TestIsHookedAgentConfirmedAlive: only a positive has-session answer counts
// as alive; an unknown session name or a tmux error fails closed.
func TestIsHookedAgentConfirmedAlive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux stub is a shell script")
	}
	binDir := t.TempDir()
	write := func(script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	const holder = "gastown/polecats/toast"

	write("#!/bin/sh\nexit 0\n")
	if !isHookedAgentConfirmedAlive(holder) {
		t.Error("existing session must be confirmed alive")
	}
	write("#!/bin/sh\necho \"can't find session\" >&2\nexit 1\n")
	if isHookedAgentConfirmedAlive(holder) {
		t.Error("missing session must not be confirmed alive")
	}
	write("#!/bin/sh\necho 'unexpected tmux failure' >&2\nexit 1\n")
	if isHookedAgentConfirmedAlive(holder) {
		t.Error("tmux error must not be confirmed alive")
	}
	write("#!/bin/sh\nexit 0\n")
	if isHookedAgentConfirmedAlive("gastown") {
		t.Error("unresolvable session name must not be confirmed alive")
	}
}
