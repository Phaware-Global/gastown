package version

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestShortCommit(t *testing.T) {
	tests := []struct {
		name   string
		hash   string
		expect string
	}{
		{"full SHA", "abcdef1234567890abcdef1234567890abcdef12", "abcdef123456"},
		{"exactly 12", "abcdef123456", "abcdef123456"},
		{"short hash", "abcdef", "abcdef"},
		{"empty", "", ""},
		{"13 chars", "abcdef1234567", "abcdef123456"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShortCommit(tt.hash)
			if got != tt.expect {
				t.Errorf("ShortCommit(%q) = %q, want %q", tt.hash, got, tt.expect)
			}
		})
	}
}

func TestCommitsMatch(t *testing.T) {
	tests := []struct {
		name   string
		a, b   string
		expect bool
	}{
		{"identical full", "abcdef1234567890", "abcdef1234567890", true},
		{"prefix match short-long", "abcdef1234567", "abcdef1234567890abcd", true},
		{"prefix match long-short", "abcdef1234567890abcd", "abcdef1234567", true},
		{"no match", "abcdef1234567", "1234567abcdef", false},
		{"too short a", "abc", "abcdef1234567", false},
		{"too short b", "abcdef1234567", "abc", false},
		{"both too short", "abc", "abc", false},
		{"exactly 7 chars match", "abcdefg", "abcdefg", true},
		{"exactly 7 chars no match", "abcdefg", "abcdefh", false},
		{"6 chars too short", "abcdef", "abcdef", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := commitsMatch(tt.a, tt.b)
			if got != tt.expect {
				t.Errorf("commitsMatch(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.expect)
			}
		})
	}
}

func TestSetCommit(t *testing.T) {
	original := Commit
	defer func() { Commit = original }()

	SetCommit("abc123def456")
	if Commit != "abc123def456" {
		t.Errorf("SetCommit did not set Commit; got %q", Commit)
	}
}

func TestIsBuildBranch(t *testing.T) {
	tests := []struct {
		branch string
		want   bool
	}{
		{"main", true},
		{"master", true},
		{"carry/operational", true},
		{"carry/staging", true},
		{"carry/", true},
		{"fix/something", false},
		{"feat/new-thing", false},
		{"develop", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.branch, func(t *testing.T) {
			if got := isBuildBranch(tt.branch); got != tt.want {
				t.Errorf("isBuildBranch(%q) = %v, want %v", tt.branch, got, tt.want)
			}
		})
	}
}

func TestCheckStaleBinary_NoCommit(t *testing.T) {
	original := Commit
	defer func() { Commit = original }()

	Commit = ""
	// Force resolveCommitHash to return empty by clearing Commit
	// (vcs.revision from build info may still be set, so this test
	// verifies the error path when no commit is available)
	info := CheckStaleBinary(t.TempDir())
	if info == nil {
		t.Fatal("CheckStaleBinary returned nil")
	}
	// Either we get an error (no commit) or we get a valid result from build info
	// Both are acceptable outcomes
	if info.BinaryCommit == "" && info.Error == nil {
		t.Error("expected error when binary commit is empty")
	}
}

// TestCheckStaleBinary_BinaryAhead verifies that when the installed binary's
// commit is a DESCENDANT of the repo HEAD (the binary is newer than the source
// checkout it's run against), it is NOT reported as stale. This reproduces the
// false-positive that misled the mayor: the binary was current with main while
// the workspace's gastown checkout lagged behind, and the old logic warned
// "stale" + advised a make install that would have downgraded the binary.
func TestCheckStaleBinary_BinaryAhead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	original := Commit
	defer func() { Commit = original }()

	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "commit A")
	headCommit := git("rev-parse", "HEAD") // older — will be repo HEAD

	// Second commit (non-.beads change) becomes the "binary" commit (newer).
	if err := os.WriteFile(filepath.Join(repo, "b.go"), []byte("package a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "commit B")
	binaryCommit := git("rev-parse", "HEAD")

	// Move repo HEAD back to A, so HEAD (A) is an ancestor of the binary (B).
	git("reset", "-q", "--hard", headCommit)

	Commit = binaryCommit
	info := CheckStaleBinary(repo)
	if info.Error != nil {
		t.Fatalf("unexpected error: %v", info.Error)
	}
	if info.IsStale {
		t.Errorf("IsStale = true, want false (binary is ahead of checkout, not stale)")
	}
	if !info.BinaryAhead {
		t.Errorf("BinaryAhead = false, want true (binary commit is a descendant of HEAD)")
	}
}
