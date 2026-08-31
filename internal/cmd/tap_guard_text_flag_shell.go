package cmd

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/google/shlex"
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
  - or, for subcommands with no flag name at all (gt nudge/escalate,
    bd create/comment), inside the positional free-text argument.

It deliberately does NOT fire on a backtick or $( anywhere else on the
command line — legitimate command substitution outside a text-bearing
flag argument is common and correct (e.g. the Dolt diagnostics in
CLAUDE.md use "$(date +%s)" inside a "tee" target path, not inside any
of the flags above). To tell those apart, the command line is first
split into shell-operator-delimited segments (on ";", "&&", "||", "|"
and newline, all quote/backtick/$(...)-aware) so a different program's
flags elsewhere on the line — even on another line, even without
surrounding whitespace around the operator — are never mistaken for
bd/gt's own arguments. Each segment is then tokenized with
github.com/google/shlex, which implements POSIX-correct backslash and
quote handling (round 1 and round 2 of gt-h38j.1 review both found new
bypasses in a hand-rolled per-segment tokenizer; the mayor's ruling on
this bead was to stop re-deriving that state machine one bypass at a
time and use a real parser instead).

Non-goals: this guard does not sanitize, escape, or rewrite the
argument text, and it does not touch Claude Code's eval wrapper (not
ours to change). It blocks and names the safe alternative:
  - bd: write the text to a file and pass it via the flag's file/stdin
    form where one exists today (--body-file for --description/-d,
    --design-file for --design); for the other text flags, rewrite the
    text without the backtick/$( construct until a file/stdin variant
    lands (tracked in bd-nre).
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
// immediately following the "bd"/"gt" invocation, after any global
// flags) to true when one of its positional (non-flag) arguments
// carries agent-authored free text with no flag name to key off — the
// same eval-executes-it hazard as a text-bearing flag. gt nudge's and
// gt escalate's message, and bd's create/comment text, are the guard's
// core case (an agent describing what it found or why it's blocked),
// not an edge: "gt nudge mayor/ \"check `id` output\"" has no
// text-bearing flag at all, and the message is positional
// (internal/cmd/nudge.go: "nudge <target> [message]";
// internal/cmd/escalate.go: "escalate [description]").
var positionalTextSubcommands = map[string]bool{
	"nudge":    true,
	"create":   true,
	"comment":  true,
	"escalate": true,
}

// shellSeparatorPattern documents (for readers of splitTopLevelSegments)
// the operators that end one shell command and start another on the
// same line: ";", "&&", "||", "|", and a bare newline.

// rawBinaryRef matches a standalone "bd" or "gt" word — used only as a
// fallback when a segment fails to tokenize at all (see
// findCommandSubstitutionInTextFlag).
var rawBinaryRef = regexp.MustCompile(`(^|[^A-Za-z0-9_./-])(bd|gt)([^A-Za-z0-9_./-]|$)`)

// splitTopLevelSegments splits a command line into the shell segments
// delimited by ";", "&&", "||", "|" and newline — the operators that
// separate one program invocation from the next. The scan tracks
// quoting state (single quotes, double quotes, backtick spans, and
// $(...) nesting depth) so an operator character that merely appears
// inside quoted or substituted text is never mistaken for a real
// separator, and a real separator glued directly to an adjacent word
// with no surrounding whitespace (e.g. "cd /x;bd update ...") is still
// recognized — round-2 finding: the previous whitespace-token-based
// splitter missed exactly this shape, letting "bd" hide inside the
// single token "/x;bd" (a false negative), while also merging an
// entire multi-line command into one segment and false-positiving on
// an unrelated program's flags on a later line (round-2 finding 2's
// mirror image).
//
// Known limitation, accepted as out of scope: a literal ')' inside a
// quoted string nested inside a $(...) span (e.g. $(echo "a)b")) will
// close the paren-depth count early, since this scanner does not track
// quote state *inside* a $(...) span. This guard is a best-effort hook
// check, not a full shell parser or a security boundary in itself —
// the same tradeoff already documented for the tokenizer below.
func splitTopLevelSegments(command string) []string {
	var segments []string
	var cur strings.Builder
	n := len(command)
	i := 0
	inSingle := false
	inDouble := false
	inBacktick := false
	parenDepth := 0

	flush := func() {
		if cur.Len() > 0 {
			segments = append(segments, cur.String())
			cur.Reset()
		}
	}

	for i < n {
		c := command[i]

		if inSingle {
			cur.WriteByte(c)
			if c == '\'' {
				inSingle = false
			}
			i++
			continue
		}

		// A backslash escapes the very next byte outright wherever it
		// appears (outside single quotes): never let what it escapes
		// be mistaken for a quote delimiter or a segment separator.
		if c == '\\' && i+1 < n {
			cur.WriteByte(c)
			cur.WriteByte(command[i+1])
			i += 2
			continue
		}

		if inDouble {
			cur.WriteByte(c)
			if c == '"' {
				inDouble = false
			}
			i++
			continue
		}

		if inBacktick {
			cur.WriteByte(c)
			if c == '`' {
				inBacktick = false
			}
			i++
			continue
		}

		if parenDepth > 0 {
			cur.WriteByte(c)
			switch c {
			case '(':
				parenDepth++
			case ')':
				parenDepth--
			}
			i++
			continue
		}

		switch c {
		case '\'':
			inSingle = true
			cur.WriteByte(c)
			i++
		case '"':
			inDouble = true
			cur.WriteByte(c)
			i++
		case '`':
			inBacktick = true
			cur.WriteByte(c)
			i++
		case '$':
			cur.WriteByte(c)
			i++
			if i < n && command[i] == '(' {
				parenDepth = 1
				cur.WriteByte('(')
				i++
			}
		case '\n', ';':
			flush()
			i++
		case '|':
			flush()
			if i+1 < n && command[i+1] == '|' {
				i += 2
			} else {
				i++
			}
		case '&':
			if i+1 < n && command[i+1] == '&' {
				flush()
				i += 2
			} else {
				cur.WriteByte(c)
				i++
			}
		default:
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return segments
}

func invokesBdOrGt(tokens []string) bool {
	for _, t := range tokens {
		if t == "bd" || t == "gt" {
			return true
		}
	}
	return false
}

func firstInvocationBinary(tokens []string) string {
	for _, t := range tokens {
		if t == "bd" || t == "gt" {
			return t
		}
	}
	return ""
}

// commandSubstitutionMatch describes a text-bearing flag argument (or
// positional free-text argument) that contains a shell
// command-substitution construct.
type commandSubstitutionMatch struct {
	flag    string // the flag token as written, e.g. "--append-notes" or "-m"
	binary  string // "bd" or "gt" — the invocation this argument belongs to
	snippet string // the offending argument text
}

// flagAndValue splits a token into (flag, value, hasValue). Handles the
// "--flag=value" form; for tokens with no "=" (or a short flag), hasValue
// is false and the caller should look at the NEXT token as the argument
// instead.
func flagAndValue(tok string) (flag, value string, hasValue bool) {
	if idx := strings.IndexByte(tok, '='); idx >= 0 && strings.HasPrefix(tok, "--") {
		return tok[:idx], tok[idx+1:], true
	}
	// Attached long-flag value: shlex merges a long flag with an
	// adjacent quoted run into one word, same as the short-flag case
	// below, e.g. --append-notes"see `find /`" tokenizes as one word
	// with no "=" and no separate next token to inspect.
	if strings.HasPrefix(tok, "--") {
		for f := range textBearingLongFlags {
			if len(tok) > len(f) && strings.HasPrefix(tok, f) {
				return f, tok[len(f):], true
			}
		}
	}
	// Attached short-flag value, the git-style spelling an agent
	// commonly writes: -m"note with $(id)" tokenizes as one word
	// ("-m" concatenated with the quoted run) since shlex merges
	// quoted and unquoted fragments of a single shell word. Without
	// this, only the separate "-m value" form was inspected.
	if len(tok) > 2 && tok[0] == '-' && tok[1] != '-' {
		short := tok[:2]
		if textBearingShortFlags[short] {
			return short, tok[2:], true
		}
	}
	return tok, "", false
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
// to each shell-operator-delimited segment of the line (see
// splitTopLevelSegments) so that a different program appearing
// elsewhere on the same line — or a different line — is never mistaken
// for the bd/gt invocation's own arguments.
func findCommandSubstitutionInTextFlag(command string) (commandSubstitutionMatch, bool) {
	for _, segment := range splitTopLevelSegments(command) {
		if strings.TrimSpace(segment) == "" {
			continue
		}
		tokens, err := shlex.Split(segment)
		if err != nil {
			// Unbalanced quoting defeats precise tokenization of this
			// segment. Per the mayor's ruling on this bead (round 2
			// produced six new hand-rolled-tokenizer bypasses): when a
			// real parser can't make sense of the input, fail closed
			// rather than silently letting it through. The remedy is
			// the same one the guard already gives: split the command
			// or use a file.
			if rawBinaryRef.MatchString(segment) && containsCommandSubstitution(segment) {
				m := rawBinaryRef.FindStringSubmatch(segment)
				return commandSubstitutionMatch{flag: "(unparsable)", binary: m[2], snippet: segment}, true
			}
			continue
		}
		if !invokesBdOrGt(tokens) {
			continue
		}
		if match, ok := findCommandSubstitutionInSegment(tokens); ok {
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
func findCommandSubstitutionInSegment(tokens []string) (commandSubstitutionMatch, bool) {
	if match, ok := findCommandSubstitutionInFlags(tokens); ok {
		return match, true
	}
	return findCommandSubstitutionInPositional(tokens)
}

func findCommandSubstitutionInFlags(tokens []string) (commandSubstitutionMatch, bool) {
	binary := firstInvocationBinary(tokens)
	for i, tok := range tokens {
		flag, value, hasValue := flagAndValue(tok)
		if !isTextBearingFlag(flag) {
			continue
		}

		var argText string
		if hasValue {
			argText = value
		} else if i+1 < len(tokens) {
			argText = tokens[i+1]
		} else {
			continue
		}

		if containsCommandSubstitution(argText) {
			return commandSubstitutionMatch{flag: flag, binary: binary, snippet: argText}, true
		}
	}
	return commandSubstitutionMatch{}, false
}

// isFlagToken reports whether tok should be treated as a flag (and
// therefore skipped) during positional free-text scanning, rather than
// as candidate positional text. A real flag token can never contain
// whitespace — only a quoted argument can — so a token like
// "- ran find /" (an agent's message that happens to start with a
// single dash, e.g. a markdown bullet) is correctly NOT a flag despite
// its leading '-'. This replaces the previous "any leading '-'" check,
// which both false-positived on flag values (see isRedirectOperator's
// doc comment) and false-negatived on exactly this shape.
func isFlagToken(tok string) bool {
	if tok == "" || tok[0] != '-' {
		return false
	}
	if strings.ContainsAny(tok, " \t\n") {
		return false
	}
	if strings.HasPrefix(tok, "--") {
		return true
	}
	return len(tok) == 2 // short flag: "-" plus one character, e.g. -m, -s, -C
}

// isRedirectOperator reports whether tok is a shell redirection
// operator (optionally prefixed by a file descriptor number), e.g.
// ">", ">>", "<", "2>", "&>". findCommandSubstitutionInPositional stops
// scanning at the first one: a redirect target is shell-interpreted
// output routing, not free text passed to bd/gt, and unlike a bd/gt
// argument it commonly contains an unquoted $(...) with an internal
// space (e.g. "> /tmp/o-$(date +%s).log", which a naive word-splitter
// sees as two words, one of which looks like a positional match).
func isRedirectOperator(tok string) bool {
	s := tok
	for len(s) > 0 && s[0] >= '0' && s[0] <= '9' {
		s = s[1:]
	}
	if s == ">" || s == ">>" || s == "<" || s == "<<" {
		return true
	}
	return strings.HasPrefix(s, ">&") || strings.HasPrefix(s, "&>")
}

// findCommandSubstitutionInPositional covers bd/gt subcommands whose
// hazardous argument has no flag name at all — gt nudge/escalate's
// message and bd create/comment's title/body are the guard's core case
// (an agent describing what it found or why it's blocked), not an
// edge: "gt nudge mayor/ \"check `id` output\"" has no text-bearing
// flag, and the message is positional.
func findCommandSubstitutionInPositional(tokens []string) (commandSubstitutionMatch, bool) {
	binIdx := -1
	for i, t := range tokens {
		if t == "bd" || t == "gt" {
			binIdx = i
			break
		}
	}
	if binIdx < 0 {
		return commandSubstitutionMatch{}, false
	}
	binary := tokens[binIdx]

	// Resolve the subcommand by scanning for the first token that IS
	// one of the known positional-text subcommands, rather than by
	// walking skipBinaryGlobalArgs's per-binary global-flag table: a
	// novel global flag that table doesn't recognize (either binary's
	// table — skipGtGlobalArgs in particular still aborts to -1 on
	// any unrecognized flag) would otherwise smuggle the subcommand
	// past this scan undetected. Fall back to skipBinaryGlobalArgs
	// only when no known subcommand token appears at all, to preserve
	// the "not a positional-text subcommand" no-match result for
	// commands like "bd show gt-abc".
	subIdx := -1
	for i := binIdx + 1; i < len(tokens); i++ {
		if positionalTextSubcommands[tokens[i]] {
			subIdx = i
			break
		}
	}
	if subIdx < 0 {
		subIdx = skipBinaryGlobalArgs(binary, tokens, binIdx+1)
		if subIdx < 0 || subIdx >= len(tokens) || !positionalTextSubcommands[tokens[subIdx]] {
			return commandSubstitutionMatch{}, false
		}
	}

	for i := subIdx + 1; i < len(tokens); i++ {
		tok := tokens[i]
		if isRedirectOperator(tok) {
			break // everything after a redirect is shell-interpreted, not a bd/gt argument
		}
		if isFlagToken(tok) {
			// A flag's own value is the flag's semantic argument, not
			// free text destined for the positional slot — only skip
			// the value when it's a separate token (no attached
			// "=value" already consumed by this same token), e.g. -p 1
			// or --labels "round-$(date +%s)" on "bd create ... -p 1
			// --labels ...".
			if !strings.Contains(tok, "=") && i+1 < len(tokens) {
				i++
			}
			continue
		}
		if containsCommandSubstitution(tok) {
			return commandSubstitutionMatch{
				flag:    positionalArgLabel,
				binary:  binary,
				snippet: tok,
			}, true
		}
	}
	return commandSubstitutionMatch{}, false
}

// skipBinaryGlobalArgs returns the index of the subcommand token
// (e.g. "create", "nudge") for the given binary ("bd" or "gt"),
// skipping any global flags between the invocation and the
// subcommand. Returns -1 if the subcommand can't be determined.
func skipBinaryGlobalArgs(binary string, tokens []string, start int) int {
	switch binary {
	case "bd":
		return skipBdGlobalArgs(tokens, start)
	case "gt":
		return skipGtGlobalArgs(tokens, start)
	default:
		return -1
	}
}

// skipBdGlobalArgs handles bd's global flags that can appear before the
// subcommand — most importantly -C/--directory <dir>, the documented
// way to target a repo (bd create --repo silently loses data, per
// gt/bd known gotchas), which round-2's positional scan missed
// entirely because it assumed the subcommand was always
// tokens[binIdx+1]. Unlike tap_guard_push_main.go's skipGitGlobalArgs,
// an unrecognized flag is skipped rather than aborting to -1: aborting
// would make findCommandSubstitutionInPositional report no match (the
// opposite of conservative), letting a novel global flag smuggle the
// subcommand past this scan undetected.
func skipBdGlobalArgs(tokens []string, start int) int {
	consumesNext := map[string]bool{
		"-C": true, "--directory": true,
		"--actor": true,
		"--db":    true,
		"--dolt-auto-commit": true,
	}
	booleanFlags := map[string]bool{
		"--global": true, "--ignore-schema-skew": true, "--json": true,
		"--profile": true, "-q": true, "--quiet": true, "--readonly": true,
		"--sandbox": true, "-v": true, "--verbose": true, "-V": true,
		"--version": true, "-h": true, "--help": true,
	}
	i := start
	for i < len(tokens) {
		f := tokens[i]
		if !strings.HasPrefix(f, "-") {
			return i
		}
		if strings.Contains(f, "=") {
			i++
			continue
		}
		if booleanFlags[f] {
			i++
			continue
		}
		if consumesNext[f] {
			i += 2
			continue
		}
		// Unknown flag: assume it's boolean and keep scanning rather
		// than aborting — see the fail-open note above.
		i++
	}
	return -1
}

// skipGtGlobalArgs is gt's equivalent of skipBdGlobalArgs. gt has no
// global flags that take a separate value, so this only needs to skip
// the boolean -h/--help and -v/--version if present before the
// subcommand.
func skipGtGlobalArgs(tokens []string, start int) int {
	booleanFlags := map[string]bool{"-h": true, "--help": true, "-v": true, "--version": true}
	i := start
	for i < len(tokens) {
		f := tokens[i]
		if !strings.HasPrefix(f, "-") {
			return i
		}
		if booleanFlags[f] {
			i++
			continue
		}
		return -1
	}
	return -1
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
	fmt.Fprintln(os.Stderr, "║  ❌ COMMAND SUBSTITUTION IN TEXT ARGUMENT BLOCKED                      ║")
	fmt.Fprintln(os.Stderr, "╠════════════════════════════════════════════════════════════════════════╣")
	flatCommand := strings.Join(strings.Fields(command), " ")
	fmt.Fprintf(os.Stderr, "║  Flag:    %-60s ║\n", truncateStr(match.flag, 60))
	fmt.Fprintf(os.Stderr, "║  Command: %-60s ║\n", truncateStr(flatCommand, 60))
	fmt.Fprintln(os.Stderr, "║                                                                        ║")
	fmt.Fprintln(os.Stderr, "║  A backtick or $(...) inside this argument runs as a shell command     ║")
	fmt.Fprintln(os.Stderr, "║  when Claude Code's Bash tool wraps this line in eval \"...\". Quoting   ║")
	fmt.Fprintln(os.Stderr, "║  the argument does not protect it — the surrounding eval re-parses     ║")
	fmt.Fprintln(os.Stderr, "║  regardless of your quote style.                                       ║")
	fmt.Fprintln(os.Stderr, "║                                                                        ║")
	// Point at a real file, not a heredoc typed into this same Bash
	// command: a heredoc body is still text on this command line, so
	// its safety depends on exactly how the Bash wrapper parses it —
	// a real file written first with the Write tool does not depend
	// on that at all. See gt-h38j.1 review round 1 finding 1.
	if match.binary == "bd" {
		fmt.Fprintln(os.Stderr, "║  Safe form: use the Write tool to save the text to a real file, then   ║")
		fmt.Fprintln(os.Stderr, "║  pass the PATH via the flag's file variant, where one exists today —   ║")
		fmt.Fprintln(os.Stderr, "║  preferred because it does not depend on how this line gets parsed:    ║")
		fmt.Fprintln(os.Stderr, "║    bd ... --body-file <path>     (for --description/-d)                ║")
		fmt.Fprintln(os.Stderr, "║    bd ... --design-file <path>   (for --design)                        ║")
		fmt.Fprintln(os.Stderr, "║  No file/stdin variant exists yet for the other text flags             ║")
		fmt.Fprintln(os.Stderr, "║  (--append-notes, --notes, --acceptance, --title, ...) — rewrite the   ║")
		fmt.Fprintln(os.Stderr, "║  text without the backtick/$(...) construct instead.                   ║")
	} else {
		fmt.Fprintln(os.Stderr, "║  Safe form: use the Write tool to save the text to a real file, then   ║")
		fmt.Fprintln(os.Stderr, "║  redirect it into --stdin — preferred because it does not depend on    ║")
		fmt.Fprintln(os.Stderr, "║  how this command line gets parsed:                                    ║")
		fmt.Fprintln(os.Stderr, "║    gt <command> ... --stdin < <path>                                   ║")
	}
	if match.flag == positionalArgLabel {
		fmt.Fprintln(os.Stderr, "║                                                                        ║")
		if match.binary == "bd" {
			fmt.Fprintln(os.Stderr, "║  This text has no flag — it's a positional argument. Rewrite it        ║")
			fmt.Fprintln(os.Stderr, "║  without the backtick/$(...) construct instead.                        ║")
		} else {
			fmt.Fprintln(os.Stderr, "║  This text has no flag — it's a positional argument.                   ║")
		}
	}
	fmt.Fprintln(os.Stderr, "╚════════════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(os.Stderr, "")
}
