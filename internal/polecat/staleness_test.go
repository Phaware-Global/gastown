package polecat

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/templates"
)

// A failed probe means staleness is unknown. Treating it as "clean tree, no agent
// bead" would let the daemon's cap-relief cleanup nuke a polecat it merely failed to
// inspect (git error, Dolt timeout), bypassing the protected agent states.
func TestAssessStaleness_ProbeErrorFailsClosed(t *testing.T) {
	info := &StalenessInfo{
		Name:       "p1",
		ProbeError: "reading agent bead: timeout",
		// Would otherwise be stale: no session, no bead, way behind main.
		CommitsBehind: 100,
	}
	stale, reason := assessStaleness(info, 20)
	if stale {
		t.Fatalf("assessStaleness marked a polecat with a failed probe stale (%q)", reason)
	}
	if !strings.Contains(reason, "probe failed") || !strings.Contains(reason, "timeout") {
		t.Errorf("reason = %q, want it to name the failed probe", reason)
	}
}

func TestAssessStaleness_NoProbeErrorStillStale(t *testing.T) {
	stale, _ := assessStaleness(&StalenessInfo{Name: "p1"}, 20)
	if !stale {
		t.Fatal("session-less polecat with no bead and no probe error should be stale")
	}
}

// Reuse writes agent_state=spawning under the per-polecat lock, but the hook bead
// that moves State/Issue lands afterwards, outside it. In that gap the polecat reads
// idle with no issue (same as at detection), so only the agent state shows the reuse.
func TestApplyRedispatchCheck(t *testing.T) {
	tests := []struct {
		name      string
		prevAgent string
		agent     string
		p         Polecat
		wantStale bool
	}{
		{"reused, hook not yet landed", "idle", "spawning", Polecat{State: StateIdle}, false},
		{"became working since detection", "idle", "working", Polecat{State: StateIdle}, false},
		{"hooked to a different issue", "idle", "idle", Polecat{State: StateIdle, Issue: "gt-new"}, false},
		{"state working", "idle", "idle", Polecat{State: StateWorking}, false},
		{"still idle and unchanged", "idle", "idle", Polecat{State: StateIdle}, true},
		{"no agent bead", "", "", Polecat{State: StateIdle}, true},
		// A dead session leaves agent_state=working behind; it is not a reuse.
		{"working left behind by a dead session", "working", "working", Polecat{State: StateIdle}, true},
		{"running left behind by a dead session", "running", "running", Polecat{State: StateIdle}, true},
		// Reuse writes spawning before anything else changes, so it always protects,
		// even when the stale leftover state was already spawning.
		{"leftover working, then reused", "working", "spawning", Polecat{State: StateIdle}, false},
		{"spawning at detection and now", "spawning", "spawning", Polecat{State: StateIdle}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := &StalenessInfo{Name: "p1", AgentState: tt.agent, IsStale: true, Reason: "stale"}
			applyRedispatchCheck(info, &tt.p, &StalenessInfo{Name: "p1", AgentState: tt.prevAgent})
			if info.IsStale != tt.wantStale {
				t.Errorf("IsStale = %v, want %v (reason %q)", info.IsStale, tt.wantStale, info.Reason)
			}
		})
	}
}

// A polecats/<name> dir whose worktree is missing or has a dangling gitdir passes
// List() but fails every git probe. It holds no work to protect and still counts
// toward the dir cap, so it must stay reclaimable rather than read as "probe failed".
func TestProbeUncommittedWork_MissingWorktreeIsNotAProbeFailure(t *testing.T) {
	dangling := t.TempDir()
	if err := os.WriteFile(filepath.Join(dangling, ".git"), []byte("gitdir: "+filepath.Join(dangling, "gone", "worktrees", "x")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"clone path does not exist": filepath.Join(t.TempDir(), "nope"),
		"dir without .git":          t.TempDir(),
		"dangling gitdir file":      dangling,
	}
	for name, path := range tests {
		t.Run(name, func(t *testing.T) {
			has, probeErr := probeUncommittedWork(path)
			if probeErr != "" || has {
				t.Errorf("probeUncommittedWork = (%v, %q), want (false, \"\")", has, probeErr)
			}
		})
	}
}

func TestProbeUncommittedWork_RealRepo(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "i"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if has, probeErr := probeUncommittedWork(dir); has || probeErr != "" {
		t.Fatalf("clean repo = (%v, %q), want (false, \"\")", has, probeErr)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if has, probeErr := probeUncommittedWork(dir); !has || probeErr != "" {
		t.Fatalf("dirty repo = (%v, %q), want (true, \"\")", has, probeErr)
	}
}

// Gas Town writes these into every polecat worktree. In a repo that does not ignore
// them they are untracked forever, so counting them as work made every polecat of the
// rig unreclaimable and left the rig pinned at its directory cap.
func TestProbeUncommittedWork_IgnoresProvisionedFilesOnly(t *testing.T) {
	newRepo := func(t *testing.T) string {
		dir := t.TempDir()
		write := func(rel string) {
			path := filepath.Join(dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("testdata/fixture.db")
		for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "i"}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		for _, f := range []string{".beads/x.jsonl", ".runtime/session_id", ".claude/commands/" + templates.CommandNames()[0] + ".md", "CLAUDE.local.md"} {
			write(f)
		}
		return dir
	}

	if has, probeErr := probeUncommittedWork(newRepo(t)); has || probeErr != "" {
		t.Fatalf("provisioned files only = (%v, %q), want (false, \"\")", has, probeErr)
	}

	// Real work must still protect the polecat, including under paths gt done's
	// broader runtime-artifact list would skip.
	for _, rel := range []string{
		"notes.txt",
		".claude/skills/new/SKILL.md",
		".claude/commands/my-own-command.md",
		"internal/coverage/report.go",
		"docs/CLAUDE.local.md",
		"testdata/fixture.db", // tracked file, modified
	} {
		t.Run(rel, func(t *testing.T) {
			dir := newRepo(t)
			path := filepath.Join(dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
				t.Fatal(err)
			}
			if has, probeErr := probeUncommittedWork(dir); !has || probeErr != "" {
				t.Fatalf("with %s = (%v, %q), want (true, \"\")", rel, has, probeErr)
			}
		})
	}
}

// A present but unreadable repository still fails closed.
func TestProbeUncommittedWork_BrokenRepoFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, probeErr := probeUncommittedWork(dir); probeErr == "" {
		t.Fatal("a .git dir that git cannot read must report a probe error")
	}
}

// A polecat with uncommitted work is never stale, whatever its beads say, so the
// scan must not spend bd calls on it. The daemon rescans a rig stuck at its
// directory cap on every heartbeat, and such polecats are most of that rig.
func TestDetectStalePolecats_SkipsBeadLookupsWhenGitRulesOutStaleness(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a sh mock bd")
	}
	binDir := t.TempDir()
	bdLog := filepath.Join(binDir, "bd.log")
	script := `#!/bin/sh
echo "$*" >> "` + bdLog + `"
for arg in "$@"; do
  case "$arg" in
    show) printf '[{"id":"x","title":"agent","issue_type":"agent","description":"agent\\n\\nrole_type: polecat\\nagent_state: idle"}]\n'; exit 0 ;;
  esac
done
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	rigPath := filepath.Join(t.TempDir(), "testrig")
	for _, name := range []string{"clean", "dirty"} {
		dir := filepath.Join(rigPath, "polecats", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "i"}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(rigPath, "polecats", "dirty", "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(&rig.Rig{Name: "testrig", Path: rigPath}, git.NewGit(rigPath), nil)
	if err := os.WriteFile(bdLog, nil, 0o644); err != nil { // drop any calls made by setup
		t.Fatal(err)
	}
	infos, err := mgr.DetectStalePolecats(20)
	if err != nil {
		t.Fatalf("DetectStalePolecats: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("got %d infos, want 2", len(infos))
	}
	clean, dirty := infos[0], infos[1]

	if dirty.IsStale || !dirty.HasUncommittedWork || !dirty.BeadsUnchecked {
		t.Errorf("dirty = %+v, want not stale, uncommitted work, beads unchecked", dirty)
	}
	if !clean.IsStale || clean.BeadsUnchecked || clean.AgentState != "idle" {
		t.Errorf("clean = %+v, want stale with agent state read from its bead", clean)
	}

	logged, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), "dirty") {
		t.Errorf("bd was called for the dirty polecat:\n%s", logged)
	}
	if !strings.Contains(string(logged), "clean") {
		t.Errorf("bd was not called for the clean polecat:\n%s", logged)
	}
}
