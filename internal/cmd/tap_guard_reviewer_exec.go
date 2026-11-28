package cmd

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

var tapGuardReviewerExecCmd = &cobra.Command{
	Use:   "reviewer-exec",
	Short: "Require 'gt reviewer exec' for commands that run PR code",
	Long: `Block the common ways of running a pull request's code unless they go
through 'gt reviewer exec', which keeps it away from the town's tmux and Dolt
servers. Registered for the Reviewer.

It reads the command line, so it catches the usual test and build runners and
scripts started as ./name, not every way of running code.

Reads the tool input from stdin in the Claude Code hook protocol.

Exit codes:
  0 - Operation allowed
  2 - Operation BLOCKED`,
	RunE: func(_ *cobra.Command, _ []string) error {
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil // fail open
		}
		what, found := unwrappedPRCode(extractCommand(input))
		if !found {
			return nil
		}
		fmt.Fprintf(os.Stderr, "❌ BLOCKED: %q runs PR code in the reviewer's own environment.\n"+
			"Run it as: gt reviewer exec -- <command> [args...]\n"+
			"For a shell line: gt reviewer exec -- sh -c '<line>'\n", what)
		return NewSilentExit(2)
	},
}

func init() {
	tapGuardCmd.AddCommand(tapGuardReviewerExecCmd)
}

// prCodeSubcommands are the subcommands of each tool that run the project's
// code. An empty list means any use of the tool does.
var prCodeSubcommands = map[string][]string{
	"go":      {"test", "run", "generate"},
	"make":    nil,
	"npm":     {"test", "t", "run", "run-script", "exec", "start", "install", "ci"},
	"pnpm":    {"test", "t", "run", "exec", "start", "install"},
	"yarn":    {"test", "run", "exec", "start", "install"},
	"bun":     {"test", "run", "x", "install"},
	"npx":     nil,
	"pytest":  nil,
	"cargo":   {"test", "run", "build", "bench"},
	"python":  {"pytest", "unittest"},
	"python3": {"pytest", "unittest"},
}

// toolValueFlags are flags that may precede the subcommand and take a value.
var toolValueFlags = map[string]bool{"-C": true, "--prefix": true, "--cwd": true, "--dir": true}

// unwrappedPRCode returns the first part of command that runs PR code other
// than through "gt reviewer exec".
func unwrappedPRCode(command string) (string, bool) {
	unclear := false
	return unwrappedInTokens(lexer{unclear: &unclear}.split(command), 0)
}

func unwrappedInTokens(tokens []shellToken, depth int) (string, bool) {
	if depth > maxShellDepth {
		return "", false
	}
	var words []string
	skipTarget := false
	for i := 0; i <= len(tokens); i++ {
		var tok shellToken
		end := i == len(tokens)
		if !end {
			tok = tokens[i]
			if what, found := unwrappedInTokens(tok.sub, depth+1); found {
				return what, true
			}
		}
		switch {
		case !end && tok.op && (tok.text == opWrite || tok.text == opRedirect || tok.text == opHereString):
			skipTarget = true
		case end || tok.op:
			if what, found := unwrappedInCommand(words, depth); found {
				return what, true
			}
			words, skipTarget = nil, false
		case skipTarget:
			skipTarget = false
		case tok.text != "":
			words = append(words, tok.text)
		}
	}
	return "", false
}

// unwrappedInCommand checks one simple command. "gt reviewer exec ..." passes
// because its program is gt.
func unwrappedInCommand(words []string, depth int) (string, bool) {
	name, args := shellScope{}.commandName(words)
	if name == "" {
		return "", false
	}
	if program := words[len(words)-len(args)-1]; strings.HasPrefix(program, "./") {
		return program, true
	}
	if isShell(name) {
		for i := 0; i+1 < len(args); i++ {
			if f := args[i]; strings.HasPrefix(f, "-") && !strings.HasPrefix(f, "--") && strings.HasSuffix(f, "c") {
				unclear := false
				return unwrappedInTokens(lexer{unclear: &unclear}.split(args[i+1]), depth+1)
			}
		}
		return "", false
	}
	subcommands, known := prCodeSubcommands[name]
	if !known {
		return "", false
	}
	if subcommands == nil {
		return name, true
	}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case toolValueFlags[a]:
			i++
		case strings.HasPrefix(a, "-"): // for python, -m precedes the module
		case slices.Contains(subcommands, a):
			return name + " " + a, true
		default:
			return "", false
		}
	}
	return "", false
}
