package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
)

// fakeMRFinder is a test stub for the mrFinder interface used by applyMQCheck.
type fakeMRFinder struct {
	issue *beads.Issue
	err   error
}

func (f fakeMRFinder) FindMRForBranchAny(branch string) (*beads.Issue, error) {
	return f.issue, f.err
}

type fakeIssueShower struct {
	issue *beads.Issue
	err   error
}

func (f fakeIssueShower) Show(issueID string) (*beads.Issue, error) {
	return f.issue, f.err
}

func TestApplyMQCheck(t *testing.T) {
	tests := []struct {
		name           string
		finder         mrFinder
		beadTerminal   bool
		hasWork        bool
		initialVerdict string
		wantVerdict    string
		wantMQStatus   string
		wantNeedsRecov bool
	}{
		{
			// The regression this change fixes: assigned bead is CLOSED
			// (e.g. aa-xtee no-op audit). Must NOT return NEEDS_MQ_SUBMIT
			// because there is nothing to submit — the work is terminal.
			name:           "closed bead skips MQ submit check",
			finder:         fakeMRFinder{issue: nil, err: nil},
			beadTerminal:   true,
			hasWork:        true,
			initialVerdict: "SAFE_TO_NUKE",
			wantVerdict:    "SAFE_TO_NUKE",
			wantMQStatus:   "submitted",
			wantNeedsRecov: false,
		},
		{
			name:           "no submittable work skips MQ submit check",
			finder:         fakeMRFinder{issue: nil, err: nil},
			beadTerminal:   false,
			hasWork:        false,
			initialVerdict: "SAFE_TO_NUKE",
			wantVerdict:    "SAFE_TO_NUKE",
			wantMQStatus:   "not_required",
			wantNeedsRecov: false,
		},
		{
			name:           "open bead with no MR escalates to NEEDS_MQ_SUBMIT",
			finder:         fakeMRFinder{issue: nil, err: nil},
			beadTerminal:   false,
			hasWork:        true,
			initialVerdict: "SAFE_TO_NUKE",
			wantVerdict:    "NEEDS_MQ_SUBMIT",
			wantMQStatus:   "not_submitted",
			wantNeedsRecov: true,
		},
		{
			name:           "open bead with MR stays SAFE_TO_NUKE",
			finder:         fakeMRFinder{issue: &beads.Issue{ID: "mr-1"}, err: nil},
			beadTerminal:   false,
			hasWork:        true,
			initialVerdict: "SAFE_TO_NUKE",
			wantVerdict:    "SAFE_TO_NUKE",
			wantMQStatus:   "submitted",
			wantNeedsRecov: false,
		},
		{
			name:           "MR lookup error is conservative (unknown, no escalation)",
			finder:         fakeMRFinder{issue: nil, err: errors.New("bd exploded")},
			beadTerminal:   false,
			hasWork:        true,
			initialVerdict: "SAFE_TO_NUKE",
			wantVerdict:    "SAFE_TO_NUKE",
			wantMQStatus:   "unknown",
			wantNeedsRecov: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := RecoveryStatus{
				Verdict: tt.initialVerdict,
				Branch:  "polecat/test",
			}
			applyMQCheck(&status, tt.finder, tt.beadTerminal, tt.hasWork)

			if status.Verdict != tt.wantVerdict {
				t.Errorf("Verdict = %q, want %q", status.Verdict, tt.wantVerdict)
			}
			if status.MQStatus != tt.wantMQStatus {
				t.Errorf("MQStatus = %q, want %q", status.MQStatus, tt.wantMQStatus)
			}
			if status.NeedsRecovery != tt.wantNeedsRecov {
				t.Errorf("NeedsRecovery = %v, want %v", status.NeedsRecovery, tt.wantNeedsRecov)
			}
		})
	}
}

func TestCleanupStatusBlocker(t *testing.T) {
	tests := []struct {
		status string
		want   string
	}{
		{status: "clean", want: ""},
		{status: "has_unpushed", want: "cleanup_status=has_unpushed"},
		{status: "unknown", want: "cleanup_status=unknown"},
		{status: "", want: "cleanup_status=<missing>"},
		{status: "weird", want: "cleanup_status=weird"},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			got := cleanupStatusBlocker(polecat.CleanupStatus(tt.status))
			if got != tt.want {
				t.Errorf("cleanupStatusBlocker(%q) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestActiveMRBlocker(t *testing.T) {
	tests := []struct {
		name string
		mrID string
		bd   issueShower
		want string
	}{
		{name: "empty", want: ""},
		{name: "closed", mrID: "mr-1", bd: fakeIssueShower{issue: &beads.Issue{ID: "mr-1", Status: "closed"}}, want: ""},
		{name: "open", mrID: "mr-1", bd: fakeIssueShower{issue: &beads.Issue{ID: "mr-1", Status: "open"}}, want: "active_mr=mr-1 status=open"},
		{name: "missing", mrID: "mr-1", bd: fakeIssueShower{err: beads.ErrNotFound}, want: "active_mr=mr-1 status=missing"},
		{name: "nil reader", mrID: "mr-1", bd: nil, want: "active_mr=mr-1 status=unverified"},
		{name: "lookup error", mrID: "mr-1", bd: fakeIssueShower{err: errors.New("bd exploded")}, want: "active_mr=mr-1 status=lookup_error: bd exploded"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := activeMRBlocker(tt.bd, tt.mrID)
			if got != tt.want {
				t.Errorf("activeMRBlocker() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatSafetyCheckBlockers(t *testing.T) {
	blocked := []*SafetyCheckResult{
		{Polecat: "gastown/fury", Reasons: []string{"cleanup_status=unknown", "active_mr=hq-wisp-1 status=open"}},
		{Polecat: "gastown/rust", Reasons: []string{"has work on hook (gt-abc)"}},
	}

	got := formatSafetyCheckBlockers(blocked)
	want := "gastown/fury: cleanup_status=unknown; active_mr=hq-wisp-1 status=open | gastown/rust: has work on hook (gt-abc)"
	if got != want {
		t.Errorf("formatSafetyCheckBlockers() = %q, want %q", got, want)
	}
}

func TestDisplaySafetyCheckBlockedToIncludesPredicates(t *testing.T) {
	var buf bytes.Buffer
	displaySafetyCheckBlockedTo(&buf, []*SafetyCheckResult{{
		Polecat: "gastown/fury",
		Reasons: []string{"cleanup_status=unknown", "active_mr=hq-wisp-1 status=open"},
	}})
	out := buf.String()
	for _, want := range []string{
		"Cannot nuke",
		"gastown/fury",
		"cleanup_status=unknown",
		"active_mr=hq-wisp-1 status=open",
		"Force nuke (LOSES WORK)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("displaySafetyCheckBlockedTo() missing %q in %q", want, out)
		}
	}
}

func TestDryRunNukeSummary(t *testing.T) {
	tests := []struct {
		name    string
		total   int
		blocked int
		want    string
	}{
		{name: "safe", total: 2, want: "Would nuke 2 polecat(s)."},
		{name: "blocked", total: 2, blocked: 1, want: "Would refuse to nuke 1 of 2 polecat(s) without --force."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dryRunNukeSummary(tt.total, tt.blocked); got != tt.want {
				t.Errorf("dryRunNukeSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}
