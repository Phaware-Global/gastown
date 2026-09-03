package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	gitpkg "github.com/steveyegge/gastown/internal/git"
)

func TestResolveMQSubmitCommitSHAUsesSubmittedBranch(t *testing.T) {
	repo := t.TempDir()
	runGitForMQSubmitTest(t, repo, "init")
	runGitForMQSubmitTest(t, repo, "config", "user.email", "test@example.com")
	runGitForMQSubmitTest(t, repo, "config", "user.name", "Test User")

	writeMQSubmitTestFile(t, repo, "file.txt", "main\n")
	runGitForMQSubmitTest(t, repo, "add", "file.txt")
	runGitForMQSubmitTest(t, repo, "commit", "-m", "main")
	runGitForMQSubmitTest(t, repo, "branch", "-M", "main")
	mainSHA := runGitForMQSubmitTest(t, repo, "rev-parse", "HEAD")

	runGitForMQSubmitTest(t, repo, "checkout", "-b", "feature/pr-target")
	writeMQSubmitTestFile(t, repo, "file.txt", "feature\n")
	runGitForMQSubmitTest(t, repo, "commit", "-am", "feature")
	featureSHA := runGitForMQSubmitTest(t, repo, "rev-parse", "HEAD")
	runGitForMQSubmitTest(t, repo, "tag", "feature/pr-target", mainSHA)

	runGitForMQSubmitTest(t, repo, "checkout", "main")
	g := gitpkg.NewGit(repo)
	got, err := resolveMQSubmitCommitSHA(g, "feature/pr-target")
	if err != nil {
		t.Fatalf("resolveMQSubmitCommitSHA: %v", err)
	}
	if got != featureSHA {
		t.Fatalf("resolveMQSubmitCommitSHA() = %s, want submitted branch tip %s", got, featureSHA)
	}
	if got == mainSHA {
		t.Fatalf("resolveMQSubmitCommitSHA() used HEAD %s instead of submitted branch tip", mainSHA)
	}
}

func TestVerifyMQSubmitPushedBranchRequiresRemoteBranch(t *testing.T) {
	repo := t.TempDir()
	remote := t.TempDir()
	runGitForMQSubmitTest(t, remote, "init", "--bare")

	runGitForMQSubmitTest(t, repo, "init")
	runGitForMQSubmitTest(t, repo, "config", "user.email", "test@example.com")
	runGitForMQSubmitTest(t, repo, "config", "user.name", "Test User")
	runGitForMQSubmitTest(t, repo, "remote", "add", "origin", remote)

	writeMQSubmitTestFile(t, repo, "file.txt", "main\n")
	runGitForMQSubmitTest(t, repo, "add", "file.txt")
	runGitForMQSubmitTest(t, repo, "commit", "-m", "main")
	runGitForMQSubmitTest(t, repo, "branch", "-M", "main")
	runGitForMQSubmitTest(t, repo, "push", "-u", "origin", "main")

	runGitForMQSubmitTest(t, repo, "checkout", "-b", "feature/pr-target")
	writeMQSubmitTestFile(t, repo, "file.txt", "feature\n")
	runGitForMQSubmitTest(t, repo, "commit", "-am", "feature")
	featureSHA := runGitForMQSubmitTest(t, repo, "rev-parse", "HEAD")

	g := gitpkg.NewGit(repo)
	err := verifyMQSubmitPushedBranch(g, "feature/pr-target", featureSHA)
	if err == nil {
		t.Fatal("verifyMQSubmitPushedBranch() = nil, want missing remote branch error")
	}
	for _, want := range []string{"git push origin feature/pr-target", "gt done"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("verifyMQSubmitPushedBranch() error missing %q: %v", want, err)
		}
	}

	runGitForMQSubmitTest(t, repo, "push", "origin", "feature/pr-target")
	if err := verifyMQSubmitPushedBranch(g, "feature/pr-target", featureSHA); err != nil {
		t.Fatalf("verifyMQSubmitPushedBranch() after push: %v", err)
	}
}

func runGitForMQSubmitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeMQSubmitTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidateMoleculePrereqs(t *testing.T) {
	tests := []struct {
		name      string
		children  []*beads.Issue
		wantErr   bool
		wantInErr []string // Substrings expected in error message
	}{
		{
			name:     "nil children",
			children: nil,
			wantErr:  false,
		},
		{
			name:     "empty children",
			children: []*beads.Issue{},
			wantErr:  false,
		},
		{
			name: "all prereqs closed",
			children: []*beads.Issue{
				{ID: "gt-mol.1", Title: "Load context", Status: "closed"},
				{ID: "gt-mol.2", Title: "Set up branch", Status: "closed"},
				{ID: "gt-mol.3", Title: "Implement", Status: "closed"},
				{ID: "gt-mol.4", Title: "Self-review", Status: "closed"},
				{ID: "gt-mol.5", Title: "Build check", Status: "closed"},
				{ID: "gt-mol.6", Title: "Commit changes", Status: "closed"},
				{ID: "gt-mol.7", Title: "Rebase verify", Status: "closed"},
				{ID: "gt-mol.8", Title: "Submit MR", Status: "open"},
				{ID: "gt-mol.9", Title: "Wait for verdict", Status: "open"},
				{ID: "gt-mol.10", Title: "Self-clean", Status: "open"},
			},
			wantErr: false,
		},
		{
			name: "missing self-review step",
			children: []*beads.Issue{
				{ID: "gt-mol.1", Title: "Load context", Status: "closed"},
				{ID: "gt-mol.2", Title: "Set up branch", Status: "closed"},
				{ID: "gt-mol.3", Title: "Implement", Status: "closed"},
				{ID: "gt-mol.4", Title: "Self-review", Status: "open"},
				{ID: "gt-mol.5", Title: "Build check", Status: "closed"},
				{ID: "gt-mol.6", Title: "Commit changes", Status: "closed"},
				{ID: "gt-mol.7", Title: "Rebase verify", Status: "closed"},
				{ID: "gt-mol.8", Title: "Submit MR", Status: "open"},
			},
			wantErr:   true,
			wantInErr: []string{"gt-mol.4", "Self-review", "--skip-deps"},
		},
		{
			name: "multiple incomplete steps",
			children: []*beads.Issue{
				{ID: "gt-mol.1", Title: "Load context", Status: "closed"},
				{ID: "gt-mol.2", Title: "Set up branch", Status: "open"},
				{ID: "gt-mol.3", Title: "Implement", Status: "in_progress"},
				{ID: "gt-mol.4", Title: "Self-review", Status: "open"},
				{ID: "gt-mol.5", Title: "Submit MR", Status: "open"},
			},
			wantErr:   true,
			wantInErr: []string{"gt-mol.2", "gt-mol.3", "gt-mol.4"},
		},
		{
			name: "no submit step found — checks all steps",
			children: []*beads.Issue{
				{ID: "gt-mol.1", Title: "Load context", Status: "closed"},
				{ID: "gt-mol.2", Title: "Implement", Status: "open"},
				{ID: "gt-mol.3", Title: "Build check", Status: "open"},
			},
			wantErr:   true,
			wantInErr: []string{"gt-mol.2", "gt-mol.3"},
		},
		{
			name: "post-submit steps open is OK",
			children: []*beads.Issue{
				{ID: "gt-mol.1", Title: "Load context", Status: "closed"},
				{ID: "gt-mol.2", Title: "Submit MR", Status: "open"},
				{ID: "gt-mol.3", Title: "Wait for verdict", Status: "open"},
			},
			wantErr: false,
		},
		{
			name: "case insensitive submit detection",
			children: []*beads.Issue{
				{ID: "gt-mol.1", Title: "Implement", Status: "closed"},
				{ID: "gt-mol.2", Title: "SUBMIT MR and enter awaiting_verdict", Status: "open"},
				{ID: "gt-mol.3", Title: "Self-clean", Status: "open"},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMoleculePrereqs(tt.children)
			if tt.wantErr && err == nil {
				t.Errorf("validateMoleculePrereqs() = nil, want error")
				return
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validateMoleculePrereqs() = %v, want nil", err)
				return
			}
			if err != nil {
				errMsg := err.Error()
				for _, want := range tt.wantInErr {
					if !strings.Contains(errMsg, want) {
						t.Errorf("error message missing %q, got: %s", want, errMsg)
					}
				}
			}
		})
	}
}

// TestMQSubmitEnsureMRBead drives ensureMRBead — the production decision
// function runMqSubmit calls — with fake create/show/validatePrefix hooks, so
// deleting or reordering the production guards fails these cases (unlike the
// previous version of this test, which re-encoded the control flow in a local
// closure the production code never executed).
func TestMQSubmitEnsureMRBead(t *testing.T) {
	const branch = "polecat/furiosa/gt-abc"
	const retryCmd = "gt mq submit"
	const mrID = "gts-mr-test"

	okIssue := &beads.Issue{ID: mrID}
	okCreate := func() (*beads.Issue, error) { return okIssue, nil }
	okShow := func(id string) (*beads.Issue, error) { return &beads.Issue{ID: id}, nil }
	okPrefix := func(string) error { return nil }

	tests := []struct {
		name        string
		existing    *beads.Issue
		create      func() (*beads.Issue, error)
		show        func(id string) (*beads.Issue, error)
		prefix      func(id string) error
		wantErr     bool
		wantCreated bool
		wantInErr   []string
	}{
		{
			name:        "create succeeds + show succeeds → confirmed, created",
			create:      okCreate,
			show:        okShow,
			prefix:      okPrefix,
			wantCreated: true,
		},
		{
			name:      "create fails → hard error wrapping cause",
			create:    func() (*beads.Issue, error) { return nil, fmt.Errorf("dolt write failed") },
			show:      okShow,
			prefix:    okPrefix,
			wantErr:   true,
			wantInErr: []string{"creating merge request bead", "dolt write failed"},
		},
		{
			name:      "empty MR ID → hard error naming gt mq submit, not gt done",
			create:    func() (*beads.Issue, error) { return &beads.Issue{ID: ""}, nil },
			show:      okShow,
			prefix:    okPrefix,
			wantErr:   true,
			wantInErr: []string{branch, "re-run gt mq submit"},
		},
		{
			name:      "show fails → hard error (GH#1945) naming gt mq submit",
			create:    okCreate,
			show:      func(string) (*beads.Issue, error) { return nil, fmt.Errorf("dolt read failed") },
			prefix:    okPrefix,
			wantErr:   true,
			wantInErr: []string{branch, mrID, "re-run gt mq submit", "dolt read failed"},
		},
		{
			name:      "show returns nil bead → hard error (GH#1945)",
			create:    okCreate,
			show:      func(string) (*beads.Issue, error) { return nil, nil },
			prefix:    okPrefix,
			wantErr:   true,
			wantInErr: []string{branch, mrID, "re-run gt mq submit"},
		},
		{
			name:      "prefix mismatch + show fails → error carries the prefix-mismatch hint",
			create:    okCreate,
			show:      func(string) (*beads.Issue, error) { return nil, fmt.Errorf("not found") },
			prefix:    func(string) error { return fmt.Errorf("bead gts-mr-test not in rig db") },
			wantErr:   true,
			wantInErr: []string{"prefix mismatch", "re-run gt mq submit"},
		},
		{
			name:        "existing MR → idempotent success without create or show",
			existing:    okIssue,
			create:      func() (*beads.Issue, error) { t.Error("create must not run on idempotent path"); return nil, nil },
			show:        func(string) (*beads.Issue, error) { t.Error("show must not run on idempotent path"); return nil, nil },
			prefix:      func(string) error { t.Error("validatePrefix must not run on idempotent path"); return nil },
			wantCreated: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr, created, err := ensureMRBead(tt.existing, branch, retryCmd, tt.create, tt.show, tt.prefix)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error: %v", err, tt.wantErr)
			}
			if created != tt.wantCreated {
				t.Errorf("created = %v, want %v", created, tt.wantCreated)
			}
			if err != nil {
				for _, want := range tt.wantInErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error missing %q; got: %s", want, err.Error())
					}
				}
				return
			}
			if mr == nil || mr.ID != mrID {
				t.Errorf("mr = %+v, want ID %q", mr, mrID)
			}
		})
	}
}

// TestMQSubmitEnsureMRBeadValidatesPrefixBeforeReadback pins the gt-gpy
// ordering: Create routes by rig alias but Show routes by ID prefix, so on a
// prefix mismatch the read-back queries the wrong database and fails with a
// misleading "unconfirmed" error. The prefix warning must fire first so the
// one message explaining the failure is emitted.
func TestMQSubmitEnsureMRBeadValidatesPrefixBeforeReadback(t *testing.T) {
	var calls []string
	_, _, err := ensureMRBead(nil, "polecat/furiosa/gt-abc", "gt mq submit",
		func() (*beads.Issue, error) { return &beads.Issue{ID: "hq-mr-1"}, nil },
		func(string) (*beads.Issue, error) { calls = append(calls, "show"); return nil, fmt.Errorf("not found") },
		func(string) error { calls = append(calls, "validatePrefix"); return fmt.Errorf("prefix mismatch") },
	)
	if err == nil {
		t.Fatal("want read-back hard error")
	}
	want := []string{"validatePrefix", "show"}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Errorf("call order = %v, want %v", calls, want)
	}
}

// TestMQSubmitRetryAfterReadbackFailureStillNudges encodes the GH#1945
// stranding scenario from PR #189 review: Create persists the bead, the
// read-back fails transiently, the command hard-fails; the re-run finds the
// bead and takes the idempotent path. That retry must succeed through
// ensureMRBead — runMqSubmit nudges the refinery unconditionally after a nil
// error, so idempotent success is the nudge precondition. Before the fix the
// nudge lived inside the create-only branch and the retried MR was never
// picked up.
func TestMQSubmitRetryAfterReadbackFailureStillNudges(t *testing.T) {
	const branch = "polecat/furiosa/gt-abc"
	persisted := &beads.Issue{ID: "gts-mr-persisted"}

	// First run: create persists, read-back fails → hard error, no nudge.
	_, _, err := ensureMRBead(nil, branch, "gt mq submit",
		func() (*beads.Issue, error) { return persisted, nil },
		func(string) (*beads.Issue, error) { return nil, fmt.Errorf("transient dolt read failure") },
		func(string) error { return nil },
	)
	if err == nil {
		t.Fatal("first run: want read-back hard error")
	}

	// Retry: the dedup lookup finds the persisted bead → idempotent success,
	// which must reach the (now unconditional) nudge in runMqSubmit.
	mr, created, err := ensureMRBead(persisted, branch, "gt mq submit",
		func() (*beads.Issue, error) { t.Error("retry must not create a second MR"); return nil, nil },
		func(string) (*beads.Issue, error) {
			t.Error("retry must not re-verify the existing MR")
			return nil, nil
		},
		func(string) error { return nil },
	)
	if err != nil {
		t.Fatalf("retry must succeed so the refinery is nudged; got %v", err)
	}
	if created {
		t.Error("retry must report the idempotent path, not a fresh create")
	}
	if mr != persisted {
		t.Errorf("retry must return the persisted MR; got %+v", mr)
	}
}
