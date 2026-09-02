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

		// --- Round-3 findings (gt-h38j.1 review of 5d0cdaa2) ---
		{
			// Finding: a backslash before the closing quote desyncs
			// the tokenizer. The agent wants --title "path C:\" (a
			// path ending in one backslash), written with two
			// backslashes so the shell's own escaping produces one.
			// The round-2 hand-rolled loop only recognized \" as an
			// escape, not \\, so it mis-closed --title's quote early
			// and swallowed --append-notes's flag name into the
			// (bogus) --title value, leaving the real hazard — the
			// backtick in --append-notes's value — in a token no flag
			// or positional scan reached.
			name:       "backslash before closing quote no longer desyncs the tokenizer",
			command:    "bd update gt-x --title \"path C:\\\\\" --append-notes \"note `id`\"",
			wantBlock:  true,
			wantFlag:   "--append-notes",
			wantBinary: "bd",
		},
		{
			name:      "value ending in a literal backslash is not itself flagged",
			command:   "bd update gt-x --title \"path C:\\\\\"",
			wantBlock: false,
		},
		{
			// Finding: bd -C <dir> create bypassed the positional scan
			// because it assumed the subcommand was always the token
			// right after "bd". bd -C <dir> is the documented way to
			// target a repo, not an edge case.
			name:       "bd -C <dir> create no longer bypasses the positional scan",
			command:    "bd -C /repo create \"Fix `id` thing\"",
			wantBlock:  true,
			wantFlag:   positionalArgLabel,
			wantBinary: "bd",
		},
		{
			name:       "bd -C <dir> comment no longer bypasses the positional scan",
			command:    "bd -C /repo comment gt-x \"see `id`\"",
			wantBlock:  true,
			wantFlag:   positionalArgLabel,
			wantBinary: "bd",
		},
		{
			// Finding: gt escalate's positional description was
			// uncovered — the exact shape CLAUDE.md tells agents to
			// run for Dolt trouble.
			name:       "gt escalate positional description now caught",
			command:    "gt escalate -s HIGH \"Dolt: `hostname` unreachable\"",
			wantBlock:  true,
			wantFlag:   positionalArgLabel,
			wantBinary: "gt",
		},
		{
			// Finding: separators were only recognized when
			// whitespace-delimited, so "cd /x;bd ..." hid "bd" inside
			// the single token "/x;bd" and was never inspected at all
			// (fail open on the real hazard).
			name:       "separator glued to adjacent word with no whitespace still splits segments",
			command:    "cd /x;bd update gt-x --notes \"`id`\"",
			wantBlock:  true,
			wantFlag:   "--notes",
			wantBinary: "bd",
		},
		{
			// Mirror image of the above: a multi-line command used to
			// be treated as one segment, so an unrelated program's
			// flag on a later line falsely blocked.
			name:      "multi-line command: git -m on a later line than gt is not blocked",
			command:   "gt sync\ngit commit -m \"$(cat msg.txt)\"",
			wantBlock: false,
		},
		{
			name:      "multi-line command: gh --body on a later line than bd is not blocked",
			command:   "bd list --status open\ngh pr comment 220 --body \"$(cat r.md)\"",
			wantBlock: false,
		},
		{
			name:      "semicolon-separated command: curl -s on the far side of a semicolon is not blocked",
			command:   "gt prime; curl -s \"https://x/$(date +%s)\"",
			wantBlock: false,
		},
		{
			// Finding: the positional scan read a redirect target as
			// free text, and a naive unquoted word-split sees
			// "$(date +%s)" as two words ("...-$(date" and
			// "+%s).log"), the first of which looks like a match.
			name:      "redirect target after positional args is not scanned",
			command:   "bd comment gt-x \"msg\" > /tmp/o-$(date +%s).log",
			wantBlock: false,
		},
		{
			// Finding: a message that merely starts with '-' was
			// mistaken for a flag and skipped (fail open) because the
			// old check treated any leading '-' as a flag.
			name:       "positional message starting with a single dash is still inspected",
			command:    "gt nudge mayor/ \"- ran `find /`\"",
			wantBlock:  true,
			wantFlag:   positionalArgLabel,
			wantBinary: "gt",
		},
		{
			// Finding: an unrecognized bd global flag used to abort
			// skipBdGlobalArgs to -1, which made the positional scan
			// report no match — the opposite of the conservative
			// intent. A future bd global flag this scan doesn't know
			// about must not smuggle the subcommand past it.
			name:       "bd unrecognized global flag no longer smuggles the subcommand past the scan",
			command:    "bd --newglobal create \"Fix `id` thing\"",
			wantBlock:  true,
			wantFlag:   positionalArgLabel,
			wantBinary: "bd",
		},

		// --- Round-4 findings (gt-h38j.1 review of 1194378b) ---
		{
			// Finding: round 3's global-flag fail-open fix only touched
			// skipBdGlobalArgs; skipGtGlobalArgs still aborted to -1 on
			// any unrecognized flag, so gt's positional scan never ran
			// at all when a novel global flag preceded the subcommand.
			name:       "gt unrecognized global flag no longer smuggles the subcommand past the scan (gt's twin of the bd fix)",
			command:    "gt --newglobal nudge mayor/ \"check `id` output\"",
			wantBlock:  true,
			wantFlag:   positionalArgLabel,
			wantBinary: "gt",
		},
		{
			// Finding: a value-taking unknown global flag reached the
			// same fail-open gap from the value side.
			name:       "bd unrecognized global flag with a value no longer smuggles the subcommand past the scan",
			command:    "bd --newglobal val create \"Fix `id` thing\"",
			wantBlock:  true,
			wantFlag:   positionalArgLabel,
			wantBinary: "bd",
		},
		{
			// Finding: the positional scan skipped a flag TOKEN via
			// isFlagToken but never skipped the token that follows it,
			// so a non-text flag's own value (not agent free text) was
			// scanned as if it were the positional argument, false-
			// blocking a legitimate command.
			name:      "positional scan skips a preceding flag's separate value, not just the flag itself",
			command:   `bd create "Fix X" -p 1 --labels "round-$(date +%s)"`,
			wantBlock: false,
		},
		{
			name:      "positional scan skips a preceding flag's quoted separate value",
			command:   `bd create "t" --assignee "$(gt whoami)"`,
			wantBlock: false,
		},
		{
			// Finding: flagAndValue inspected the attached value form
			// (git-style -mtext) only for short flags; a long flag glued
			// to its quoted value by shlex (no "=", no separate next
			// token) was never scanned.
			name:       "attached long-flag value caught (git-style --append-notes\"text\")",
			command:    "bd update gt-x --append-notes\"see `find /`\"",
			wantBlock:  true,
			wantFlag:   "--append-notes",
			wantBinary: "bd",
		},
		{
			name:       "attached long-flag value caught for --body",
			command:    "gt mail send x/ --body\"hi `id`\"",
			wantBlock:  true,
			wantFlag:   "--body",
			wantBinary: "gt",
		},
		{
			// Round-5 HIGH finding: the round-4 flag-value skip consumed
			// the token after ANY flag token, so a boolean flag sitting
			// before the positional message swallowed the message
			// unscanned — reopening the founding gt-h38j hole.
			name:       "boolean short flag before positional message no longer swallows it",
			command:    "gt nudge mayor/ -f \"check `find /` output\"",
			wantBlock:  true,
			wantFlag:   positionalArgLabel,
			wantBinary: "gt",
		},
		{
			name:       "boolean long flag before positional message no longer swallows it",
			command:    "gt escalate --json \"Dolt: `find /` hung\"",
			wantBlock:  true,
			wantFlag:   positionalArgLabel,
			wantBinary: "gt",
		},
		{
			name:       "bd boolean flag before positional title no longer swallows it",
			command:    "bd create --json \"t `id`\"",
			wantBlock:  true,
			wantFlag:   positionalArgLabel,
			wantBinary: "bd",
		},
		{
			name:       "boolean flag directly before a dollar-paren message no longer swallows it",
			command:    `gt nudge mayor/ --if-fresh "$(find /)"`,
			wantBlock:  true,
			wantFlag:   positionalArgLabel,
			wantBinary: "gt",
		},
		{
			// Round-5: isFlagToken counted the bare end-of-options marker
			// "--" as a flag, so it too swallowed the token after it.
			name:       "end-of-options marker no longer swallows the positional title",
			command:    "bd create -- \"title with `find /`\"",
			wantBlock:  true,
			wantFlag:   positionalArgLabel,
			wantBinary: "bd",
		},
		{
			name:       "everything after end-of-options is scanned as positional text",
			command:    `gt nudge mayor/ -- "msg $(id)"`,
			wantBlock:  true,
			wantFlag:   positionalArgLabel,
			wantBinary: "gt",
		},
		{
			// Round-5 MEDIUM finding, the over-block half of the same
			// skip: after a boolean flag the skip consumed the
			// value-taking FLAG token instead of its value, so the value
			// was then scanned as positional prose.
			name:      "boolean flag before a value-taking flag no longer false-blocks its value",
			command:   `bd create "t" --json --labels "round-$(date +%s)"`,
			wantBlock: false,
		},
		{
			name:      "redirect target after a boolean flag is still not scanned",
			command:   "gt nudge mayor --force > /tmp/o-$(date +%s).log",
			wantBlock: false,
		},
		{
			// Round-5 MEDIUM finding: the "=" split preempted the
			// attached long-flag prefix match, so an attached value
			// containing "=" split into the flag "--append-notesa" and
			// was never inspected — the exact gt-h38j incident spelling
			// with a KEY=value phrase in the note.
			name:       "attached long-flag value containing '=' caught",
			command:    "bd update gt-x --append-notes\"a=b `find /`\"",
			wantBlock:  true,
			wantFlag:   "--append-notes",
			wantBinary: "bd",
		},
		{
			name:       "attached long-flag value without '=' still caught",
			command:    "bd update gt-x --append-notes\"plain `find /`\"",
			wantBlock:  true,
			wantFlag:   "--append-notes",
			wantBinary: "bd",
		},
		{
			name:       "flag=value spelling with a further '=' inside the value still caught",
			command:    "bd update gt-x --append-notes=\"a=b `find /`\"",
			wantBlock:  true,
			wantFlag:   "--append-notes",
			wantBinary: "bd",
		},
		{
			name:       "attached --body value containing '=' caught",
			command:    "gt mail send x/ --body\"k=v `id`\"",
			wantBlock:  true,
			wantFlag:   "--body",
			wantBinary: "gt",
		},
		{
			// The file variants carry a path, not free text, and are the
			// guard's own recommended safe form — the attached-value
			// prefix match must not misread them as --body/--append-notes
			// with an attached substitution.
			name:      "body-file with '=' and a substitution in the path is a different flag, not blocked",
			command:   "bd update gt-x --body-file=/tmp/n-$(date +%s).txt",
			wantBlock: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			match, found := findCommandSubstitutionInTextFlag(tc.command)

			if found != tc.wantBlock {
				t.Fatalf("command %q: blocked = %v; want %v (match=%+v)", tc.command, found, tc.wantBlock, match)
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
