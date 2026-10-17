package cmd

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

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
// any line (once heredoc bodies are removed), or after a shell operator or a -c
// wrapper.
// A prefix of VAR=value assignments and launcher words (nohup, env, ...) is
// allowed. The command word itself must be bd or dolt, so 'gt dolt ...' never
// matches, and 'bd create "fix dolt start"' does not either: only flags may sit
// between bd and 'dolt'.
const (
	doltStartBoundary = "(?m)(?:^|[|&;(`]|\\bthen\\b|\\bdo\\b|-[a-z]*c\\s+['\"])\\s*"
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
	if reason := matchesDoltServerStart(command); reason != "" {
		printDoltStartBlock(reason, command)
		return NewSilentExit(2)
	}
	return nil
}

// matchesDoltServerStart returns why command starts a Dolt server, or "".
func matchesDoltServerStart(command string) string {
	code := stripHeredocBodies(command)
	for _, p := range doltStartPatterns {
		if p.re.MatchString(code) {
			return p.reason
		}
	}
	return ""
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
