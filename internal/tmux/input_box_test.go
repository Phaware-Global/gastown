package tmux

import "testing"

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
