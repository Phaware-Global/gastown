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
				if j := strings.IndexByte(command[i+1:], '"'); j >= 0 {
					b.WriteString(command[i+1 : i+1+j])
					i += j + 2
				} else {
					b.WriteString(command[i+1:])
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
	return tok.text, "", false, 0
}

func containsCommandSubstitution(s string) bool {
	return strings.ContainsRune(s, '`') || strings.Contains(s, "$(")
}

// findCommandSubstitutionInTextFlag scans command for a text-bearing
// flag whose argument contains a backtick or $(. It does not itself
// check invokesBdOrGt — callers combine both.
func findCommandSubstitutionInTextFlag(command string) (commandSubstitutionMatch, bool) {
	tokens := tokenizeShellWords(command)
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

func runTapGuardTextFlagShell(cmd *cobra.Command, args []string) error {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil // fail open on hook-protocol weirdness
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
	if match.binary == "bd" {
		fmt.Fprintln(os.Stderr, "║  Safe form: write the text to a file and pass it via the flag's        ║")
		fmt.Fprintln(os.Stderr, "║  file/stdin variant instead of inline text:                            ║")
		fmt.Fprintln(os.Stderr, "║    bd ... --body-file <path>     (for --description/-d)                ║")
		fmt.Fprintln(os.Stderr, "║    bd ... --design-file <path>   (for --design)                        ║")
		fmt.Fprintln(os.Stderr, "║    bd ... <flag>-file <path>     (other text flags — see bd-nre)       ║")
		fmt.Fprintln(os.Stderr, "║    Use \"-\" as <path> to read from stdin/heredoc instead of a file.     ║")
	} else {
		fmt.Fprintln(os.Stderr, "║  Safe form: use --stdin instead of inline text:                        ║")
		fmt.Fprintln(os.Stderr, "║    gt <command> ... --stdin <<'EOF'                                    ║")
		fmt.Fprintln(os.Stderr, "║    <your text here>                                                     ║")
		fmt.Fprintln(os.Stderr, "║    EOF                                                                  ║")
	}
	fmt.Fprintln(os.Stderr, "╚════════════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(os.Stderr, "")
}
