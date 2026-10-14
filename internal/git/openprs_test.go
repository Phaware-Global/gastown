package git

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func commitFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", name)
	runGit(t, dir, "commit", "-m", "add "+name)
}

// A branch rebased onto a moved target gets new SHAs but keeps its patches, so
// patch-ids must match where SHAs no longer do.
func TestPatchIDsInRange_SurvivesRebase(t *testing.T) {
	dir := t.TempDir()
	g := NewGit(dir)
	runGit(t, dir, "init", "--initial-branch", "main")
	runGit(t, dir, "config", "user.email", "t@t.com")
	runGit(t, dir, "config", "user.name", "T")
	commitFile(t, dir, "base.txt", "base\n")

	runGit(t, dir, "checkout", "-b", "pr")
	commitFile(t, dir, "a1.txt", "a1\n")
	commitFile(t, dir, "a2.txt", "a2\n")
	runGit(t, dir, "branch", "copy")
	runGit(t, dir, "checkout", "copy")
	commitFile(t, dir, "mine.txt", "mine\n")

	before, err := g.PatchIDsInRange("main", "pr")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 {
		t.Fatalf("pr patch-ids = %v, want 2", before)
	}

	// Target moves on, then the copy is rebased onto it.
	runGit(t, dir, "checkout", "main")
	commitFile(t, dir, "moved.txt", "moved\n")
	runGit(t, dir, "checkout", "copy")
	runGit(t, dir, "rebase", "main")

	copyIDs, err := g.PatchIDsInRange("main", "copy")
	if err != nil {
		t.Fatal(err)
	}
	copyShas, _ := g.run("rev-list", "main..copy")
	set := map[string]bool{}
	for _, id := range copyIDs {
		set[id] = true
	}
	for _, id := range before {
		if !set[id] {
			t.Fatalf("patch-id %s from pr missing after rebase; copy ids = %v", id, copyIDs)
		}
	}
	if len(copyIDs) != 3 {
		t.Fatalf("copy patch-ids = %v, want 3", copyIDs)
	}
	// Sanity: the SHAs really did change (otherwise this test proves nothing).
	prList, _ := g.run("rev-list", "pr")
	shared := 0
	prSet := map[string]bool{}
	for _, s := range strings.Fields(prList) {
		prSet[s] = true
	}
	for _, s := range strings.Fields(copyShas) {
		if prSet[s] {
			shared++
		}
	}
	if shared != 0 {
		t.Fatalf("expected rebase to rewrite SHAs, %d still shared", shared)
	}
}

func TestPatchIDsInRange_EmptyRange(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	ids, err := g.PatchIDsInRange("HEAD", "HEAD")
	if err != nil || len(ids) != 0 {
		t.Fatalf("ids = %v, err = %v; want none", ids, err)
	}
}

// A gh that never returns, with a grandchild holding stdout, must not hang
// ListOpenPRs past its deadline.
func TestListOpenPRs_TimesOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	bin := t.TempDir()
	stub := "#!/bin/sh\nsleep 30 &\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(stub), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	g := NewGit(t.TempDir())
	start := time.Now()
	_, err := g.listOpenPRs(200 * time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("listOpenPRs took %v; deadline not enforced", elapsed)
	}
}
