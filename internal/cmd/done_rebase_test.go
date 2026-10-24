package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitpkg "github.com/steveyegge/gastown/internal/git"
)

// fakeRebaseGit lets us drive autoRebaseOnTarget without a real git repo for
// the gating-decision tests.
type fakeRebaseGit struct {
	rebaseErr   error
	onOrigin    bool
	originErr   error
	rebaseCalls int
	abortCalls  int
}

func (f *fakeRebaseGit) Rebase(onto string) error {
	f.rebaseCalls++
	return f.rebaseErr
}

func (f *fakeRebaseGit) PushRemoteBranchExists(remote, branch string) (bool, error) {
	return f.onOrigin, f.originErr
}

func (f *fakeRebaseGit) AbortRebase() error {
	f.abortCalls++
	return nil
}

// TestAutoRebaseOnTarget_GatingDecisions verifies the skip/rebase decision
// matrix (gh#3400). The behavior under test is the *decision*, not the actual
// git mechanics — those are exercised separately below against a real repo.
func TestAutoRebaseOnTarget_GatingDecisions(t *testing.T) {
	tests := []struct {
		name          string
		behind        int
		preVerified   bool
		alreadyPushed bool
		onOrigin      bool
		originErr     error
		directPush    bool
		wantRebased   bool
		wantSkip      string
		wantCalls     int
	}{
		{
			name:        "not behind: no-op",
			behind:      0,
			wantRebased: false,
			wantSkip:    "",
			wantCalls:   0,
		},
		{
			name:        "behind by 1: rebase runs",
			behind:      1,
			wantRebased: true,
			wantSkip:    "",
			wantCalls:   1,
		},
		{
			name:        "behind by 5: rebase runs",
			behind:      5,
			wantRebased: true,
			wantSkip:    "",
			wantCalls:   1,
		},
		{
			name:        "pre-verified: skip even when behind",
			behind:      3,
			preVerified: true,
			wantRebased: false,
			wantSkip:    "--pre-verified is set",
			wantCalls:   0,
		},
		{
			name:          "already pushed: skip to avoid divergence",
			behind:        3,
			alreadyPushed: true,
			wantRebased:   false,
			wantSkip:      "prior push checkpoint exists",
			wantCalls:     0,
		},
		{
			name:        "branch exists on origin: skip to keep history fast-forwardable",
			behind:      3,
			onOrigin:    true,
			wantRebased: false,
			wantSkip:    "branch already exists on origin",
			wantCalls:   0,
		},
		{
			name:        "direct push with branch on origin: still rebases so branch:main fast-forwards",
			behind:      3,
			onOrigin:    true,
			directPush:  true,
			wantRebased: true,
			wantCalls:   1,
		},
		{
			name:        "origin unreachable: skip rather than risk rewriting a published branch",
			behind:      3,
			originErr:   errors.New("network down"),
			wantRebased: false,
			wantSkip:    "could not check origin: network down",
			wantCalls:   0,
		},
		{
			name:          "pre-verified takes precedence over already-pushed",
			behind:        3,
			preVerified:   true,
			alreadyPushed: true,
			wantRebased:   false,
			wantSkip:      "--pre-verified is set",
			wantCalls:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeRebaseGit{onOrigin: tt.onOrigin, originErr: tt.originErr}
			rebased, skipReason, err := autoRebaseOnTarget(fake, "feature", "origin/main", tt.behind, tt.preVerified, tt.alreadyPushed, tt.directPush)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rebased != tt.wantRebased {
				t.Errorf("rebased = %v, want %v", rebased, tt.wantRebased)
			}
			if skipReason != tt.wantSkip {
				t.Errorf("skipReason = %q, want %q", skipReason, tt.wantSkip)
			}
			if fake.rebaseCalls != tt.wantCalls {
				t.Errorf("rebase calls = %d, want %d", fake.rebaseCalls, tt.wantCalls)
			}
			if fake.abortCalls != 0 {
				t.Errorf("abort calls = %d on success path, want 0", fake.abortCalls)
			}
		})
	}
}

// TestAutoRebaseOnTarget_ConflictAborts verifies that a rebase failure causes
// AbortRebase to fire and the returned error includes remediation guidance.
func TestAutoRebaseOnTarget_ConflictAborts(t *testing.T) {
	fake := &fakeRebaseGit{rebaseErr: errors.New("CONFLICT (content): merge conflict in foo.txt")}

	rebased, skipReason, err := autoRebaseOnTarget(fake, "feature", "origin/main", 1, false, false, false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if rebased {
		t.Error("rebased should be false on conflict")
	}
	if skipReason != "" {
		t.Errorf("skipReason should be empty on conflict, got %q", skipReason)
	}
	if fake.rebaseCalls != 1 {
		t.Errorf("expected 1 rebase call, got %d", fake.rebaseCalls)
	}
	if fake.abortCalls != 1 {
		t.Errorf("expected AbortRebase to fire on conflict, got %d calls", fake.abortCalls)
	}
	// Remediation guidance is part of the contract — agents read this message.
	msg := err.Error()
	if !strings.Contains(msg, "auto-rebase onto origin/main failed") {
		t.Errorf("error missing context: %q", msg)
	}
	if !strings.Contains(msg, "git rebase origin/main") {
		t.Errorf("error missing remediation hint: %q", msg)
	}
	if !strings.Contains(msg, "rerun gt done") {
		t.Errorf("error missing rerun hint: %q", msg)
	}
}

// TestAutoRebaseOnTarget_RealRepoSuccess exercises the rebase against a real
// git working tree to confirm the wiring (Rebase call) actually replays the
// branch onto a moved base. (gh#3400, scenario (a) from the bead notes.)
func TestAutoRebaseOnTarget_RealRepoSuccess(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	testRunGit(t, tmp, "init", "--initial-branch", "main", repo)
	testRunGit(t, repo, "config", "user.email", "test@test.com")
	testRunGit(t, repo, "config", "user.name", "Test")
	addEmptyOrigin(t, tmp, repo)

	// Initial commit on main.
	writeRepoFile(t, repo, "README.md", "# initial\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "initial")

	// Branch off and add a polecat commit on a non-conflicting file.
	testRunGit(t, repo, "checkout", "-b", "feature")
	writeRepoFile(t, repo, "feature.txt", "feature work\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "feature")

	// Move main forward independently — also non-conflicting with feature.
	testRunGit(t, repo, "checkout", "main")
	writeRepoFile(t, repo, "main-new.txt", "new on main\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "advance main")

	testRunGit(t, repo, "checkout", "feature")

	g := gitpkg.NewGit(repo)
	rebased, skipReason, err := autoRebaseOnTarget(g, "feature", "main", 1, false, false, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rebased {
		t.Fatalf("expected rebased=true, got false (skip=%q)", skipReason)
	}

	// After rebase, both files must be present and the feature commit must sit
	// on top of the advance-main commit.
	if _, statErr := os.Stat(filepath.Join(repo, "main-new.txt")); statErr != nil {
		t.Errorf("main-new.txt missing after rebase: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(repo, "feature.txt")); statErr != nil {
		t.Errorf("feature.txt missing after rebase: %v", statErr)
	}
}

// TestAutoRebaseOnTarget_RealRepoConflictAborts exercises the conflict path
// against a real git working tree: feature and main both touch the same file,
// rebase fails with a CONFLICT, and AbortRebase must restore the working tree
// so the polecat can address the conflict manually. (gh#3400, scenario (b).)
func TestAutoRebaseOnTarget_RealRepoConflictAborts(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	testRunGit(t, tmp, "init", "--initial-branch", "main", repo)
	testRunGit(t, repo, "config", "user.email", "test@test.com")
	testRunGit(t, repo, "config", "user.name", "Test")
	addEmptyOrigin(t, tmp, repo)

	writeRepoFile(t, repo, "shared.txt", "v0\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "initial")

	// Feature changes shared.txt to v1.
	testRunGit(t, repo, "checkout", "-b", "feature")
	writeRepoFile(t, repo, "shared.txt", "v1-feature\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "feature edit")

	// Main changes shared.txt to a different value — guaranteed conflict.
	testRunGit(t, repo, "checkout", "main")
	writeRepoFile(t, repo, "shared.txt", "v1-main\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "main edit")

	testRunGit(t, repo, "checkout", "feature")

	g := gitpkg.NewGit(repo)
	rebased, skipReason, err := autoRebaseOnTarget(g, "feature", "main", 1, false, false, false)
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if rebased {
		t.Error("rebased should be false on conflict")
	}
	if skipReason != "" {
		t.Errorf("skipReason should be empty on conflict, got %q", skipReason)
	}

	// AbortRebase is required to leave the working tree in a clean state. After
	// abort, the rebase-merge dir must be gone — otherwise the polecat is stuck
	// in a half-rebased state.
	if _, statErr := os.Stat(filepath.Join(repo, ".git", "rebase-merge")); !os.IsNotExist(statErr) {
		t.Errorf(".git/rebase-merge should not exist after abort (stat err: %v)", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(repo, ".git", "rebase-apply")); !os.IsNotExist(statErr) {
		t.Errorf(".git/rebase-apply should not exist after abort (stat err: %v)", statErr)
	}
}

// addEmptyOrigin wires repo to a local bare origin that has no branches, so
// the branch under test is not on origin and a rebase is permitted.
func addEmptyOrigin(t *testing.T, tmp, repo string) {
	t.Helper()
	origin := filepath.Join(tmp, "origin.git")
	testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", origin)
	testRunGit(t, repo, "remote", "add", "origin", origin)
}

func writeRepoFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestAutoRebaseOnTarget_BranchOnOriginIsNotRewritten: a
// branch that already exists on origin (the head of an open PR) must not be
// rebased onto a newer base, because the rewritten history no longer
// fast-forwards from origin and the push is rejected. All repos are temp
// directories; the "origin" is a local bare repo.
func TestAutoRebaseOnTarget_BranchOnOriginIsNotRewritten(t *testing.T) {
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	repo := filepath.Join(tmp, "repo")
	testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", origin)
	testRunGit(t, tmp, "init", "--initial-branch", "main", repo)
	testRunGit(t, repo, "config", "user.email", "test@test.com")
	testRunGit(t, repo, "config", "user.name", "Test")
	testRunGit(t, repo, "remote", "add", "origin", origin)

	writeRepoFile(t, repo, "README.md", "# initial\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "initial")
	testRunGit(t, repo, "push", "origin", "main")

	// Feature branch is pushed to origin (the open PR's head).
	testRunGit(t, repo, "checkout", "-b", "feature")
	writeRepoFile(t, repo, "feature.txt", "feature work\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "feature")
	testRunGit(t, repo, "push", "origin", "feature")

	// Base moves forward; the polecat adds one more local commit on top.
	testRunGit(t, repo, "checkout", "main")
	writeRepoFile(t, repo, "main-new.txt", "new on main\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "advance main")
	testRunGit(t, repo, "push", "origin", "main")
	testRunGit(t, repo, "checkout", "feature")
	writeRepoFile(t, repo, "feature2.txt", "more feature work\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "feature 2")

	g := gitpkg.NewGit(repo)
	headBefore, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	rebased, skipReason, err := autoRebaseOnTarget(g, "feature", "origin/main", 1, false, false, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rebased {
		t.Fatal("rebased = true for a branch that already exists on origin; history was rewritten")
	}
	if skipReason == "" {
		t.Error("expected a skip reason for a branch that exists on origin")
	}
	headAfter, _ := g.Rev("HEAD")
	if headAfter != headBefore {
		t.Errorf("HEAD moved from %s to %s", headBefore, headAfter)
	}

	// The push must fast-forward without force.
	testRunGit(t, repo, "push", "origin", "feature")
}

// TestAutoRebaseOnTarget_BranchOnlyOnPushURLIsNotRewritten covers a remote
// whose pushurl differs from its fetch url (fork workflow): the branch exists
// only on the push target, which is where the next push goes, so it must not
// be rebased even though ls-remote against the fetch url finds nothing.
func TestAutoRebaseOnTarget_BranchOnlyOnPushURLIsNotRewritten(t *testing.T) {
	tmp := t.TempDir()
	fetchRemote := filepath.Join(tmp, "upstream.git")
	pushRemote := filepath.Join(tmp, "fork.git")
	repo := filepath.Join(tmp, "repo")
	testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", fetchRemote)
	testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", pushRemote)
	testRunGit(t, tmp, "init", "--initial-branch", "main", repo)
	testRunGit(t, repo, "config", "user.email", "test@test.com")
	testRunGit(t, repo, "config", "user.name", "Test")
	testRunGit(t, repo, "remote", "add", "origin", fetchRemote)
	testRunGit(t, repo, "remote", "set-url", "--push", "origin", pushRemote)

	writeRepoFile(t, repo, "README.md", "# initial\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "initial")

	testRunGit(t, repo, "checkout", "-b", "feature")
	writeRepoFile(t, repo, "feature.txt", "feature work\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "feature")
	testRunGit(t, repo, "push", "origin", "feature") // lands on the push url only

	testRunGit(t, repo, "checkout", "main")
	writeRepoFile(t, repo, "main-new.txt", "new on main\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "advance main")
	testRunGit(t, repo, "checkout", "feature")

	g := gitpkg.NewGit(repo)
	headBefore, _ := g.Rev("HEAD")
	rebased, skipReason, err := autoRebaseOnTarget(g, "feature", "main", 1, false, false, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rebased {
		t.Fatal("rebased = true for a branch that exists on the push url")
	}
	if skipReason != "branch already exists on origin" {
		t.Errorf("skipReason = %q", skipReason)
	}
	if headAfter, _ := g.Rev("HEAD"); headAfter != headBefore {
		t.Errorf("HEAD moved from %s to %s", headBefore, headAfter)
	}
}

// TestAutoRebaseOnTarget_DirectPushRebasesPublishedBranch: under the direct
// merge strategy gt done pushes branch:main, and a branch on origin does not
// back a PR. Skipping the rebase would make that push non-fast-forward, so the
// rebase must still run and the push to main must fast-forward without force.
func TestAutoRebaseOnTarget_DirectPushRebasesPublishedBranch(t *testing.T) {
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	repo := filepath.Join(tmp, "repo")
	testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", origin)
	testRunGit(t, tmp, "init", "--initial-branch", "main", repo)
	testRunGit(t, repo, "config", "user.email", "test@test.com")
	testRunGit(t, repo, "config", "user.name", "Test")
	testRunGit(t, repo, "remote", "add", "origin", origin)

	writeRepoFile(t, repo, "README.md", "# initial\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "initial")
	testRunGit(t, repo, "push", "origin", "main")

	testRunGit(t, repo, "checkout", "-b", "feature")
	writeRepoFile(t, repo, "feature.txt", "feature work\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "feature")
	testRunGit(t, repo, "push", "origin", "feature")

	testRunGit(t, repo, "checkout", "main")
	writeRepoFile(t, repo, "main-new.txt", "new on main\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "advance main")
	testRunGit(t, repo, "push", "origin", "main")
	testRunGit(t, repo, "checkout", "feature")

	g := gitpkg.NewGit(repo)
	rebased, skipReason, err := autoRebaseOnTarget(g, "feature", "origin/main", 1, false, false, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rebased {
		t.Fatalf("expected rebase under direct push, got skip=%q", skipReason)
	}
	testRunGit(t, repo, "push", "origin", "feature:main")
}
