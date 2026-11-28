package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/reviewer"
)

// stubReviewerPasses points the pass gate at a temp dir with a fixed set of
// enabled perspectives.
func stubReviewerPasses(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	prevNames, prevDir := reviewerEnabledPerspectives, reviewerPassDir
	reviewerEnabledPerspectives = func() ([]string, error) { return names, nil }
	reviewerPassDir = dir
	t.Cleanup(func() { reviewerEnabledPerspectives, reviewerPassDir = prevNames, prevDir })
	return dir
}

func runStubPass(t *testing.T, dir, name, sha, answer string) {
	t.Helper()
	spec := reviewer.PassSpec{Perspective: name, PR: 7, SHA: sha, Round: 1, Prompt: "p"}
	_, err := reviewer.RunPass(context.Background(), dir, spec, func(context.Context, string) (string, error) {
		time.Sleep(time.Millisecond)
		return answer, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGateReviewPost(t *testing.T) {
	const sha = "0123456789abcdef"
	const answer = `{"perspective":"security","verdict":"one issue","findings":[{"path":"a.go","line":3,"priority":"high","title":"t","body":"b"}]}`

	tests := []struct {
		name    string
		tamper  func(t *testing.T, dir string, fs *reviewer.Findings)
		shaFlag string
		wantErr string
	}{
		{name: "consolidated payload posts"},
		{name: "invented finding", tamper: func(_ *testing.T, _ string, fs *reviewer.Findings) {
			fs.Findings = append(fs.Findings, reviewer.Finding{Path: "b.go", Line: 1, Priority: "low", Title: "x"})
		}, wantErr: "no raw source"},
		{name: "finding citing a raw id that does not exist", tamper: func(_ *testing.T, _ string, fs *reviewer.Findings) {
			fs.Findings[0].Sources = []string{"security:2"}
		}, wantErr: "no raw source"},
		{name: "finding silently removed", tamper: func(_ *testing.T, _ string, fs *reviewer.Findings) {
			fs.Findings = nil
		}, wantErr: "is not what"},
		{name: "downgraded priority", tamper: func(_ *testing.T, _ string, fs *reviewer.Findings) {
			fs.Findings[0].Priority = "low"
		}, wantErr: "is not what"},
		{name: "missing receipt", tamper: func(t *testing.T, dir string, _ *reviewer.Findings) {
			if err := os.Remove(filepath.Join(dir, "receipt-security.json")); err != nil {
				t.Fatal(err)
			}
		}, wantErr: "no receipt"},
		{name: "result edited after consolidate", tamper: func(t *testing.T, dir string, _ *reviewer.Findings) {
			if err := os.WriteFile(filepath.Join(dir, "perspective-security.json"), []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}, wantErr: "result hash mismatch"},
		{name: "no reviewed_sha", tamper: func(_ *testing.T, _ string, fs *reviewer.Findings) {
			fs.ReviewedSHA = ""
		}, wantErr: "no reviewed_sha"},
		{name: "other --sha", shaFlag: "fedcba9876543210", wantErr: "differs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := stubReviewerPasses(t, "security")
			runStubPass(t, dir, "security", sha, answer)
			supplied, err := consolidateVerifiedPasses(7, sha, "", &reviewer.Edits{})
			if err != nil {
				t.Fatal(err)
			}
			if tc.tamper != nil {
				tc.tamper(t, dir, supplied)
			}
			got, err := gateReviewPost(7, tc.shaFlag, supplied)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(got.SummaryBody(sha), "pass finished ") {
					t.Errorf("body lacks the pass end time:\n%s", got.SummaryBody(sha))
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestReviewerPassSettingsKeepsOnlyGuards(t *testing.T) {
	got, err := reviewerPassSettings([]byte(`{"hooks":{"PreToolUse":[{"matcher":"x"}],"SessionStart":[{"matcher":"y"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "PreToolUse") || strings.Contains(got, "SessionStart") {
		t.Errorf("settings = %s", got)
	}
	if _, err := reviewerPassSettings([]byte(`{"hooks":{}}`)); err == nil {
		t.Error("settings without guards were accepted")
	}
}

func TestClaudePassArgs(t *testing.T) {
	args := claudePassArgs([]string{"--dangerously-skip-permissions", "--settings", "/role.json"}, "{}")
	if slices.Contains(args, "/role.json") {
		t.Errorf("role settings not replaced: %v", args)
	}
	i := slices.Index(args, "--setting-sources")
	if i < 0 || args[i+1] != "user" {
		t.Errorf("project settings must not load: %v", args)
	}
}

func TestClaudeAnswer(t *testing.T) {
	if got, err := claudeAnswer([]byte(`{"subtype":"success","result":"ok"}`)); err != nil || got != "ok" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range []string{`{"subtype":"error_max_turns"}`, `{"subtype":"success","is_error":true}`, `junk`} {
		if _, err := claudeAnswer([]byte(bad)); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
}

func TestGateReviewPostCarried(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(file, content string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, file), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", file)
		git("commit", "-qm", file)
		return git("rev-parse", "HEAD")
	}
	git("init", "-q", "-b", "main")
	commit("shared.txt", "1\n2\n3\n4\n5\n6\n7\n8\n")
	git("checkout", "-qb", "pr")
	reviewed := commit("shared.txt", "1\n2\n3\n4\n5\n6\nPR\n8\n")

	// head returns the PR head after the given change on top of the reviewed one.
	head := func(name string, change func()) string {
		git("checkout", "-q", "main")
		git("checkout", "-qB", name, reviewed)
		change()
		return git("rev-parse", "HEAD")
	}
	mergeMain := func(file, content string) func() {
		return func() {
			git("checkout", "-q", "main")
			git("reset", "-q", "--hard", git("merge-base", reviewed, "main"))
			commit(file, content)
			git("checkout", "-q", "-")
			git("merge", "-q", "--no-edit", "main")
		}
	}

	tests := []struct {
		name    string
		change  func()
		wantErr string
	}{
		{name: "merge of base touching another file", change: mergeMain("other.txt", "o\n")},
		{name: "merge of base editing the same file without moving the PR's lines",
			change: mergeMain("shared.txt", "ONE\n2\n3\n4\n5\n6\n7\n8\n")},
		{name: "merge of base that moves the PR's lines",
			change: mergeMain("shared.txt", "0\n1\n2\n3\n4\n5\n6\n7\n8\n"), wantErr: "diff changed"},
		{name: "new PR commit", change: func() { commit("extra.txt", "x\n") }, wantErr: "diff changed"},
		{name: "whitespace-only change", change: func() { commit("shared.txt", "1\n2\n3\n4\n5\n6\nPR \n8\n") }, wantErr: "diff changed"},
		{name: "changed binary", change: func() { commit("blob.bin", "\x00\x01") }, wantErr: "diff changed"},
	}
	prevBase := reviewerBaseSHA
	reviewerBaseSHA = func(_ int, sha string) string { return git("merge-base", "main", sha) }
	t.Cleanup(func() { reviewerBaseSHA = prevBase })

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			newHead := head(fmt.Sprintf("case%d", i), tc.change)
			dir := stubReviewerPasses(t, "security")
			runStubPass(t, dir, "security", reviewed, `{"perspective":"security","verdict":"clean","findings":[]}`)

			if _, err := consolidateVerifiedPasses(7, newHead, "", &reviewer.Edits{}); err == nil {
				t.Fatal("a new head was accepted without --carried-from")
			}
			fs, err := consolidateVerifiedPasses(7, newHead, reviewed, &reviewer.Edits{})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			posted, err := gateReviewPost(7, "", fs)
			if err != nil {
				t.Fatal(err)
			}
			if body := posted.SummaryBody(newHead); !strings.Contains(body, "Carried from:** `"+reviewed+"`") {
				t.Errorf("body does not name the reviewed head:\n%s", body)
			}
		})
	}
}
