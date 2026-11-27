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
  - echo, printf or print of a secret variable, and heredocs or here-strings
    that print one, including through a variable assigned from it
  - reading /proc/<pid>/environ
  - any pipeline that starts with one of these, whatever filters follow

A secret variable is an upper-case name containing TOKEN, SECRET, PASSWORD,
PASSWD or CREDENTIAL, or ending in KEY or PAT as its own word (API_KEY,
GITHUB_PAT).

Allowed: listing names only (compgen -e, env | cut -d= -f1), presence checks
(test -n "$VAR" && echo set, "${VAR:+set}", ${#VAR}) and passing a secret to a
program that uses it.

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
  Which names exist?   compgen -e        (or: env | cut -d= -f1)
  Need the value?      pass "$VAR" straight to the program that uses it

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
		if isSecretVarName(ref) || s.tainted[ref] {
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
		for i, c := range p {
			if reason := s.command(c, p[i+1:], depth); reason != "" {
				return reason
			}
		}
	}
	return ""
}

func (s *envScan) command(c envCommand, rest envPipeline, depth int) string {
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
	return s.run(c.words, c, rest, depth)
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
func (s *envScan) run(words []envWord, c envCommand, rest envPipeline, depth int) string {
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
		return s.run(args, c, rest, depth)
	}

	for _, a := range args {
		if procEnviron.MatchString(a.text) && !slices.Contains([]string{"ls", "stat", "test", "[", "echo", "printf"}, name) {
			return "reading a process environment file"
		}
	}

	switch name {
	case "env":
		return s.env(args, c, rest, depth)
	case "printenv":
		return s.printenv(args, c, rest)
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
		for _, a := range args {
			if ref := s.secretRef(a); ref != "" {
				return name + " prints the secret variable " + ref
			}
		}
		return s.stdinPrint(c)
	case "cat", "tee", "head", "tail", "less", "more", "sed", "awk", "grep", "rev", "base64", "xxd", "od":
		return s.stdinPrint(c)
	case "bash", "sh", "zsh", "dash", "ksh":
		for i := 0; i+1 < len(args); i++ {
			if f := args[i].text; strings.HasPrefix(f, "-") && !strings.HasPrefix(f, "--") && strings.HasSuffix(f, "c") {
				return s.script(args[i+1].text, depth+1)
			}
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

func (s *envScan) assign(name string, w envWord) {
	if s.secretRef(w) != "" {
		s.tainted[name] = true
	}
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
			return "this prints the secret variable " + ref
		}
	}
	return ""
}

// env judges env, which prints the environment unless it is given a command.
func (s *envScan) env(args []envWord, c envCommand, rest envPipeline, depth int) string {
	nulSeparated := false
	for len(args) > 0 {
		a := args[0].text
		switch {
		case a == "--help" || a == "--version":
			return ""
		case a == "-0" || a == "--null":
			nulSeparated = true
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
			return s.run(args, c, rest, depth)
		}
		args = args[1:]
	}
	if !nulSeparated && namesOnly(c, rest) {
		return ""
	}
	return "env with no command prints the whole environment"
}

func (s *envScan) printenv(args []envWord, c envCommand, rest envPipeline) string {
	var names []string
	nulSeparated := false
	for _, a := range args {
		switch {
		case a.text == "--help" || a.text == "--version":
			return ""
		case a.text == "-0" || a.text == "--null":
			nulSeparated = true
		case !strings.HasPrefix(a.text, "-"):
			names = append(names, a.text)
		}
	}
	for _, n := range names {
		if isSecretVarName(n) || s.tainted[n] {
			return "printenv prints the secret variable " + n
		}
	}
	if len(names) > 0 || (!nulSeparated && namesOnly(c, rest)) {
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

// namesOnly reports whether the output of the listing c is cut down to variable
// names before anything prints it. Only filters that pass whole lines through
// may come before the cut.
func namesOnly(c envCommand, rest envPipeline) bool {
	for _, r := range c.redirects {
		if r.fd != "2" { // other redirects send values around the pipe
			return false
		}
	}
	for _, next := range rest {
		if len(next.words) == 0 || len(next.redirects) > 0 || len(next.heredocs) > 0 {
			return false
		}
		args := make([]string, len(next.words)-1)
		for i, w := range next.words[1:] {
			args[i] = w.text
		}
		switch path.Base(next.words[0].text) {
		case "sort":
			if slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "-o") || strings.HasPrefix(a, "--output") }) {
				return false
			}
		case "grep":
			if slices.ContainsFunc(args, func(a string) bool {
				return a == "--only-matching" || strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "o")
			}) {
				return false
			}
		case "cut":
			return cutKeepsNames(args)
		case "awk":
			return len(args) == 2 && args[0] == "-F=" && strings.ReplaceAll(args[1], " ", "") == "{print$1}"
		default:
			return false
		}
	}
	return false
}

// cutKeepsNames reports whether cut keeps exactly the text before the first =.
func cutKeepsNames(args []string) bool {
	delimiter, field := false, false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-s":
		case a == "-d=" || a == "--delimiter==":
			delimiter = true
		case a == "-d" && i+1 < len(args) && args[i+1] == "=":
			delimiter = true
			i++
		case a == "-f1" || a == "--fields=1":
			field = true
		case a == "-f" && i+1 < len(args) && args[i+1] == "1":
			field = true
			i++
		default:
			return false
		}
	}
	return delimiter && field
}
