package tmux

import (
	"os"
	"path/filepath"
	"strconv"
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

// TestProcessTreeMatches distinguishes the two outcomes that a bare bool cannot:
// a walk that ran and found nothing, versus a walk that could not run.
//
// Callers of the liveness probe KILL sessions, so collapsing these is what let a
// transient pgrep/ps failure read as a dead agent and reap a healthy polecat
// mid-turn (gt-azm0 / gt-kncti). pgrep and ps both exit 1 to mean "nothing
// matched", which is a real answer; anything else is ambiguity.
func TestProcessTreeMatches(t *testing.T) {
	t.Parallel()

	t.Run("own process is found by name", func(t *testing.T) {
		t.Parallel()
		self := strconv.Itoa(os.Getpid())
		comm := filepath.Base(os.Args[0])
		found, err := processTreeMatches(self, []string{comm}, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !found {
			t.Errorf("expected to find own process %s named %q", self, comm)
		}
	})

	t.Run("clean no-match is not an error", func(t *testing.T) {
		t.Parallel()
		self := strconv.Itoa(os.Getpid())
		found, err := processTreeMatches(self, []string{"definitely-not-a-real-binary-xyzzy"}, 0)
		if err != nil {
			t.Fatalf("a name that simply does not match must not be an error, got: %v", err)
		}
		if found {
			t.Error("expected no match")
		}
	})

	t.Run("nonexistent pid is a clean answer, not an error", func(t *testing.T) {
		t.Parallel()
		// ps exits 1 for an unknown pid, which means "no such process" — a real
		// answer. Treating it as ambiguity would make every reaped session
		// unverifiable.
		found, err := processTreeMatches("99999999", []string{"anything"}, 0)
		if err != nil {
			t.Fatalf("unknown pid must be a clean answer, got: %v", err)
		}
		if found {
			t.Error("expected no match for a nonexistent pid")
		}
	})

	t.Run("depth is bounded", func(t *testing.T) {
		t.Parallel()
		found, err := processTreeMatches(strconv.Itoa(os.Getpid()), []string{"x"}, 999)
		if err != nil || found {
			t.Errorf("past max depth must return (false, nil), got (%v, %v)", found, err)
		}
	})
}

// TestSplitProcessNames covers the parsing that decides which binaries count as
// "the agent" — an empty result must be surfaced rather than silently matching
// nothing, since that would make a healthy agent look dead.
func TestSplitProcessNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want int
	}{
		{"node,claude", 2},
		{" node , claude ", 2},
		{"claude", 1},
		{"", 0},
		{" ", 0},
		{",,", 0},
	}
	for _, tt := range tests {
		if got := len(splitProcessNames(tt.in)); got != tt.want {
			t.Errorf("splitProcessNames(%q) returned %d names, want %d", tt.in, got, tt.want)
		}
	}
}
