package polecat

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
		agent     string
		p         Polecat
		wantStale bool
	}{
		{"reused, hook not yet landed", "spawning", Polecat{State: StateIdle}, false},
		{"working agent state", "working", Polecat{State: StateIdle}, false},
		{"hooked to a different issue", "idle", Polecat{State: StateIdle, Issue: "gt-new"}, false},
		{"state working", "idle", Polecat{State: StateWorking}, false},
		{"still idle and unchanged", "idle", Polecat{State: StateIdle}, true},
		{"no agent bead", "", Polecat{State: StateIdle}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := &StalenessInfo{Name: "p1", AgentState: tt.agent, IsStale: true, Reason: "stale"}
			applyRedispatchCheck(info, &tt.p, &StalenessInfo{Name: "p1"})
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
