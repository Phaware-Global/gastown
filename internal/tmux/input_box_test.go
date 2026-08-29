package tmux

import (
	"errors"
	"testing"
)

// TestInputBoxSubmitted verifies that submission detection keys off the live
// input box (the last prompt-prefix line) rather than arbitrary pane churn.
// Regression guard for the nudge-stranding bug where a busy, animating agent
// made the old "pane content changed" check a false positive: the spinner
// mutated the pane every frame while the typed nudge sat unsubmitted.
func TestInputBoxSubmitted(t *testing.T) {
	t.Parallel()
	const nbsp = "\u00a0"
	const prefix = "\u276F " // ❯ + regular space (DefaultReadyPromptPrefix)
	const pc = "\u276F"      // ❯

	tests := []struct {
		name          string
		lines         []string
		prefix        string
		wantSubmitted bool
		wantConcl     bool
	}{
		{
			// gt-zlfq: Claude Code collapses a long pasted nudge to a
			// "[Pasted text #N]" chip. That is still unsubmitted text in the
			// box, and detection must be CONCLUSIVE about it — a merely
			// inconclusive verdict falls through to a content-diff that any
			// ticking status line satisfies, reporting a stranded nudge as
			// delivered.
			name:          "collapsed paste chip is unsubmitted text",
			lines:         []string{"  earlier transcript", pc + " [Pasted text #5][Pasted text #6]"},
			prefix:        prefix,
			wantSubmitted: false,
			wantConcl:     true,
		},
		{
			name:          "empty box after submit",
			lines:         []string{pc + " some earlier message", "  status bar", pc + " "},
			prefix:        prefix,
			wantSubmitted: true,
			wantConcl:     true,
		},
		{
			name:          "bare prompt no trailing space",
			lines:         []string{pc},
			prefix:        prefix,
			wantSubmitted: true,
			wantConcl:     true,
		},
		{
			name:          "box still holds typed nudge",
			lines:         []string{pc + " 📬 You have new mail from telegraph/jira/Kevin Jones."},
			prefix:        prefix,
			wantSubmitted: false,
			wantConcl:     true,
		},
		{
			name:          "NBSP after prompt char, empty box",
			lines:         []string{pc + nbsp},
			prefix:        prefix,
			wantSubmitted: true,
			wantConcl:     true,
		},
		{
			name:          "NBSP after prompt char, holds text",
			lines:         []string{pc + nbsp + "check telegraph messages"},
			prefix:        prefix,
			wantSubmitted: false,
			wantConcl:     true,
		},
		{
			name:          "last prompt line is the live box (earlier echoes ignored)",
			lines:         []string{pc + " old submitted text", "assistant response", pc + " "},
			prefix:        prefix,
			wantSubmitted: true,
			wantConcl:     true,
		},
		{
			name:          "no prompt line found is inconclusive",
			lines:         []string{"some output", "esc to interrupt"},
			prefix:        prefix,
			wantSubmitted: false,
			wantConcl:     false,
		},
		{
			name:          "empty prefix is inconclusive",
			lines:         []string{pc + " "},
			prefix:        "",
			wantSubmitted: false,
			wantConcl:     false,
		},
		{
			// Prompt detection must preserve the space boundary so a word-like
			// prefix doesn't false-match an unrelated line (e.g. "in " vs "info").
			name:          "word-like prefix does not match without boundary",
			lines:         []string{"info: something happened", "input received"},
			prefix:        "in ",
			wantSubmitted: false,
			wantConcl:     false,
		},
		{
			name:          "word-like prefix matches with boundary, holds text",
			lines:         []string{"in flight command"},
			prefix:        "in ",
			wantSubmitted: false,
			wantConcl:     true,
		},
		{
			// Multi-token prefix where the pane renders NBSP but the configured
			// prefix uses a regular space (or vice versa): normalization must let
			// the prefix strip so an empty box still reads as submitted.
			name:          "multi-word prefix with NBSP mismatch, empty box",
			lines:         []string{"beads" + nbsp + pc + " "},
			prefix:        "beads " + pc + " ",
			wantSubmitted: true,
			wantConcl:     true,
		},
		{
			name:          "multi-word prefix with NBSP mismatch, holds text",
			lines:         []string{"beads" + nbsp + pc + " status?"},
			prefix:        "beads " + pc + " ",
			wantSubmitted: false,
			wantConcl:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotSubmitted, gotConcl := inputBoxSubmitted(tt.lines, tt.prefix)
			if gotSubmitted != tt.wantSubmitted || gotConcl != tt.wantConcl {
				t.Errorf("inputBoxSubmitted(%q, %q) = (%v, %v), want (%v, %v)",
					tt.lines, tt.prefix, gotSubmitted, gotConcl, tt.wantSubmitted, tt.wantConcl)
			}
		})
	}
}

// TestInputBoxClearedFrom covers the gt-zlfq decision: distinguishing an agent
// that is busy because it received a nudge from one that is busy with the nudge
// still stranded, unsubmitted, in its input box.
//
// Before the fix the startup-nudge retry gated purely on agent idleness, so a
// busy-but-stranded agent was read as "nudge received" and never retried —
// skipping the retry in exactly the case that needed it. The polecat then sat
// idle without its work prompt until it was reaped and its bead reassigned.
func TestInputBoxClearedFrom(t *testing.T) {
	t.Parallel()
	const prefix = "❯ "
	const pc = "❯"

	tests := []struct {
		name       string
		lines      []string
		captureErr error
		want       bool
	}{
		{
			name:  "bare prompt is cleared",
			lines: []string{"  transcript", pc + " "},
			want:  true,
		},
		{
			name:  "stranded plain text is not cleared",
			lines: []string{"  transcript", pc + " STAND DOWN: gt-lurb is already slung"},
			want:  false,
		},
		{
			name:  "stranded collapsed paste chip is not cleared",
			lines: []string{"  transcript", pc + " [Pasted text #5][Pasted text #6]"},
			want:  false,
		},
		{
			// Bias toward "cleared": never invent a strand we cannot see, or
			// callers would re-nudge healthy agents.
			name:       "capture error reports cleared",
			lines:      nil,
			captureErr: errors.New("no server"),
			want:       true,
		},
		{
			name:  "unlocatable input box reports cleared",
			lines: []string{"some pane with no prompt at all"},
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := inputBoxClearedFrom(tt.lines, prefix, tt.captureErr); got != tt.want {
				t.Errorf("inputBoxClearedFrom() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestNudgeOptsClearSemantics pins the distinction between the two clear flags,
// which the gt-zlfq review showed is easy to conflate and dangerous to get wrong.
//
// ClearOnStrand wipes AFTER a failed submit and is only safe when the caller
// re-delivers — a bare clear destroys the message outright, which is strictly
// worse than leaving stranded text that would eventually auto-submit.
//
// ClearBeforeSend wipes BEFORE typing and is mandatory for any retry running
// against a box that may be non-empty. The delivery protocol has no pre-clear of
// its own, so without it a retry is appended to the stranded text and submitted
// as one fused line — and Enter verification then sees a bare prompt and reports
// success, hiding the corruption from every caller.
func TestNudgeOptsClearSemantics(t *testing.T) {
	t.Parallel()

	var zero NudgeOpts
	if zero.ClearOnStrand || zero.ClearBeforeSend {
		t.Fatal("zero NudgeOpts must not clear anything: a bare NudgeSession() must stay non-destructive")
	}

	// The two flags are independent: a retry against a known-stranded box needs
	// both (clear the old text, and clear again if this attempt also strands).
	retry := NudgeOpts{ClearOnStrand: true, ClearBeforeSend: true}
	if !retry.ClearBeforeSend {
		t.Error("a retry against a non-empty box must set ClearBeforeSend or it concatenates")
	}
	if !retry.ClearOnStrand {
		t.Error("a retry that re-delivers should also clear on a fresh strand")
	}
}
