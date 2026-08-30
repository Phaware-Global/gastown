package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var tapGuardTextFlagShellCmd = &cobra.Command{
	Use:   "command-substitution",
	Short: "Block backtick / $(...) constructs inside text-bearing bd/gt CLI flag arguments",
	Long: `Block command-substitution constructs in bd/gt text-flag arguments.

The Claude Code Bash tool wraps every command as
  zsh -c '... && eval "cd <dir> && timeout N <command>" && pwd -P'

Inside eval plus double quotes, zsh performs command substitution on any
backtick or $(...) span in the argument text, regardless of whether the
agent quoted its own argument with single or double quotes — the
surrounding eval re-parses either way. On 2026-08-29 two
"bd update gt-ou48 --append-notes ..." calls carrying a backticked
` + "`find /`" + ` phrase each spawned a ~14 hour full-filesystem scan; neither
note was ever written (see gt-h38j).

This guard fires only when BOTH hold:
  - the command invokes bd or gt, AND
  - a backtick or $( appears inside the argument to one of the
    text-bearing flags: --append-notes, --notes, --acceptance,
    --description, -d, --body, --message, -m, --design, --reason, -r,
    --args, --title, --subject, -s

It deliberately does NOT fire on a backtick or $( anywhere else on the
command line — legitimate command substitution outside a text-bearing
flag argument is common and correct (e.g. the Dolt diagnostics in
CLAUDE.md use "$(date +%s)" inside a "tee" target path, not inside any
of the flags above).

Non-goals: this guard does not sanitize, escape, or rewrite the
argument text, and it does not touch Claude Code's eval wrapper (not
ours to change). It blocks and names the safe alternative:
  - bd: write the text to a file and pass it via the flag's file/stdin
    form (bd already has --body-file for --description and
    --design-file for --design; matching -file flags for the other
    text fields are landing separately in bd-nre and can be referenced
    here regardless of merge order).
  - gt: use --stdin, which every gt subcommand that accepts free text
    (sling, mail send, escalate, handoff, nudge, mq reject, warrant)
    already supports.

Exit codes:
  0 - Operation allowed (no bd/gt invocation, or no text-bearing flag
      argument contains a command-substitution construct)
  2 - Operation BLOCKED`,
	RunE: runTapGuardTextFlagShell,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardTextFlagShellCmd)
}

// textBearingLongFlags are the long-form CLI flags this guard treats as
// carrying agent-authored free text destined for a bd or gt argument.
var textBearingLongFlags = map[string]bool{
	"--append-notes": true,
	"--notes":        true,
	"--acceptance":   true,
	"--description":  true,
	"--body":         true,
	"--message":      true,
	"--design":       true,
	"--reason":       true,
	"--args":         true,
	"--title":        true,
	"--subject":      true,
}

// textBearingShortFlags are the short-form equivalents. Note some of
// these are ambiguous across bd/gt (e.g. "-s" is bd's --status short
// flag but gt mail send's --subject short flag) — the guard does not
// try to disambiguate by binary before deciding whether to block; an
// enum-valued flag like --status is vanishingly unlikely to ever
// legitimately need a backtick or $( in its value, so treating it as
// text-bearing here costs nothing but a false-positive on a construct
// that would never appear there anyway.
var textBearingShortFlags = map[string]bool{
	"-d": true,
	"-m": true,
	"-r": true,
	"-s": true,
}

func isTextBearingFlag(tok string) bool {
	return textBearingLongFlags[tok] || textBearingShortFlags[tok]
}

// positionalTextSubcommands maps a bd/gt subcommand (the token
// immediately following the "bd"/"gt" invocation token) to true when
// one of its positional (non-flag) arguments carries agent-authored
// free text with no flag name to key off — the same eval-executes-it
// hazard as a text-bearing flag. gt nudge's message and bd's
// create/comment text are the guard's core case (an agent describing
// what it found), not an edge: "gt nudge mayor/ \"check `id` output\""
// has no text-bearing flag at all, and the message is positional
// (internal/cmd/nudge.go: "nudge <target> [message]").
var positionalTextSubcommands = map[string]bool{
	"nudge":   true,
	"create":  true,
	"comment": true,
}

// shellSeparators are the tokens that end one bd/gt-or-other-program
// invocation and start another on the same command line. Scoping the
// scan to the segment between separators is what keeps this guard
// from blocking an unrelated program's flags just because "bd" or
// "gt" appears somewhere else on the line (e.g. "gt done && gh pr
// create --title x --body \"$(cat f)\"" must not block on gh's
// --body — that is the documented PR workflow).
var shellSeparators = map[string]bool{
	";":  true,
	"&&": true,
	"||": true,
	"|":  true,
}

// splitIntoSegments breaks tokens into the sub-slices between
// shellSeparators tokens (the separators themselves are dropped).
func splitIntoSegments(tokens []cmdToken) [][]cmdToken {
	var segments [][]cmdToken
	var cur []cmdToken
	for _, t := range tokens {
		if shellSeparators[t.text] {
			if len(cur) > 0 {
				segments = append(segments, cur)
			}
			cur = nil
			continue
		}
		cur = append(cur, t)
	}
	if len(cur) > 0 {
		segments = append(segments, cur)
	}
	return segments
}

// cmdToken is one shell word extracted from a command line, along with
// the byte offset in the original string where the word started.
type cmdToken struct {
	text  string
	start int
}

// tokenizeShellWords splits a command string into shell-like words,
// stripping single/double quote delimiters but preserving their
// contents verbatim — including any backtick or $( inside — so callers
// can inspect the raw argument text for command-substitution
// constructs regardless of how the agent quoted it.
//
// This is NOT a full shell lexer: a quote is matched to the next
// occurrence of the same quote character with no escape handling
// (same tradeoff tap_guard_push_main.go's unquote() makes elsewhere in
// this package). That is intentional — a hook-fired best-effort guard,
// not a security boundary in itself. Quoted and unquoted fragments
// within a single word are concatenated the way a real shell would
// (e.g. --notes="text" and --notes=text' more' both work), which is
// what lets the flag=value case below split correctly.
func tokenizeShellWords(command string) []cmdToken {
	var tokens []cmdToken
	n := len(command)
	i := 0
	for i < n {
		for i < n && isShellSpace(command[i]) {
			i++
		}
		if i >= n {
			break
		}
		start := i
		var b strings.Builder
		for i < n && !isShellSpace(command[i]) {
			switch command[i] {
			case '\'':
				if j := strings.IndexByte(command[i+1:], '\''); j >= 0 {
					b.WriteString(command[i+1 : i+1+j])
					i += j + 2
				} else {
					b.WriteString(command[i+1:])
					i = n
				}
			case '"':
				// Honor a backslash-escaped quote (\") as a literal "
				// inside the double-quoted run rather than treating it
				// as the terminator — an unescaped desync here closes
				// the string early, and the remainder (including any
				// backtick/$( after the real closing quote) re-tokenizes
				// outside the flag argument, silently bypassing the
				// guard on an ordinary agent-authored note containing
				// an escaped quote.
				j := i + 1
				closed := false
				for j < n {
					if command[j] == '\\' && j+1 < n && command[j+1] == '"' {
						b.WriteByte('"')
						j += 2
						continue
					}
					if command[j] == '"' {
						closed = true
						j++
						break
					}
					b.WriteByte(command[j])
					j++
				}
				if closed {
					i = j
				} else {
					i = n
				}
			default:
				b.WriteByte(command[i])
				i++
			}
		}
		tokens = append(tokens, cmdToken{text: b.String(), start: start})
	}
	return tokens
}

func isShellSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// invokesBdOrGt reports whether any token is exactly "bd" or "gt". A
// token match (not a substring/regex match) so words like "gtk" or
// "abd" don't false-positive.
func invokesBdOrGt(tokens []cmdToken) bool {
	for _, t := range tokens {
		if t.text == "bd" || t.text == "gt" {
			return true
		}
	}
	return false
}

// nearestInvocationBinary returns "bd" or "gt" — whichever invocation
// token most closely precedes beforePos, falling back to the first
// bd/gt token anywhere in the command if none precedes it. Callers
// only reach this after invokesBdOrGt has confirmed at least one
// exists, so the fallback always finds something.
func nearestInvocationBinary(tokens []cmdToken, beforePos int) string {
	binary := ""
	for _, t := range tokens {
		if t.text != "bd" && t.text != "gt" {
			continue
		}
		if t.start <= beforePos {
			binary = t.text
			continue
		}
		if binary == "" {
			binary = t.text
		}
		break
	}
	return binary
}

// commandSubstitutionMatch describes a text-bearing flag argument that
// contains a shell command-substitution construct.
type commandSubstitutionMatch struct {
	flag    string // the flag token as written, e.g. "--append-notes" or "-m"
	binary  string // "bd" or "gt" — the nearest preceding invocation
	snippet string // the offending argument text
}

// flagAndValue splits a token into (flag, value, hasValue, valueStart).
// Handles the "--flag=value" form; for tokens with no "=" (or a short
// flag), hasValue is false and the caller should look at the NEXT
// token as the argument instead.
func flagAndValue(tok cmdToken) (flag, value string, hasValue bool, valueStart int) {
	if idx := strings.IndexByte(tok.text, '='); idx >= 0 && strings.HasPrefix(tok.text, "--") {
		return tok.text[:idx], tok.text[idx+1:], true, tok.start + idx + 1
	}
	// Attached short-flag value, the git-style spelling an agent
	// commonly writes: -m"note with $(id)" tokenizes as one word
	// ("-m" concatenated with the quoted run) since tokenizeShellWords
	// merges quoted and unquoted fragments of a single shell word.
	// Without this, only the separate "-m value" form was inspected.
	if len(tok.text) > 2 && tok.text[0] == '-' && tok.text[1] != '-' {
		short := tok.text[:2]
		if textBearingShortFlags[short] {
			return short, tok.text[2:], true, tok.start + 2
		}
	}
	return tok.text, "", false, 0
}

func containsCommandSubstitution(s string) bool {
	return strings.ContainsRune(s, '`') || strings.Contains(s, "$(")
}

// positionalArgLabel is the commandSubstitutionMatch.flag value used
// when the match came from a positional argument (positionalTextSubcommands)
// rather than a named flag.
const positionalArgLabel = "(positional)"

// findCommandSubstitutionInTextFlag scans command for a bd/gt
// invocation whose text-bearing flag argument, or positional
// free-text argument, contains a backtick or $(. The scan is scoped
// to each shell-separator-delimited segment of the line so that a
// different program appearing elsewhere on the same line (e.g. "gt
// done && gh pr create --body \"$(cat f)\"") is never mistaken for
// the bd/gt invocation's own arguments. It does not itself check
// invokesBdOrGt on the whole line — callers combine both.
func findCommandSubstitutionInTextFlag(command string) (commandSubstitutionMatch, bool) {
	tokens := tokenizeShellWords(command)
	for _, segment := range splitIntoSegments(tokens) {
		if !invokesBdOrGt(segment) {
			continue
		}
		if match, ok := findCommandSubstitutionInSegment(segment); ok {
			return match, true
		}
	}
	return commandSubstitutionMatch{}, false
}

// findCommandSubstitutionInSegment scans one segment already known to
// invoke bd or gt, checking text-bearing flag arguments first and
// then, for subcommands that carry a positional free-text argument
// (positionalTextSubcommands), every non-flag token after the
// subcommand.
func findCommandSubstitutionInSegment(tokens []cmdToken) (commandSubstitutionMatch, bool) {
	if match, ok := findCommandSubstitutionInFlags(tokens); ok {
		return match, true
	}
	return findCommandSubstitutionInPositional(tokens)
}

func findCommandSubstitutionInFlags(tokens []cmdToken) (commandSubstitutionMatch, bool) {
	for i, tok := range tokens {
		flag, value, hasValue, valueStart := flagAndValue(tok)
		if !isTextBearingFlag(flag) {
			continue
		}

		var argText string
		var argStart int
		if hasValue {
			argText, argStart = value, valueStart
		} else if i+1 < len(tokens) {
			argText, argStart = tokens[i+1].text, tokens[i+1].start
		} else {
			continue
		}

		if containsCommandSubstitution(argText) {
			return commandSubstitutionMatch{
				flag:    flag,
				binary:  nearestInvocationBinary(tokens, argStart),
				snippet: argText,
			}, true
		}
	}
	return commandSubstitutionMatch{}, false
}

// findCommandSubstitutionInPositional covers bd/gt subcommands whose
// hazardous argument has no flag name at all — gt nudge's message and
// bd create/comment's title/body are the guard's core case (an agent
// describing what it found), not an edge: "gt nudge mayor/ \"check
// `id` output\"" has no text-bearing flag, and the message is
// positional.
func findCommandSubstitutionInPositional(tokens []cmdToken) (commandSubstitutionMatch, bool) {
	binIdx := -1
	for i, t := range tokens {
		if t.text == "bd" || t.text == "gt" {
			binIdx = i
			break
		}
	}
	if binIdx < 0 || binIdx+1 >= len(tokens) {
		return commandSubstitutionMatch{}, false
	}
	subcommand := tokens[binIdx+1].text
	if !positionalTextSubcommands[subcommand] {
		return commandSubstitutionMatch{}, false
	}
	binary := tokens[binIdx].text
	for i := binIdx + 2; i < len(tokens); i++ {
		tok := tokens[i]
		if strings.HasPrefix(tok.text, "-") {
			continue // flags and their values are not the free-text argument
		}
		if containsCommandSubstitution(tok.text) {
			return commandSubstitutionMatch{
				flag:    positionalArgLabel,
				binary:  binary,
				snippet: tok.text,
			}, true
		}
	}
	return commandSubstitutionMatch{}, false
}

func runTapGuardTextFlagShell(cmd *cobra.Command, args []string) error {
	// Stdin is a terminal only when a human types this command
	// directly from a shell — no hook payload to evaluate, allow.
	// Under Claude Code hooks stdin is always a pipe carrying the
	// hook JSON; skipping the read for a terminal also avoids the
	// hang risk of io.ReadAll blocking forever on a manual invocation
	// (the same fix tap_guard.go and tap_guard_interactive_input.go
	// apply for the identical reason).
	if isStdinTerminal() {
		return nil
	}

	// Deliberate choice, not an accident of the error branch: a pipe
	// read failure or an unparseable/empty payload both fail OPEN
	// here, unlike pr-workflow's fail-closed stance. pr-workflow
	// blocks broad command CATEGORIES (any "gh pr create", any
	// direct merge); this guard only ever blocks a command it has
	// positively matched to the narrow gt-h38j shape (a command-
	// substitution construct inside a specific bd/gt text argument).
	// Under the catch-all "Bash" matcher this guard now inspects
	// every Bash call town-wide, so failing closed on a transient
	// stdin hiccup would turn a rare missed block into a town-wide
	// Bash outage — a worse trade than the narrow miss it would be
	// guarding against.
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil
	}

	command := extractCommand(input)
	if command == "" {
		return nil
	}

	tokens := tokenizeShellWords(command)
	if !invokesBdOrGt(tokens) {
		return nil
	}

	match, ok := findCommandSubstitutionInTextFlag(command)
	if !ok {
		return nil
	}

	printCommandSubstitutionBlock(match, command)
	return NewSilentExit(2)
}

func printCommandSubstitutionBlock(match commandSubstitutionMatch, command string) {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "╔════════════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(os.Stderr, "║  ❌ COMMAND SUBSTITUTION IN TEXT ARGUMENT BLOCKED                       ║")
	fmt.Fprintln(os.Stderr, "╠════════════════════════════════════════════════════════════════════════╣")
	fmt.Fprintf(os.Stderr, "║  Flag:    %-63s ║\n", truncateStr(match.flag, 63))
	fmt.Fprintf(os.Stderr, "║  Command: %-63s ║\n", truncateStr(command, 63))
	fmt.Fprintln(os.Stderr, "║                                                                          ║")
	fmt.Fprintln(os.Stderr, "║  A backtick or $(...) inside this argument runs as a shell command     ║")
	fmt.Fprintln(os.Stderr, "║  when Claude Code's Bash tool wraps this line in eval \"...\". Quoting   ║")
	fmt.Fprintln(os.Stderr, "║  the argument does not protect it — the surrounding eval re-parses     ║")
	fmt.Fprintln(os.Stderr, "║  regardless of your quote style.                                        ║")
	fmt.Fprintln(os.Stderr, "║                                                                          ║")
	// Point at a real file, not a heredoc typed into this same Bash
	// command: a heredoc body is still text on this command line, so
	// its safety depends on exactly how the Bash wrapper parses it —
	// a real file written first with the Write tool does not depend
	// on that at all. See gt-h38j.1 review round 1 finding 1.
	if match.binary == "bd" {
		fmt.Fprintln(os.Stderr, "║  Safe form: use the Write tool to save the text to a real file, then   ║")
		fmt.Fprintln(os.Stderr, "║  pass the PATH via the flag's file variant — preferred because it      ║")
		fmt.Fprintln(os.Stderr, "║  does not depend on how this command line gets parsed:                 ║")
		fmt.Fprintln(os.Stderr, "║    bd ... --body-file <path>     (for --description/-d)                ║")
		fmt.Fprintln(os.Stderr, "║    bd ... --design-file <path>   (for --design)                        ║")
		fmt.Fprintln(os.Stderr, "║    bd ... <flag>-file <path>     (other text flags — see bd-nre)       ║")
	} else {
		fmt.Fprintln(os.Stderr, "║  Safe form: use the Write tool to save the text to a real file, then   ║")
		fmt.Fprintln(os.Stderr, "║  redirect it into --stdin — preferred because it does not depend on    ║")
		fmt.Fprintln(os.Stderr, "║  how this command line gets parsed:                                    ║")
		fmt.Fprintln(os.Stderr, "║    gt <command> ... --stdin < <path>                                   ║")
	}
	if match.flag == positionalArgLabel {
		fmt.Fprintln(os.Stderr, "║                                                                          ║")
		fmt.Fprintln(os.Stderr, "║  This text has no flag — it's a positional argument. Rewrite it        ║")
		fmt.Fprintln(os.Stderr, "║  without the backtick/$(...), or pass it via the flag form above       ║")
		fmt.Fprintln(os.Stderr, "║  if this subcommand has one (e.g. bd create --title-file <path>).      ║")
	}
	fmt.Fprintln(os.Stderr, "╚════════════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(os.Stderr, "")
}
