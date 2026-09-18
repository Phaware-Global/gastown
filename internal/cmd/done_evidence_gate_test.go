package cmd

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestLacksCompletionEvidence covers hq-8ynas required behavior #1: a
// zero-commit "no code changes" close must not happen on silence alone.
func TestLacksCompletionEvidence(t *testing.T) {
	tests := []struct {
		name          string
		isPolecat     bool
		hasEvidence   bool
		cleanupStatus string // EXPLICIT --cleanup-status only
		skipVerify    bool
		isNoMergeTask bool
		want          bool
	}{
		{
			name:      "hga-2put shape: fresh dispatch, zero commits, no notes — refuse",
			isPolecat: true, hasEvidence: false,
			want: true,
		},
		{
			name:      "notes recorded — evidence satisfies the gate",
			isPolecat: true, hasEvidence: true,
			want: false,
		},
		{
			name:          "explicit --cleanup-status=clean is an opt-in signal, not silence",
			isPolecat:     true,
			hasEvidence:   false,
			cleanupStatus: "clean",
			want:          false,
		},
		{
			name:      "--skip-verify is an explicit operator override",
			isPolecat: true, hasEvidence: false, skipVerify: true,
			want: false,
		},
		{
			name:      "no_merge/review_only task legitimately has no commits or notes",
			isPolecat: true, hasEvidence: false, isNoMergeTask: true,
			want: false,
		},
		{
			name:      "non-polecat (crew/mayor) exempt",
			isPolecat: false, hasEvidence: false,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lacksCompletionEvidence(tt.isPolecat, tt.hasEvidence, tt.cleanupStatus, tt.skipVerify, tt.isNoMergeTask)
			if got != tt.want {
				t.Errorf("lacksCompletionEvidence() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCompletionCommitShaLine covers hq-8ynas required behavior #3: never
// cite a commit_sha the worker did not produce — specifically, never present
// the base branch's own tip as if it were evidence of a fix. PR #234 finding
// D scoped that judgment to the verified-push call site only (VerifyPushedCommit
// already succeeded); the --skip-verify path never claims a SHA is or isn't
// evidence, since nothing was actually checked there.
func TestCompletionCommitShaLine(t *testing.T) {
	const baseSHA = "fa4eda4ec46bc606a66382914dcf2d8a9fc71827"

	tests := []struct {
		name       string
		commitSHA  string
		baseSHA    string
		verified   bool
		wantHasSha bool // whether the literal "commit_sha:" label should appear
	}{
		{
			name:       "verified path, HEAD is exactly the base tip — hga-2put's actual incident shape",
			commitSHA:  baseSHA,
			baseSHA:    baseSHA,
			verified:   true,
			wantHasSha: false,
		},
		{
			name:       "verified path, HEAD predates the base tip but is a real, distinct commit — legit evidence",
			commitSHA:  "b2fc53e5",
			baseSHA:    baseSHA,
			verified:   true,
			wantHasSha: true,
		},
		{
			name:       "verified path, no HEAD sha resolved — nothing to cite",
			commitSHA:  "",
			baseSHA:    baseSHA,
			verified:   true,
			wantHasSha: false,
		},
		{
			name:       "--skip-verify path: SHA is reported as-is, no evidence judgment either way (finding D)",
			commitSHA:  baseSHA,
			baseSHA:    baseSHA,
			verified:   false,
			wantHasSha: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := completionCommitShaLine(tt.commitSHA, tt.baseSHA, "develop", tt.verified)
			hasSha := tt.commitSHA != "" && strings.Contains(got, "commit_sha: "+tt.commitSHA)
			if hasSha != tt.wantHasSha {
				t.Errorf("completionCommitShaLine(%q, %q, verified=%v) = %q, wantHasSha=%v", tt.commitSHA, tt.baseSHA, tt.verified, got, tt.wantHasSha)
			}
			if tt.verified && tt.commitSHA != "" && tt.commitSHA == tt.baseSHA && strings.Contains(got, tt.commitSHA) {
				t.Errorf("completionCommitShaLine(%q, %q, verified=true) = %q must not cite the base-tip sha as evidence", tt.commitSHA, tt.baseSHA, got)
			}
		})
	}
}

// historyFixtureEntry is one bd-history commit snapshot, newest-first, for
// building canned `bd history --json` output in TestHasEvidenceFromThisDispatch.
type historyFixtureEntry struct {
	assignee string
	notes    string
	design   string
}

// writeHistoryStub installs a fake `bd` on PATH that answers `bd history <id>
// --json [--limit N]` with entries (newest-first) and fails everything else,
// including the --allow-stale capability probe (BdSupportsAllowStaleWithEnv),
// so MaybePrependAllowStaleWithEnv resolves to "unsupported" and passes
// history's args through unmodified.
func writeHistoryStub(t *testing.T, entries []historyFixtureEntry) {
	t.Helper()

	var lines []string
	for i, e := range entries {
		ts := fmt.Sprintf("2026-09-18T19:%02d:00.000Z", 40-i) // strictly descending, newest first
		lines = append(lines, fmt.Sprintf(
			`{"CommitHash":"c%d","Committer":"test","CommitDate":%q,"Issue":{"id":"gt-test","assignee":%q,"notes":%q,"design":%q}}`,
			i, ts, e.assignee, e.notes, e.design))
	}
	body := "[" + strings.Join(lines, ",") + "]"

	binDir := t.TempDir()
	script := "#!/usr/bin/env sh\n" +
		"if [ \"$1\" = \"history\" ]; then\n" +
		"  cat <<'HISTEOF'\n" + body + "\nHISTEOF\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	writeBDStub(t, binDir, script, "@echo off\r\nexit 1\r\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	beads.ResetBdAllowStaleCacheForTest()
}

// TestHasEvidenceFromThisDispatch covers PR #234 finding A: hasCompletionEvidence
// must not pass on notes/design that merely happen to be non-empty — they have
// to have been written during the CURRENT assignment, not carried over from a
// prior dispatch (the hga-2put shape: val's own notes from filing the bug would
// otherwise pass every re-dispatch of that bead).
func TestHasEvidenceFromThisDispatch(t *testing.T) {
	const agent = "gastown/polecats/dag"

	t.Run("hga-2put shape: notes predate this assignment and are untouched — refuse", func(t *testing.T) {
		writeHistoryStub(t, []historyFixtureEntry{
			{assignee: agent, notes: "val's original findings"}, // now
			{assignee: agent, notes: "val's original findings"}, // hook moment (oldest in run)
			{assignee: "", notes: "val's original findings"},    // before dispatch — run boundary
		})
		bd := beads.New(t.TempDir())
		current := &beads.Issue{Notes: "val's original findings"}
		if got := hasEvidenceFromThisDispatch(bd, "gt-test", agent, current); got {
			t.Error("hasEvidenceFromThisDispatch() = true, want false (notes unchanged since dispatch)")
		}
	})

	t.Run("notes written during this assignment — evidence satisfies the gate", func(t *testing.T) {
		writeHistoryStub(t, []historyFixtureEntry{
			{assignee: agent, notes: "checked X, no code change needed"}, // now
			{assignee: agent, notes: ""},                                 // hook moment (oldest in run)
			{assignee: "", notes: ""},
		})
		bd := beads.New(t.TempDir())
		current := &beads.Issue{Notes: "checked X, no code change needed"}
		if got := hasEvidenceFromThisDispatch(bd, "gt-test", agent, current); !got {
			t.Error("hasEvidenceFromThisDispatch() = false, want true (notes added during this assignment)")
		}
	})

	t.Run("design changed during this assignment also counts", func(t *testing.T) {
		writeHistoryStub(t, []historyFixtureEntry{
			{assignee: agent, design: "root cause: X; verified via Y"},
			{assignee: agent, design: ""},
			{assignee: "", design: ""},
		})
		bd := beads.New(t.TempDir())
		current := &beads.Issue{Design: "root cause: X; verified via Y"}
		if got := hasEvidenceFromThisDispatch(bd, "gt-test", agent, current); !got {
			t.Error("hasEvidenceFromThisDispatch() = false, want true (design added during this assignment)")
		}
	})

	t.Run("agent never appears in history — fail closed", func(t *testing.T) {
		writeHistoryStub(t, []historyFixtureEntry{
			{assignee: "someone/else", notes: "notes"},
		})
		bd := beads.New(t.TempDir())
		current := &beads.Issue{Notes: "notes"}
		if got := hasEvidenceFromThisDispatch(bd, "gt-test", agent, current); got {
			t.Error("hasEvidenceFromThisDispatch() = true, want false (agent absent from history)")
		}
	})

	t.Run("bd history fails — fail closed, not open", func(t *testing.T) {
		binDir := t.TempDir()
		writeBDStub(t, binDir, "#!/usr/bin/env sh\nexit 1\n", "@echo off\r\nexit 1\r\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		beads.ResetBdAllowStaleCacheForTest()

		bd := beads.New(t.TempDir())
		current := &beads.Issue{Notes: "anything"}
		if got := hasEvidenceFromThisDispatch(bd, "gt-test", agent, current); got {
			t.Error("hasEvidenceFromThisDispatch() = true, want false when bd history errors")
		}
	})

	t.Run("no currentAgent — fail closed", func(t *testing.T) {
		bd := beads.New(t.TempDir())
		current := &beads.Issue{Notes: "anything"}
		if got := hasEvidenceFromThisDispatch(bd, "gt-test", "", current); got {
			t.Error("hasEvidenceFromThisDispatch() = true, want false with no currentAgent")
		}
	})
}
