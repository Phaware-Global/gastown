package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// stubGh puts a gh on PATH that runs the given shell script.
func stubGh(t *testing.T, script string) *Git {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\n"+script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://github.com/example/repo.git"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	return NewGit(repo)
}

func TestGhPrReviewThreads_LastReplyAuthor(t *testing.T) {
	g := stubGh(t, `cat <<'JSON'
{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[
{"id":"replied","comments":{"nodes":[{"author":{"login":"reviewer"}}]},"latest":{"totalCount":2,"nodes":[{"author":{"login":"worker"}}]}},
{"id":"unreplied","comments":{"nodes":[{"author":{"login":"worker"}}]},"latest":{"totalCount":1,"nodes":[{"author":{"login":"worker"}}]}}
]}}}}}
JSON
`)
	threads, err := g.GhPrReviewThreads(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 2 || threads[0].LastReplyAuthor != "worker" || threads[1].LastReplyAuthor != "" {
		t.Fatalf("threads = %+v, want a reply by worker then none", threads)
	}
}

func TestGhPrApprovedAtSHA(t *testing.T) {
	g := stubGh(t, `cat <<'JSON'
{"reviews":[
{"author":{"login":"current"},"state":"APPROVED","commit":{"oid":"abc"},"submittedAt":"2026-01-01T00:00:00Z"},
{"author":{"login":"current"},"state":"COMMENTED","commit":{"oid":"abc"},"submittedAt":"2026-01-02T00:00:00Z"},
{"author":{"login":"stale"},"state":"APPROVED","commit":{"oid":"old"},"submittedAt":"2026-01-01T00:00:00Z"},
{"author":{"login":"withdrew"},"state":"APPROVED","commit":{"oid":"abc"},"submittedAt":"2026-01-01T00:00:00Z"},
{"author":{"login":"withdrew"},"state":"CHANGES_REQUESTED","commit":{"oid":"abc"},"submittedAt":"2026-01-02T00:00:00Z"}
]}
JSON
`)
	for user, want := range map[string]bool{"Current": true, "stale": false, "withdrew": false, "absent": false} {
		got, err := g.GhPrApprovedAtSHA(1, user, "abc")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("GhPrApprovedAtSHA(%q) = %v, want %v", user, got, want)
		}
	}
}

// gh pr view spells a bot "app/name"; thread comments spell it "name".
func TestGhPrAuthor(t *testing.T) {
	g := stubGh(t, `echo '{"author":{"login":"app/worker"}}'`+"\n")
	got, err := g.GhPrAuthor(1)
	if err != nil || got != "worker" {
		t.Fatalf("GhPrAuthor = %q, %v; want worker", got, err)
	}
}
