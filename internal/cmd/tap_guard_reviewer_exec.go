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

It reads the command line, so it catches the usual test and build runners,
scripts started as ./name, and scripts handed to a shell or sourced, not every
way of running code. It also applies the dangerous-command and dolt-start checks
to what "gt reviewer exec" wraps.

Reads the tool input from stdin in the Claude Code hook protocol.

Exit codes:
  0 - Operation allowed
  2 - Operation BLOCKED`,
	RunE: func(_ *cobra.Command, _ []string) error {
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil // fail open
		}
		raw := rawCommand(input)
		if reason := wrappedReason(raw); reason != "" {
			printDangerousBlock(reason, raw)
			return NewSilentExit(2)
		}
		what, found := unwrappedPRCode(raw)
		if !found {
			return nil
		}
		fmt.Fprintf(os.Stderr, "❌ BLOCKED: %q runs PR code in the reviewer's own environment.\n"+
			"Run it as: gt reviewer exec -- <command> [args...]\n"+
			"One command per call. It starts outside the checkout, so name the checkout:\n"+
			"  gt reviewer exec -- go -C \"$PWD\" test ./...\n", what)
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
	"npm":     {"test", "t", "run", "run-script", "exec", "start", "install", "i", "add", "ci"},
	"pnpm":    {"test", "t", "run", "exec", "start", "install", "i", "add", "ci"},
	"yarn":    {"test", "run", "exec", "start", "install", "add"},
	"bun":     {"test", "run", "x", "install", "i", "add"},
	"npx":     nil,
	"pytest":  nil,
	"cargo":   {"test", "run", "build", "bench"},
	"python":  {"pytest", "unittest"},
	"python3": {"pytest", "unittest"},
}

// toolValueFlags are flags that may precede the subcommand and take a value.
var toolValueFlags = map[string]bool{"-C": true, "--prefix": true, "--cwd": true, "--dir": true}

// wrappedReason applies, to what "gt reviewer exec" wraps, the guards that are
// registered by command prefix and so never run on the wrapped command.
func wrappedReason(command string) string {
	inner := reviewerExecPrefix.ReplaceAllString(command, "$1")
	if inner == command {
		return ""
	}
	if reason := dangerousReason(inner); reason != "" {
		return reason
	}
	reason, _ := matchesDoltServerStartWithin(inner, doltStartMatchBudget)
	return reason
}

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
	var (
		words []string
		stdin *string // text the current command is fed
		fed   bool    // the current command reads a pipe or a file
		op    string  // redirect operator awaiting its target
	)
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
		case tok.stdin != nil:
			stdin = tok.stdin
		case tok.op && (tok.text == opWrite || tok.text == opRedirect || tok.text == opHereString):
			op = tok.text
		case end || tok.op:
			if what, found := unwrappedInCommand(words, stdin, fed, depth); found {
				return what, true
			}
			// A pipe carries over the line breaks that may follow it.
			if tok.text == "|" || len(words) > 0 || tok.text != ";" {
				fed = tok.text == "|"
			}
			words, stdin, op = nil, nil, ""
		case tok.text == "":
		case op != "":
			switch op {
			case opHereString:
				text := tok.text
				stdin = &text
			case opRedirect:
				fed = true
			}
			op = ""
		default:
			words = append(words, tok.text)
		}
	}
	return "", false
}

// unwrappedInCommand checks one simple command, fed stdin if non-nil or else a
// pipe or file if fed. "gt reviewer exec ..." passes because its program is gt.
func unwrappedInCommand(words []string, stdin *string, fed bool, depth int) (string, bool) {
	scope := shellScope{keepReviewerExec: true}
	name, args := scope.commandName(words)
	if name == "" {
		return "", false
	}
	if program := words[len(words)-len(args)-1]; strings.HasPrefix(program, "./") {
		return program, true
	}
	if isSource(name) && len(args) > 0 {
		return name + " " + args[0], true
	}
	nested := func(text string) (string, bool) {
		unclear := false
		return unwrappedInTokens(lexer{unclear: &unclear}.split(text), depth+1)
	}
	if isShell(name) {
		for i := 0; i+1 < len(args); i++ {
			if f := args[i]; strings.HasPrefix(f, "-") && !strings.HasPrefix(f, "--") && strings.HasSuffix(f, "c") {
				return nested(args[i+1])
			}
		}
		// These run no script.
		if len(args) == 1 && (args[0] == "--version" || args[0] == "--help") {
			return "", false
		}
		script, fromStdin := scope.shellInput(args)
		switch {
		case !fromStdin:
			return name + " " + script, true
		case stdin != nil:
			return nested(*stdin)
		case fed:
			return name + " reading a script from a pipe or file", true
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
	// A bare "yarn" installs.
	asksAbout := func(a string) bool { return a == "-v" || a == "--version" || a == "-h" || a == "--help" }
	if name == "yarn" && !slices.ContainsFunc(args, asksAbout) {
		return name, true
	}
	return "", false
}
