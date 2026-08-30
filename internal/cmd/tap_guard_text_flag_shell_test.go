package cmd

import (
	"os"
	"testing"
)

// setStdinForTest replaces os.Stdin with a pipe pre-loaded with data,
// so RunE functions that do io.ReadAll(os.Stdin) can be exercised
// end-to-end in tests. Returns a restore func.
func setStdinForTest(t *testing.T, data string) func() {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := w.WriteString(data); err != nil {
		t.Fatalf("write to pipe: %v", err)
	}
	w.Close()

	orig := os.Stdin
	os.Stdin = r
	return func() {
		os.Stdin = orig
		r.Close()
	}
}

// TestFindCommandSubstitutionInTextFlag pins the detection logic behind
// gt-h38j.1: agent-authored text destined for a text-bearing bd/gt flag
// must not carry a backtick or $(...) construct, because Claude Code's
// Bash tool wraps every command in `eval "..."`, which re-parses and
// executes any such construct before bd/gt ever sees the argument (see
// gt-h38j for the incident: two `bd update --append-notes` calls
// carrying a backticked `find /` phrase each spawned a ~14h runaway
// scan and silently lost the note).
func TestFindCommandSubstitutionInTextFlag(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		wantBlock  bool
		wantFlag   string
		wantBinary string
	}{
		// --- Positives: must block ---
		{
			name:       "bd append-notes with backtick",
			command:    "bd update gt-ou48 --append-notes \"runaway `find /` scans saturating disk I/O\"",
			wantBlock:  true,
			wantFlag:   "--append-notes",
			wantBinary: "bd",
		},
		{
			name:       "bd append-notes with dollar-paren",
			command:    `bd update gt-ou48 --append-notes "runaway $(find /) scans"`,
			wantBlock:  true,
			wantFlag:   "--append-notes",
			wantBinary: "bd",
		},
		{
			name:       "gt mail send -m with backtick",
			command:    "gt mail send mayor/ -s \"Update\" -m \"see `whoami` for details\"",
			wantBlock:  true,
			wantFlag:   "-m",
			wantBinary: "gt",
		},
		{
			name:       "gt mail send --message long form",
			command:    "gt mail send mayor/ --message \"see $(whoami) for details\"",
			wantBlock:  true,
			wantFlag:   "--message",
			wantBinary: "gt",
		},
		{
			name:       "short flag -d caught",
			command:    "bd update gt-abc -d \"desc with `id` embedded\"",
			wantBlock:  true,
			wantFlag:   "-d",
			wantBinary: "bd",
		},
		{
			name:       "short flag -r caught",
			command:    "gt escalate -s HIGH -r \"reason with $(id) embedded\"",
			wantBlock:  true,
			wantFlag:   "-r",
			wantBinary: "gt",
		},
		{
			name:       "short flag -s caught",
			command:    "gt mail send mayor/ -s \"subject with `id`\" -m body",
			wantBlock:  true,
			wantFlag:   "-s",
			wantBinary: "gt",
		},
		{
			name:       "single-quoted argument still caught",
			command:    "bd update gt-abc --append-notes 'note with `find /` inside'",
			wantBlock:  true,
			wantFlag:   "--append-notes",
			wantBinary: "bd",
		},
		{
			name:       "flag=value long form caught",
			command:    "bd update gt-abc --notes=\"has $(whoami) inline\"",
			wantBlock:  true,
			wantFlag:   "--notes",
			wantBinary: "bd",
		},
		{
			name:       "acceptance flag caught",
			command:    "bd update gt-abc --acceptance \"done when `date` passes\"",
			wantBlock:  true,
			wantFlag:   "--acceptance",
			wantBinary: "bd",
		},
		{
			name:       "design flag caught",
			command:    "bd update gt-abc --design \"plan: $(cat notes)\"",
			wantBlock:  true,
			wantFlag:   "--design",
			wantBinary: "bd",
		},
		{
			name:       "title flag caught",
			command:    "bd create --title \"Bug: `id` fails\"",
			wantBlock:  true,
			wantFlag:   "--title",
			wantBinary: "bd",
		},
		{
			name:       "gt args flag caught",
			command:    "gt sling gt-abc gastown --args \"focus on $(id)\"",
			wantBlock:  true,
			wantFlag:   "--args",
			wantBinary: "gt",
		},
		{
			name:       "gt reason flag caught",
			command:    "gt mq reject greenplace mr-1 --reason \"bad: `id`\"",
			wantBlock:  true,
			wantFlag:   "--reason",
			wantBinary: "gt",
		},
		{
			name:       "gt body flag caught",
			command:    "gt handoff --body \"progress: $(ls)\"",
			wantBlock:  true,
			wantFlag:   "--body",
			wantBinary: "gt",
		},

		// --- Negatives: must NOT block ---
		{
			name:      "append-notes-file is a different flag, not blocked",
			command:   "bd update gt-abc --append-notes-file /tmp/notes.txt",
			wantBlock: false,
		},
		{
			name:      "stdin heredoc form not blocked",
			command:   "gt mail send mayor/ -s Update --stdin",
			wantBlock: false,
		},
		{
			name:      "CLAUDE.md dolt diagnostics line: $(date +%s) outside any text-bearing flag",
			command:   "gt dolt dump 2>&1 | tee /tmp/dolt-hang-$(date +%s).log",
			wantBlock: false,
		},
		{
			name:      "plain bd command with no text-bearing flags",
			command:   "bd show gt-abc",
			wantBlock: false,
		},
		{
			name:      "backtick outside a text-bearing flag argument",
			command:   "bd update gt-abc --priority 1 `echo hi`",
			wantBlock: false,
		},
		{
			name:      "no bd/gt invocation at all",
			command:   `curl -d "$(cat foo)" https://example.com`,
			wantBlock: false,
		},
		{
			// Round-2 finding 2: whole-line scan blocked another
			// program's flags whenever bd/gt appeared anywhere on
			// the same command line, even in a different shell
			// segment. --body here belongs to gh, not gt.
			name:      "compound command: gh --body in a different segment than gt is not blocked",
			command:   `gt done && gh pr create --title "x" --body "$(cat /tmp/pr.md)"`,
			wantBlock: false,
		},
		{
			name:      "compound command: git -m in a different segment than bd is not blocked",
			command:   "bd close gt-x && git commit -m \"fix: $(date +%F)\"",
			wantBlock: false,
		},
		{
			// Round-2 finding 3: an escaped quote inside the
			// argument used to desync the tokenizer and bypass
			// the guard by closing the string early.
			name:       "escaped quote inside argument still caught",
			command:    "bd update gt-x --append-notes \"escaped \\\" quote and `id`\"",
			wantBlock:  true,
			wantFlag:   "--append-notes",
			wantBinary: "bd",
		},
		{
			// Round-2 finding 4: attached short-flag value
			// (git-style -mtext) was never inspected.
			name:       "attached short-flag value -m\"...\" caught",
			command:    `bd update gt-x -m"note with $(id)"`,
			wantBlock:  true,
			wantFlag:   "-m",
			wantBinary: "bd",
		},
		{
			// Round-2 finding 6: positional free text (no flag at
			// all) is the guard's core case, not an edge.
			name:       "gt nudge positional message caught",
			command:    "gt nudge mayor/ \"check `id` output\"",
			wantBlock:  true,
			wantFlag:   positionalArgLabel,
			wantBinary: "gt",
		},
		{
			name:       "bd create positional title caught",
			command:    "bd create \"Fix `parseFoo` handling\"",
			wantBlock:  true,
			wantFlag:   positionalArgLabel,
			wantBinary: "bd",
		},
		{
			name:      "gt nudge with clean positional message is not blocked",
			command:   `gt nudge mayor/ "all clear, no code spans here"`,
			wantBlock: false,
		},
		{
			name:      "bd show positional id is not a text-carrying subcommand, not blocked",
			command:   "bd show gt-abc `echo hi`",
			wantBlock: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tokens := tokenizeShellWords(tc.command)
			invokes := invokesBdOrGt(tokens)
			match, found := findCommandSubstitutionInTextFlag(tc.command)
			blocked := invokes && found

			if blocked != tc.wantBlock {
				t.Fatalf("command %q: blocked = %v; want %v (match=%+v)", tc.command, blocked, tc.wantBlock, match)
			}
			if !tc.wantBlock {
				return
			}
			if match.flag != tc.wantFlag {
				t.Errorf("command %q: flag = %q; want %q", tc.command, match.flag, tc.wantFlag)
			}
			if match.binary != tc.wantBinary {
				t.Errorf("command %q: binary = %q; want %q", tc.command, match.binary, tc.wantBinary)
			}
		})
	}
}

// TestRunTapGuardTextFlagShell exercises the full hook entry point
// (stdin JSON in, exit-code semantics out) rather than just the
// detection helper, so a regression in extractCommand wiring or the
// bd/gt gate would also be caught here.
func TestRunTapGuardTextFlagShell(t *testing.T) {
	tests := []struct {
		name        string
		stdinJSON   string
		wantBlocked bool
	}{
		{
			name:        "blocked: bd append-notes with backtick",
			stdinJSON:   `{"tool_input": {"command": "bd update gt-ou48 --append-notes \"runaway ` + "`find /`" + ` scan\""}}`,
			wantBlocked: true,
		},
		{
			name:        "blocked: gt mail send -m with dollar-paren",
			stdinJSON:   `{"tool_input": {"command": "gt mail send mayor/ -s x -m \"see $(whoami)\""}}`,
			wantBlocked: true,
		},
		{
			name:        "allowed: gt mail send --stdin form",
			stdinJSON:   `{"tool_input": {"command": "gt mail send mayor/ -s x --stdin"}}`,
			wantBlocked: false,
		},
		{
			name:        "allowed: CLAUDE.md dolt diagnostics regression case",
			stdinJSON:   `{"tool_input": {"command": "gt dolt dump 2>&1 | tee /tmp/dolt-hang-$(date +%s).log"}}`,
			wantBlocked: false,
		},
		{
			name:        "allowed: non bd/gt command",
			stdinJSON:   `{"tool_input": {"command": "echo hello"}}`,
			wantBlocked: false,
		},
		{
			name:        "allowed: empty payload fails open",
			stdinJSON:   `{}`,
			wantBlocked: false,
		},
		{
			name:        "allowed: gh --body in a different segment than gt",
			stdinJSON:   `{"tool_input": {"command": "gt done && gh pr create --title x --body \"$(cat /tmp/pr.md)\""}}`,
			wantBlocked: false,
		},
		{
			name:        "blocked: gt nudge positional message with backtick",
			stdinJSON:   `{"tool_input": {"command": "gt nudge mayor/ \"check ` + "`id`" + ` output\""}}`,
			wantBlocked: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			restore := setStdinForTest(t, tc.stdinJSON)
			defer restore()

			err := runTapGuardTextFlagShell(tapGuardTextFlagShellCmd, nil)

			if tc.wantBlocked {
				silent, ok := err.(*SilentExitError)
				if !ok || silent.Code != 2 {
					t.Fatalf("expected blocked (SilentExit 2), got err=%v", err)
				}
			} else if err != nil {
				t.Fatalf("expected allowed (nil error), got err=%v", err)
			}
		})
	}
}
