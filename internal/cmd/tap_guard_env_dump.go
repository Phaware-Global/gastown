package cmd

import (
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// Contract of the env-dump guard.
//
// Threat model: a cooperative agent that prints secret values by accident, for
// example while debugging. The guard is not a sandbox. An agent that sets out to
// get around it can, and so can a command that falls under a known limit below;
// either way the value reaches the transcript and has to be rotated.
//
// Rule: a command is allowed only if the guard can show from its text that no
// secret value reaches output. When a variable name or the text of a script is
// built at run time the guard cannot know it, so it blocks:
//
//   - a command that prints variables by name (printenv, declare, typeset,
//     local, export and readonly with -p or a list of names, env with a name)
//     is given a name that is not a plain literal: an expansion, a command
//     substitution, a glob or a brace list
//   - an indirect expansion or nameref is printed: ${!name}, zsh ${(P)name} and
//     ${(e)name}, bash ${name@P}, and variables declared with -n
//   - eval, sh -c or env -S is given text that contains an expansion
//   - a shell reads its script from stdin, unless that is a here-string or
//     heredoc of literal text, which is then judged like a -c script
//
// Expanding a literal name that is not secret stays allowed (echo $HOME).
//
// Known limits, which are the residual risk:
//
//   - zsh flag delimiters that hide the name, e.g. ${(s:):)NAME}
//   - command names, and arguments other than names and scripts, that are built
//     at run time ($cmd, ps $flags)
//   - values passed on through anything but a direct assignment: function
//     arguments, loop variables, read, printf -v, files
//   - script files, programs in other languages, remote environments (ssh,
//     docker exec), tmux show-environment and set -x tracing

var tapGuardEnvDumpCmd = &cobra.Command{
	Use:   "env-dump",
	Short: "Block commands that print environment values or secret variables",
	Long: `Block Bash commands that would print environment values into an agent
transcript.

A transcript is sent to the model provider and kept in session history, so a
secret printed once has to be rotated. Redacting the output with sed or grep is
not a safe substitute: a pattern that does not match prints everything.

Blocked:
  - env, printenv, set, export, export -p, declare, typeset and readonly when
    they list variables, and ps forms that show process environments (e, -E)
  - printenv, declare -p, typeset or export -p of a secret variable
  - any of these prints, or echo and printf, of an indirect expansion
    (${!name}, zsh ${(P)name}), whose target cannot be known
  - zsh typeset -m and its relatives, which list variables by pattern
  - echo, printf or print of a secret variable, and heredocs or here-strings
    that print one, including through a variable assigned from it
  - reading /proc/<pid>/environ
  - any pipeline that starts with one of these, whatever filters follow
  - echo or printf of a secret piped into a program, which cannot be told apart
    from a program that displays its input

A secret variable is an upper-case name containing TOKEN, SECRET, PASSWORD,
PASSWD or CREDENTIAL, or ending in KEY or PAT as its own word (API_KEY,
GITHUB_PAT).

Allowed: listing names only (compgen -e, which is a bash builtin: from zsh run
bash -c 'compgen -e'), presence checks (test -n "$VAR" && echo set,
"${VAR:+set}", ${#VAR}), listing names or array keys by indirection
(${!prefix*}, ${!array[@]}) and passing a secret to a program that uses it. Cutting
env output down to names is blocked: the continuation lines of a multi-line value
get through cut and awk.

This is a check on ordinary shell commands, not a sandbox: a script in another
language, or a command name built at run time, is not seen.

The guard fails closed: an unreadable hook payload, an unterminated quote in a
command that mentions the environment, and nesting too deep to follow are all
blocked.

Exit codes:
  0 - Operation allowed
  2 - Operation BLOCKED`,
	RunE: runTapGuardEnvDump,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardEnvDumpCmd)
}

func runTapGuardEnvDump(cmd *cobra.Command, args []string) error {
	// A terminal means a person ran the guard by hand; there is no payload.
	if isStdinTerminal() {
		return nil
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		printEnvDumpBlock("the hook payload could not be read")
		return NewSilentExit(2)
	}
	command := extractCommand(input)
	if command == "" {
		printEnvDumpBlock("the hook payload carries no command to inspect")
		return NewSilentExit(2)
	}
	if reason := envDumpReason(command); reason != "" {
		printEnvDumpBlock(reason)
		return NewSilentExit(2)
	}
	return nil
}

// printEnvDumpBlock does not repeat the command, which may itself hold a value.
func printEnvDumpBlock(reason string) {
	fmt.Fprintf(os.Stderr, `
❌ ENVIRONMENT PRINT BLOCKED: %s

Command output goes into the session transcript, so a printed secret must be
rotated. Do not retry with a sed or grep redaction.

  Is a variable set?   test -n "$VAR" && echo set
  Which names exist?   bash -c 'compgen -e'
  Need the value?      pass "$VAR" straight to the program that uses it
  Feeding a tool?      let it read the environment (e.g. GH_TOKEN), or a file: cmd --password-stdin < file

`, reason)
}

// maxEnvScriptDepth bounds how far nested scripts (sh -c, eval, $(...)) are
// followed before the command is refused.
const maxEnvScriptDepth = 8

var (
	secretVarName = regexp.MustCompile(`TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL|APIKEY|(^|_)(KEY|PAT)$`)
	upperVarName  = regexp.MustCompile(`^[A-Z0-9_]+$`)
	// procEnviron matches a path argument, not prose that mentions one.
	procEnviron = regexp.MustCompile(`^/proc/(\$\([^)]*\)|\S)*environ$`)
	// suspiciousText decides whether a command that cannot be parsed is
	// refused.
	suspiciousText = regexp.MustCompile(`\b(env|printenv|set|export|declare|typeset|readonly|environ)\b|\$\{?[A-Z0-9_]*(TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL|KEY|PAT)`)
)

func isSecretVarName(name string) bool {
	return upperVarName.MatchString(name) && secretVarName.MatchString(name)
}

// envDumpReason says why command would print environment values, or "" when it
// would not.
func envDumpReason(command string) string {
	scan := &envScan{tainted: map[string]bool{}}
	return scan.script(command, 0)
}

type envScan struct {
	tainted map[string]bool // variables assigned from a secret
}

// secretRef returns the first secret variable w expands.
func (s *envScan) secretRef(w envWord) string {
	for _, ref := range w.refs {
		if ref == indirectRef || isSecretVarName(ref) || s.tainted[ref] {
			return ref
		}
	}
	return ""
}

func (s *envScan) script(script string, depth int) string {
	if depth > maxEnvScriptDepth {
		return "the command nests too deeply to inspect"
	}
	pipelines, err := lexEnvScript(script)
	if err != nil {
		if suspiciousText.MatchString(script) {
			return "the command cannot be parsed (" + err.Error() + ")"
		}
		return ""
	}
	for _, p := range pipelines {
		for _, c := range p {
			if reason := s.command(c, depth); reason != "" {
				return reason
			}
		}
	}
	return ""
}

func (s *envScan) command(c envCommand, depth int) string {
	words := slices.Clone(c.words)
	for _, r := range c.redirects {
		words = append(words, r.target)
	}
	for _, h := range c.heredocs {
		words = append(words, h.body)
	}
	for _, w := range words {
		for _, sub := range w.subs {
			if reason := s.script(sub, depth+1); reason != "" {
				return reason
			}
		}
	}
	for _, r := range c.redirects {
		if r.op == "<" && procEnviron.MatchString(r.target.text) {
			return "reading a process environment file"
		}
	}
	return s.run(c.words, c, depth)
}

// envWrappers are the programs that run another command, beyond those the
// memory-write guard already knows.
var envWrappers = map[string]map[string]bool{
	"caffeinate": {"-t": true, "-w": true},
	"noglob":     {},
	"nocorrect":  {},
	"watch":      {"-n": true},
}

// run judges the simple command in words, which is c's words or what is left of
// them after a wrapper.
func (s *envScan) run(words []envWord, c envCommand, depth int) string {
	for len(words) > 0 {
		w := words[0].text
		if shellKeywords[w] {
			words = words[1:]
			continue
		}
		m := shellAssignment.FindStringSubmatch(w)
		if m == nil {
			break
		}
		s.assign(m[1], words[0])
		words = words[1:]
	}
	if len(words) == 0 {
		return ""
	}
	name, args := path.Base(words[0].text), words[1:]

	valueFlags, wrapper := wrapperValueFlags[name]
	if !wrapper {
		valueFlags, wrapper = envWrappers[name]
	}
	if wrapper && name != "env" {
		for len(args) > 0 && strings.HasPrefix(args[0].text, "-") {
			flag := args[0].text
			if name == "command" && (flag == "-v" || flag == "-V") {
				return ""
			}
			if valueFlags[flag] && len(args) > 1 {
				args = args[1:]
			}
			args = args[1:]
		}
		if name == "timeout" && len(args) > 0 { // the duration
			args = args[1:]
		}
		return s.run(args, c, depth)
	}

	for _, a := range args {
		if procEnviron.MatchString(a.text) && !slices.Contains([]string{"ls", "stat", "test", "[", "echo", "printf"}, name) {
			return "reading a process environment file"
		}
	}

	switch name {
	case "env":
		return s.env(args, c, depth)
	case "printenv":
		return s.printenv(args)
	case "set":
		if len(args) == 0 {
			return "bare set lists every variable"
		}
	case "export", "declare", "typeset", "readonly", "local":
		return s.declare(name, args)
	case "ps":
		if psShowsEnvironment(args) {
			return "this ps form prints process environments"
		}
	case "echo", "printf", "print":
		// Blocked even when piped on: the next program may display its input.
		for _, a := range args {
			if ref := s.secretRef(a); ref != "" {
				return name + " prints " + printed(ref)
			}
		}
		return s.stdinPrint(c)
	case "cat", "tee", "head", "tail", "less", "more", "sed", "awk", "grep", "rev", "base64", "xxd", "od":
		return s.stdinPrint(c)
	case "bash", "sh", "zsh", "dash", "ksh":
		if script, ok := shellCommandString(name, args); ok {
			return s.script(script, depth+1)
		}
		for _, h := range c.heredocs {
			if reason := s.script(h.body.text, depth+1); reason != "" {
				return reason
			}
		}
	case "eval":
		text := make([]string, len(args))
		for i, a := range args {
			text[i] = a.text
		}
		return s.script(strings.Join(text, " "), depth+1)
	}
	return ""
}

// shellCommandString returns the script a shell is given with -c. It is the
// first operand, so other options may stand on either side of -c or share its
// word.
func shellCommandString(shell string, args []envWord) (string, bool) {
	command := false
	for i := 0; i < len(args); i++ {
		a := args[i].text
		switch {
		case a == "--" || a == "-":
			if command && i+1 < len(args) {
				return args[i+1].text, true
			}
			return "", false
		case strings.HasPrefix(a, "--"):
			if a == "--rcfile" || a == "--init-file" || a == "--emulate" {
				i++
			}
		case len(a) > 1 && (a[0] == '-' || a[0] == '+'):
			values := 0
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'c':
					command = command || a[0] == '-'
				case 'o', 'O':
					// zsh and ksh also take the option name in the same word.
					if (shell == "zsh" || shell == "ksh") && j+1 < len(a) {
						j = len(a)
						continue
					}
					values++
				}
			}
			i += values
		default:
			return a, command
		}
	}
	return "", false
}

func (s *envScan) assign(name string, w envWord) {
	if s.secretRef(w) != "" {
		s.tainted[name] = true
	}
}

// printed says what printing the ref found by secretRef would reveal.
func printed(ref string) string {
	if ref == indirectRef {
		return "an indirect expansion, whose target may be a secret"
	}
	return "the secret variable " + ref
}

// stdinPrint judges a heredoc or here-string fed to a program that prints its
// input.
func (s *envScan) stdinPrint(c envCommand) string {
	var inputs []envWord
	for _, r := range c.redirects {
		if r.op == "<<<" {
			inputs = append(inputs, r.target)
		}
	}
	for _, h := range c.heredocs {
		inputs = append(inputs, h.body)
	}
	for _, in := range inputs {
		if ref := s.secretRef(in); ref != "" {
			return "this prints " + printed(ref)
		}
	}
	return ""
}

// env judges env, which prints the environment unless it is given a command.
func (s *envScan) env(args []envWord, c envCommand, depth int) string {
	for len(args) > 0 {
		a := args[0].text
		switch {
		case a == "--help" || a == "--version":
			return ""
		case a == "-u" || a == "-C" || a == "-P":
			if len(args) > 1 {
				args = args[1:]
			}
		case a == "-S" || a == "--split-string":
			if len(args) > 1 {
				return s.script(args[1].text, depth+1)
			}
		case strings.HasPrefix(a, "-"):
		case shellAssignment.MatchString(a):
			s.assign(shellAssignment.FindStringSubmatch(a)[1], args[0])
		default:
			return s.run(args, c, depth)
		}
		args = args[1:]
	}
	return "env with no command prints the whole environment"
}

func (s *envScan) printenv(args []envWord) string {
	var names []string
	for _, a := range args {
		if slices.Contains(a.refs, indirectRef) {
			return "printenv prints " + printed(indirectRef)
		}
		switch {
		case a.text == "--help" || a.text == "--version":
			return ""
		case !strings.HasPrefix(a.text, "-"):
			names = append(names, a.text)
		}
	}
	for _, n := range names {
		if isSecretVarName(n) || s.tainted[n] {
			return "printenv prints the secret variable " + n
		}
	}
	if len(names) > 0 {
		return ""
	}
	return "bare printenv prints the whole environment"
}

// declare judges export and its relatives, which list variables when given no
// name and print the named ones with -p.
func (s *envScan) declare(builtin string, args []envWord) string {
	var flags string
	var names []envWord
	for _, a := range args {
		if len(a.text) > 1 && (a.text[0] == '-' || a.text[0] == '+') {
			flags += a.text[1:]
			continue
		}
		names = append(names, a)
	}
	if strings.ContainsAny(flags, "fF") { // functions, not variables
		return ""
	}
	// zsh -m takes the names as patterns and prints whatever matches.
	pattern := strings.Contains(flags, "m") &&
		slices.ContainsFunc(names, func(w envWord) bool { return !strings.Contains(w.text, "=") })
	if pattern {
		return builtin + " -m lists the variables matching a pattern and their values"
	}
	// A number after zsh's -p picks the print format; it is not a name.
	if strings.Contains(flags, "p") &&
		!slices.ContainsFunc(names, func(w envWord) bool { return strings.Trim(w.text, "0123456789") != "" }) {
		names = nil
	}
	if len(names) == 0 {
		if builtin == "local" && flags == "" {
			return ""
		}
		return builtin + " with no variable name lists variables and their values"
	}
	prints := strings.Contains(flags, "p") ||
		flags == "" && (builtin == "declare" || builtin == "typeset")
	for _, w := range names {
		name, _, assigned := strings.Cut(w.text, "=")
		switch {
		case assigned:
			s.assign(name, w)
		case prints && slices.Contains(w.refs, indirectRef):
			return builtin + " prints " + printed(indirectRef)
		case prints && (isSecretVarName(name) || s.tainted[name]):
			return builtin + " prints the secret variable " + name
		}
	}
	return ""
}

// psShowsEnvironment reports macOS -E and the BSD-style e option.
func psShowsEnvironment(args []envWord) bool {
	for i := 0; i < len(args); i++ {
		a := args[i].text
		switch {
		case slices.Contains([]string{"-o", "-O", "-p", "-u", "-U", "-g", "-G", "-t", "o", "p", "t", "U"}, a):
			i++
		case strings.HasPrefix(a, "-"):
			if !strings.HasPrefix(a, "--") && strings.Contains(a, "E") {
				return true
			}
		case strings.Contains(a, "e") && strings.Trim(a, "aAcCefghjlLmMrsSTuvwxX") == "":
			return true
		}
	}
	return false
}
