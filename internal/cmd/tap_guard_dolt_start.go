package cmd

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var tapGuardDoltStartCmd = &cobra.Command{
	Use:   "dolt-start",
	Short: "Block agents from starting a Dolt server themselves",
	Long: `Block 'bd dolt start' and 'dolt sql-server' in Gas Town agent sessions.

Both bind the shared Dolt port (3307) to whatever store the agent's current
directory points at. From an agent's own directory that is an empty store, so
every other agent then reads empty data until the canonical server reclaims the
port. Gas Town owns the server: agents check it with 'gt dolt status' and
escalate when it is down.

Reads the tool input from stdin in the Claude Code hook protocol. The opencode
plugin (used by the deacon and dogs) pipes the same JSON in from its
tool.execute.before hook, since opencode sessions do not run Claude hooks.

Exit codes:
  0 - Operation allowed
  2 - Operation BLOCKED`,
	RunE: runTapGuardDoltStart,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardDoltStartCmd)
}

// The start commands are matched where a command word can begin: the start of
// any line or after a shell operator, in text with heredoc bodies and quoted
// strings removed (see splitQuoted).
// A prefix of VAR=value assignments and launcher words (nohup, env, ...) is
// allowed. The command word itself must be bd or dolt, so 'gt dolt ...' never
// matches, and 'bd create "fix dolt start"' does not either: only flags may sit
// between bd and 'dolt'.
const (
	doltStartBoundary = "(?m)(?:^|[|&;(`]|\\bthen\\b|\\bdo\\b)\\s*"
	doltStartPrefix   = "(?:[A-Za-z_][A-Za-z0-9_]*=\\S*\\s+)*(?:(?:nohup|exec|env|command|time|setsid|sudo)\\s+(?:-\\S+\\s+)*(?:[A-Za-z_][A-Za-z0-9_]*=\\S*\\s+)*)*"
	doltFlags         = "(?:\\s+-{1,2}[\\w-]+(?:=\\S+)?(?:\\s+[^\\s-]\\S*)?)*"
)

var doltStartPatterns = []struct {
	re     *regexp.Regexp
	reason string
}{
	{
		regexp.MustCompile(doltStartBoundary + doltStartPrefix + "(?:\\S*/)?bd" + doltFlags + "\\s+dolt\\s+(?:start|restart)\\b"),
		"bd dolt start ignores dolt.auto-start and binds the shared port to this directory's store",
	},
	{
		regexp.MustCompile(doltStartBoundary + doltStartPrefix + "(?:\\S*/)?dolt" + doltFlags + "\\s+sql-server\\b"),
		"dolt sql-server binds the shared port to this directory's store",
	},
}

var heredocStart = regexp.MustCompile(`<<-?\s*(?:'([^']+)'|"([^"]+)"|\\?(\w+))`)

func runTapGuardDoltStart(cmd *cobra.Command, args []string) error {
	if !isGasTownAgentContext() {
		return nil
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil // fail open
	}
	command := extractCommand(input)
	if command == "" {
		return nil
	}
	reason, ok := matchesDoltServerStartWithin(command, doltStartMatchBudget)
	if !ok {
		fmt.Fprintf(os.Stderr, "[gastown] dolt-start guard timed out after %v (command allowed)\n", doltStartMatchBudget)
		return nil
	}
	if reason != "" {
		printDoltStartBlock(reason, command)
		return NewSilentExit(2)
	}
	return nil
}

// doltStartMatchBudget bounds the matcher: the quote scanner can backtrack
// exponentially on crafted input, and this guard runs in front of every agent
// command. It fails open, like the guard's other error paths.
const doltStartMatchBudget = 200 * time.Millisecond

// matchesDoltServerStartWithin runs matchesDoltServerStart for at most budget.
// ok is false when the budget ran out, in which case the matcher is abandoned
// (the guard process exits right after) and no verdict is given.
func matchesDoltServerStartWithin(command string, budget time.Duration) (reason string, ok bool) {
	done := make(chan string, 1)
	go func() { done <- matchesDoltServerStart(command) }()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case reason = <-done:
		return reason, true
	case <-timer.C:
		return "", false
	}
}

// shellCArg matches the end of text that has just named a shell's -c script, so
// the quoted string that follows is code rather than data.
var shellCArg = regexp.MustCompile(`\b(?:ba|z|da|k)?sh\s+(?:-\S+\s+)*-[A-Za-z]*c\s*$`)

// matchesDoltServerStart returns why command starts a Dolt server, or "".
func matchesDoltServerStart(command string) string {
	bare, nested := splitQuoted(stripHeredocBodies(command))
	for _, p := range doltStartPatterns {
		if p.re.MatchString(bare) {
			return p.reason
		}
	}
	// Each nested script is strictly shorter than the text it came from.
	for _, n := range nested {
		if reason := matchesDoltServerStart(n); reason != "" {
			return reason
		}
	}
	return ""
}

// splitQuoted separates a command line into its code and the code hidden in its
// quotes. Quoted text is data (an escalation or mail that quotes 'bd dolt start'
// to warn about it), so bare has every '...', "..." and $'...' replaced by an
// empty pair of the same quote; the patterns then see only real command words.
// Two kinds of quoted text do run, and come back in nested to be matched on
// their own: the script after sh -c, and $(...) or `...` inside double quotes.
// A quote that never closes is left in bare, so it can only add matches.
func splitQuoted(s string) (bare string, nested []string) {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '\\' && i+1 < len(s):
			b.WriteString(s[i : i+2])
			i += 2
		case c == '\'' || (c == '$' && i+1 < len(s) && s[i+1] == '\''):
			start := i + 1
			if c == '$' {
				start++
			}
			end := closingSingleQuote(s, start, c == '$')
			if end < 0 {
				b.WriteString(s[i:])
				return b.String(), nested
			}
			if shellCArg.MatchString(b.String()) {
				nested = append(nested, s[start:end])
			}
			b.WriteString("''")
			i = end + 1
		case c == '"':
			end := closingDoubleQuote(s, i+1)
			if end < 0 {
				b.WriteString(s[i:])
				return b.String(), nested
			}
			if shellCArg.MatchString(b.String()) {
				nested = append(nested, s[i+1:end])
			}
			nested = append(nested, substitutions(s[i+1:end])...)
			b.WriteString(`""`)
			i = end + 1
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nested
}

// closingSingleQuote returns the index of the quote ending a single-quoted
// string whose body starts at from, or -1. Only $'...' honors backslash escapes.
func closingSingleQuote(s string, from int, ansiC bool) int {
	for i := from; i < len(s); i++ {
		switch {
		case ansiC && s[i] == '\\':
			i++
		case s[i] == '\'':
			return i
		}
	}
	return -1
}

// closingDoubleQuote returns the index of the quote ending a double-quoted
// string whose body starts at from, or -1. A $(...) inside may hold quotes of
// its own, so it is skipped as a unit.
func closingDoubleQuote(s string, from int) int {
	for i := from; i < len(s); i++ {
		switch {
		case s[i] == '\\':
			i++
		case s[i] == '"':
			return i
		case s[i] == '$' && i+1 < len(s) && s[i+1] == '(':
			end := closingParen(s, i+2)
			if end < 0 {
				return -1
			}
			i = end
		}
	}
	return -1
}

// closingParen returns the index of the ')' matching a '(' just before from, or -1.
func closingParen(s string, from int) int {
	for depth, i := 1, from; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '\'':
			if end := closingSingleQuote(s, i+1, false); end >= 0 {
				i = end
			}
		case '"':
			if end := closingDoubleQuote(s, i+1); end >= 0 {
				i = end
			}
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return -1
}

// substitutions returns the commands run by $(...) and `...` inside the body of
// a double-quoted string.
func substitutions(body string) []string {
	var out []string
	for i := 0; i < len(body); i++ {
		switch {
		case body[i] == '\\':
			i++
		case body[i] == '$' && i+1 < len(body) && body[i+1] == '(':
			end := closingParen(body, i+2)
			if end < 0 {
				end = len(body)
			}
			out = append(out, body[i+2:end])
			i = end
		case body[i] == '`':
			end := strings.IndexByte(body[i+1:], '`')
			if end < 0 {
				end = len(body) - i - 1
			}
			out = append(out, body[i+1:i+1+end])
			i += end + 1
		}
	}
	return out
}

// stripHeredocBodies drops the text of heredocs, which is data (a mail body, a
// bead description) and often quotes the very commands this guard blocks.
func stripHeredocBodies(command string) string {
	var out []string
	terminator := ""
	for _, line := range strings.Split(command, "\n") {
		if terminator != "" {
			if strings.TrimSpace(line) == terminator {
				terminator = ""
			}
			continue
		}
		out = append(out, line)
		if m := heredocStart.FindStringSubmatch(line); m != nil {
			terminator = m[1] + m[2] + m[3]
		}
	}
	return strings.Join(out, "\n")
}

func printDoltStartBlock(reason, originalCommand string) {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(os.Stderr, "║  ❌ STARTING A DOLT SERVER IS BLOCKED                            ║")
	fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintf(os.Stderr, "║  Command: %-53s ║\n", truncateStr(originalCommand, 53))
	fmt.Fprintf(os.Stderr, "║  Reason:  %-53s ║\n", truncateStr(reason, 53))
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  Gas Town owns the Dolt server. Check it with: gt dolt status    ║")
	fmt.Fprintln(os.Stderr, "║  If it is down, escalate; do not start one from your own cwd:    ║")
	fmt.Fprintln(os.Stderr, "║    gt escalate -s HIGH \"Dolt: server unreachable\"                ║")
	fmt.Fprintln(os.Stderr, "║  (an operator restarts it with: gt dolt start)                   ║")
	fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(os.Stderr, "")
}
