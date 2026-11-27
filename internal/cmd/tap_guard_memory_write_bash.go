package cmd

import (
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/memwritebaseline"
	"golang.org/x/text/cases"
)

// This file finds the files a Bash command writes, so the memory-write guard
// can judge them. It follows the ordinary ways an agent writes a file from the
// shell: redirects, tee, in-place editors, and the copy, move and create
// commands, including through cd, simple variables, command substitutions and
// scripts fed to a shell. It is not a shell: a path computed at run time, or a
// write made by another program (a Python or Node script, say), is not seen.
//
// The verdict only ever adds to that of the frozen detector in
// memwritebaseline. Where text goes to another shell it is read both as bash
// and as zsh reads it, and a command that names a memory path is blocked if
// any of it cannot be read for certain.

// shellToken is one word, after quote removal, or one operator. It may instead
// carry something the command around it runs or reads.
type shellToken struct {
	text  string
	op    bool
	sub   []shellToken // the commands of a substitution, run in a subshell
	stdin *string      // the body of a heredoc
}

// Operators lexShell emits besides the command separators. Every redirect that
// writes a file is opWrite; every other redirect is opRedirect or opHereString.
// All of them take the following word as their target.
const (
	opWrite      = ">"
	opRedirect   = "<"
	opHereString = "<<<"
)

// substitutionMark stands in a word for text computed at run time, so that the
// paths a substitution reads are not taken for part of the word.
const substitutionMark = "$()"

// lexer reads a command as one family of shells does. Inside $( ), bash and sh
// end a heredoc at the first line starting delimiter+")" and nest ${ } only on
// "${", while zsh reads on to a line holding the delimiter alone if there is one
// and nests on a bare "{" too. Either reading can quote or comment out what the
// other runs, so every text a shell reads is lexed both ways.
type lexer struct {
	zsh     bool
	unclear *bool // set on text this reading cannot be sure of
}

func (l lexer) doubt() { *l.unclear = true }

// split splits a command line into tokens. Comments are dropped and heredoc
// bodies are set aside, so text that merely mentions a write is not read as one.
func (l lexer) split(s string) []shellToken {
	toks, _ := l.commands(s, false)
	return toks
}

type heredoc struct {
	tag    string
	quoted bool // a quoted delimiter keeps the body from being expanded
	body   *string
}

// commands lexes s and returns the tokens and how much of s they cover. When
// nested, s follows a "$(" and lexing stops at the ")" that closes it.
func (l lexer) commands(s string, nested bool) ([]shellToken, int) {
	var (
		toks     []shellToken
		word     strings.Builder
		inWord   bool
		quoted   bool
		parens   int
		heredocs []heredoc // heredocs whose bodies start at the next newline
		wantTag  bool      // the next word is a heredoc delimiter
	)
	flush := func() {
		if !inWord {
			return
		}
		if wantTag {
			body := new(string)
			heredocs = append(heredocs, heredoc{tag: word.String(), quoted: quoted, body: body})
			toks = append(toks, shellToken{stdin: body})
			wantTag = false
		} else {
			toks = append(toks, shellToken{text: word.String()})
		}
		word.Reset()
		inWord, quoted = false, false
	}
	emit := func(op string) {
		flush()
		toks = append(toks, shellToken{text: op, op: true})
	}
	at := func(i int) byte {
		if i < len(s) {
			return s[i]
		}
		return 0
	}
	// substitute lexes the substitution at s[i], if there is one, into the
	// current word and returns the index of its last byte.
	substitute := func(i int, inQuotes bool) (int, bool) {
		subs, last, ok := l.substitution(s, i, inQuotes)
		if !ok {
			return i, false
		}
		toks = append(toks, subs...)
		word.WriteString(substitutionMark)
		inWord = true
		return last, true
	}

	i := 0
	for ; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			if i+1 < len(s) {
				i++
				if s[i] != '\n' { // backslash-newline continues the line
					word.WriteByte(s[i])
					inWord, quoted = true, true
				}
			}
		case '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				l.doubt()
				end = len(s) - i - 1
			}
			word.WriteString(s[i+1 : i+1+end])
			inWord, quoted = true, true
			i += end + 1
		case '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if last, ok := substitute(i, true); ok {
					i = last
					continue
				}
				if s[i] == '\\' && strings.IndexByte("\"\\$`\n", at(i+1)) >= 0 {
					i++
				}
				if i < len(s) {
					word.WriteByte(s[i])
				}
			}
			if i >= len(s) {
				l.doubt()
			}
			inWord, quoted = true, true
		case '$', '`':
			if c == '$' && at(i+1) == '$' { // the process id: no "${" or "$(" follows
				word.WriteString("$$")
				inWord = true
				i++
				continue
			}
			// A ")" inside ${ } does not close the enclosing "$(".
			if nested && c == '$' && at(i+1) == '{' {
				end := l.parameterEnd(s, i)
				if end < 0 {
					l.doubt()
				} else {
					inner := s[i+2 : end]
					if subs := l.expansions(inner); len(subs) > 0 {
						toks = append(toks, subs...)
						word.WriteString(substitutionMark)
					} else {
						word.WriteString(s[i : end+1])
					}
					inWord, i = true, end
					continue
				}
			}
			if last, ok := substitute(i, false); ok {
				i = last
				continue
			}
			word.WriteByte(c)
			inWord = true
		case ' ', '\t', '\r':
			flush()
		case '#':
			if inWord {
				word.WriteByte(c)
				continue
			}
			for i+1 < len(s) && s[i+1] != '\n' {
				i++
			}
		case '\n':
			emit(";")
			for _, h := range heredocs {
				body, last, ok := l.heredocBody(s, i, h.tag, nested)
				if !ok {
					// No delimiter: the "<<" was most likely a shift, so the
					// lines after it are still commands.
					l.doubt()
					break
				}
				*h.body, i = body, last
				if !h.quoted {
					*h.body = unescapeHeredoc(body)
					toks = append(toks, l.expansions(body)...)
				}
			}
			heredocs = nil
		case '(':
			if end := arithmeticEnd(s, i); end >= 0 {
				// In (( )) the operators < > and << are arithmetic.
				flush()
				toks = append(toks, l.expansions(s[i+2:end-1])...)
				i = end
				continue
			}
			parens++
			emit("(")
		case ')':
			if nested && parens == 0 {
				flush()
				return toks, i + 1
			}
			parens--
			emit(")")
		case ';':
			emit(";")
		case '|':
			if at(i+1) == '|' || at(i+1) == '&' {
				i++
			}
			emit("|")
		case '&':
			switch at(i + 1) {
			case '>': // &> and &>> write both streams to a file
				continue
			case '&':
				i++
			}
			emit("&")
		case '>', '<':
			// A bare number glued to the operator is its file descriptor.
			if inWord && !quoted && strings.Trim(word.String(), "0123456789") == "" {
				word.Reset()
				inWord = false
			}
			if c == '<' {
				switch {
				case at(i+1) == '<' && at(i+2) == '<':
					i += 2
					emit(opHereString)
				case at(i+1) == '<':
					i++
					if at(i+1) == '-' {
						i++
					}
					flush()
					wantTag = true
				case at(i+1) == '>': // <> opens for reading and writing
					i++
					emit(opWrite)
				default:
					emit(opRedirect)
				}
				continue
			}
			if at(i+1) == '>' {
				i++
			}
			op := opWrite
			if at(i+1) == '&' {
				i++
				// >&2 and >&- duplicate or close a descriptor; >&name writes name.
				if n := at(i + 1); n == '-' || (n >= '0' && n <= '9') {
					op = opRedirect
				}
			}
			if at(i+1) == '|' || at(i+1) == '!' { // clobber forms
				i++
			}
			emit(op)
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	flush()
	if nested {
		l.doubt() // no ")" closed the "$("
	}
	return toks, i
}

// substitution lexes the $(...), $((...)) or `...` that starts at s[i]. It
// returns the commands run as sub tokens and the index of the closing byte.
func (l lexer) substitution(s string, i int, inQuotes bool) ([]shellToken, int, bool) {
	switch {
	case s[i] == '`':
		escapable := "\\`$"
		if inQuotes {
			escapable += `"`
		}
		var body strings.Builder
		j := i + 1
		for ; j < len(s) && s[j] != '`'; j++ {
			if s[j] == '\\' && j+1 < len(s) && strings.IndexByte(escapable, s[j+1]) >= 0 {
				j++
			}
			body.WriteByte(s[j])
		}
		if j >= len(s) {
			l.doubt()
		}
		return subshell(l.split(body.String())), j, true
	case !strings.HasPrefix(s[i:], "$("):
		return nil, i, false
	}
	if end := arithmeticEnd(s, i+1); end >= 0 {
		return l.expansions(s[i+3 : end-1]), end, true
	}
	toks, n := l.commands(s[i+2:], true)
	return subshell(toks), i + 1 + n, true
}

func subshell(toks []shellToken) []shellToken {
	if len(toks) == 0 {
		return nil
	}
	return []shellToken{{sub: toks}}
}

// expansions returns the commands run by substitutions in text that is expanded
// but not run itself: arithmetic, or an unquoted heredoc body.
func (l lexer) expansions(text string) []shellToken {
	var toks []shellToken
	for i := 0; i < len(text); i++ {
		if text[i] == '\\' {
			i++
			continue
		}
		if subs, last, ok := l.substitution(text, i, false); ok {
			toks = append(toks, subs...)
			i = last
		}
	}
	return toks
}

// arithmeticEnd returns the index of the last ")" of the (( )) that starts at
// s[i], or -1 if s[i:] does not start one. As in the shell, "((a); (b))" is two
// nested subshells.
func arithmeticEnd(s string, i int) int {
	if !strings.HasPrefix(s[i:], "((") {
		return -1
	}
	depth := 0
	for j := i; j < len(s); j++ {
		switch s[j] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 1 {
				if j+1 < len(s) && s[j+1] == ')' {
					return j + 1
				}
				return -1
			}
		}
	}
	return -1
}

// heredocBody reads the body that follows the newline at s[i], up to the line
// holding tag alone or, inside $( ), a line starting tag+")" as l reads it. It
// returns the body and the index of the last byte used.
func (l lexer) heredocBody(s string, i int, tag string, nested bool) (body string, last int, ok bool) {
	paren := -1 // the first line starting tag+")"
	var parenLast int
	for start := i + 1; start < len(s); {
		line := s[start:]
		if end := strings.IndexByte(line, '\n'); end >= 0 {
			line = line[:end]
		}
		trimmed := strings.TrimLeft(line, "\t")
		if trimmed == tag {
			return s[i+1 : start], start + len(line), true
		}
		if nested && paren < 0 && strings.HasPrefix(trimmed, tag+")") {
			paren, parenLast = start, start+len(line)-len(trimmed)+len(tag)-1
			if !l.zsh {
				break
			}
		}
		start += len(line) + 1
	}
	if paren >= 0 {
		return s[i+1 : paren], parenLast, true
	}
	return "", i, false
}

// unescapeHeredoc returns the text a command reads from an unquoted heredoc,
// where a backslash escapes only another backslash, "$", "`" and a newline.
func unescapeHeredoc(body string) string {
	return strings.NewReplacer(`\\`, `\`, `\$`, `$`, "\\`", "`", "\\\n", "").Replace(body)
}

// parameterEnd returns the index of the "}" closing the "${" at s[i], or -1.
func (l lexer) parameterEnd(s string, i int) int {
	depth := 0
	var quote byte
	for j := i; j < len(s); j++ {
		switch c := s[j]; {
		case c == '\\' && quote != '\'':
			j++
		case quote != '\'' && c == '$' && j+1 < len(s) && s[j+1] == '$':
			j++
		case quote != '\'' && (c == '`' || c == '$' && j+1 < len(s) && s[j+1] == '('):
			// A "}" in a substitution's comment or heredoc closes nothing.
			_, j, _ = l.substitution(s, j, quote == '"')
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '$' && j+1 < len(s) && s[j+1] == '{':
			depth++
			j++
		case c == '{' && l.zsh:
			depth++
		case c == '}':
			if depth--; depth == 0 {
				return j
			}
		}
	}
	return -1
}

// shellScope is the state a command inherits from the ones before it.
type shellScope struct {
	scan  *shellScan
	depth int // how many shells deep the command is
	cwd   string
	vars  map[string]string
}

func (s shellScope) clone() shellScope {
	vars := make(map[string]string, len(s.vars))
	for k, v := range s.vars {
		vars[k] = v
	}
	return shellScope{scan: s.scan, depth: s.depth, cwd: s.cwd, vars: vars}
}

// shellScan is what one command line shares across all the shells it starts.
type shellScan struct {
	unclear bool            // some text could not be read for certain
	written map[string]bool // files the command writes
}

// maxShellDepth bounds the shells inside shells that are followed, since each
// level whose readings differ doubles the work.
const maxShellDepth = 6

var (
	shellVarRef     = regexp.MustCompile(`\$(\{[A-Za-z_][A-Za-z0-9_]*\}|[A-Za-z_][A-Za-z0-9_]*)`)
	shellAssignment = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
)

// expand substitutes variables the command itself assigned; others stay as
// written.
func (s shellScope) expand(word string) string {
	return shellVarRef.ReplaceAllStringFunc(word, func(ref string) string {
		if v, ok := s.vars[strings.Trim(ref, "${}")]; ok {
			return v
		}
		return ref
	})
}

// resolve turns a path argument into the path it names from the current
// directory.
func (s shellScope) resolve(word string) string {
	p := s.expand(word)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "~") && !strings.HasPrefix(p, "$") && s.cwd != "" {
		p = s.cwd + "/" + p
	}
	return path.Clean(p)
}

// unreadableMemoryCommand stands for the path when a command is blocked because
// it names a memory path and cannot be read for certain.
const unreadableMemoryCommand = "(unclear shell syntax naming a memory path)"

// bashMemoryWrite returns the first Claude Code memory path that command,
// started in cwd, writes. It blocks whatever the baseline detector blocks.
func bashMemoryWrite(command, cwd string) (string, bool) {
	// The baseline answers first, so nothing below can lose one of its blocks.
	if p, ok := memwritebaseline.Write(command, cwd); ok {
		return p, true
	}
	scan := &shellScan{written: map[string]bool{}}
	if p, ok := scanShellWrites(command, shellScope{scan: scan, cwd: cwd, vars: map[string]string{}}); ok {
		return p, true
	}
	if scan.unclear && namesMemoryPath(command, cwd) {
		return unreadableMemoryCommand, true
	}
	return "", false
}

// namesMemoryPath reports whether the command or its directory mentions a
// memory path, however it is quoted.
func namesMemoryPath(command, cwd string) bool {
	text := strings.NewReplacer(`'`, "", `"`, "", `\`, "").Replace(command + "\n" + cwd)
	text = cases.Fold().String(text)
	return strings.Contains(text, ".claude/projects") && strings.Contains(text, "memory")
}

// scanShellWrites scans text a shell reads, under both the bash and the zsh
// reading. Readings that differ leave the text unclear.
func scanShellWrites(text string, scope shellScope) (string, bool) {
	if scope.depth > maxShellDepth {
		scope.scan.unclear = true
		return "", false
	}
	scope.depth++
	bash := lexer{unclear: &scope.scan.unclear}.split(text)
	zsh := lexer{zsh: true, unclear: &scope.scan.unclear}.split(text)
	readings := [][]shellToken{bash}
	if !reflect.DeepEqual(bash, zsh) {
		scope.scan.unclear = true
		readings = append(readings, zsh)
	}
	for _, toks := range readings {
		if p, ok := scanShellTokens(toks, scope.clone()); ok {
			return p, true
		}
	}
	return "", false
}

func scanShellTokens(toks []shellToken, scope shellScope) (string, bool) {
	var (
		outer []shellScope // scopes saved at each open parenthesis
		words []string     // the current command's words
		redir []string     // files the current command redirects into
		stdin *string      // text the current command is fed
		op    string       // redirect operator awaiting its target
		piped bool         // the current command reads a pipe
		input []string     // files the current command reads from
	)
	end := func() (string, bool) {
		defer func() { words, redir, input, stdin, op = nil, nil, nil, nil, "" }()
		// The shell opens redirects before the command runs.
		for _, r := range redir {
			p := scope.resolve(r)
			if isMemoryTarget(p) {
				return p, true
			}
			scope.scan.written[p] = true
		}
		if name, _ := scope.commandName(words); isShell(name) {
			for _, f := range input {
				if scope.scan.written[scope.resolve(f)] {
					scope.scan.unclear = true
				}
			}
		}
		return scope.run(words, stdin, piped)
	}

	for _, t := range toks {
		switch {
		case t.sub != nil:
			if p, ok := scanShellTokens(t.sub, scope.clone()); ok {
				return p, true
			}
		case t.stdin != nil:
			stdin = t.stdin
		case !t.op:
			switch op {
			case opWrite:
				redir = append(redir, t.text)
			case opHereString:
				text := t.text
				stdin = &text
			case opRedirect:
				input = append(input, t.text)
			default:
				words = append(words, t.text)
			}
			op = ""
		case t.text == opWrite || t.text == opRedirect || t.text == opHereString:
			op = t.text
		default:
			// "<(" and ">(" hand the command a file made as it runs.
			if t.text == "(" && op != "" {
				if name, _ := scope.commandName(words); isShell(name) || isSource(name) {
					scope.scan.unclear = true
				}
			}
			ran := len(words) > 0
			if p, ok := end(); ok {
				return p, true
			}
			// A pipe carries over the line breaks that may follow it.
			if t.text == "|" || ran || t.text != ";" && t.text != "(" {
				piped = t.text == "|"
			}
			switch t.text {
			case "(":
				outer = append(outer, scope.clone())
			case ")":
				if n := len(outer); n > 0 {
					scope, outer = outer[n-1], outer[:n-1]
				}
			}
		}
	}
	return end()
}

func isMemoryTarget(p string) bool {
	// The trailing slash makes the memory directory itself count.
	return p != "" && (isClaudeCodeMemoryPath(p) || isClaudeCodeMemoryPath(p+"/"))
}

// wrapperValueFlags lists, for commands that run another command, the flags
// whose value is a separate word.
var wrapperValueFlags = map[string]map[string]bool{
	"builtin": {}, "command": {}, "exec": {}, "nohup": {}, "stdbuf": {}, "time": {},
	"env":     {"-u": true, "-C": true},
	"nice":    {"-n": true},
	"sudo":    {"-u": true, "-g": true},
	"timeout": {"-s": true, "-k": true},
	"xargs":   {"-I": true, "-n": true, "-P": true, "-L": true, "-d": true, "-E": true, "-s": true},
}

var shellKeywords = map[string]bool{
	"!": true, "{": true, "}": true, "if": true, "then": true, "elif": true,
	"else": true, "while": true, "until": true, "do": true,
}

// commandName strips keywords, assignments and wrappers from words and returns
// the program that runs with its arguments.
func (s shellScope) commandName(words []string) (string, []string) {
	for len(words) > 0 {
		w := words[0]
		if shellKeywords[w] || shellAssignment.MatchString(w) {
			words = words[1:]
			continue
		}
		name := path.Base(s.expand(w))
		valueFlags, wrapper := wrapperValueFlags[name]
		if !wrapper {
			return name, words[1:]
		}
		words = words[1:]
		for len(words) > 0 && strings.HasPrefix(words[0], "-") {
			if valueFlags[words[0]] && len(words) > 1 {
				words = words[1:]
			}
			words = words[1:]
		}
		if name == "timeout" && len(words) > 0 { // the duration
			words = words[1:]
		}
	}
	return "", nil
}

func isShell(name string) bool {
	switch name {
	case "bash", "sh", "zsh", "dash", "ksh":
		return true
	}
	return false
}

func isSource(name string) bool { return name == "source" || name == "." }

// run applies one simple command, fed stdin if non-nil or else a pipe if piped,
// to the scope and reports a memory write.
func (s *shellScope) run(words []string, stdin *string, piped bool) (string, bool) {
	name, args := s.commandName(words)
	switch {
	case name == "": // assignments with no command set variables
		s.assign(words)
	case name == "export" || name == "local" || name == "declare" || name == "typeset" || name == "readonly":
		s.assign(args)
	case name == "cd" || name == "pushd":
		s.chdir(args)
	case isShell(name):
		for i := 0; i+1 < len(args); i++ {
			if f := args[i]; strings.HasPrefix(f, "-") && !strings.HasPrefix(f, "--") && strings.HasSuffix(f, "c") {
				return scanShellWrites(args[i+1], s.clone())
			}
		}
		script, fromStdin := s.shellInput(args)
		return s.runScript(script, fromStdin, stdin, piped)
	case isSource(name):
		if len(args) == 0 {
			return "", false
		}
		return s.runScript(args[0], s.isStdin(args[0]), stdin, piped)
	case name == "eval":
		return scanShellWrites(strings.Join(args, " "), s.clone())
	default:
		expanded := make([]string, len(args))
		for i, a := range args {
			expanded[i] = s.expand(a)
		}
		isMemory := func(word string) bool { return isMemoryTarget(s.resolve(word)) }
		for _, target := range writtenFiles(name, expanded, isMemory) {
			p := s.resolve(target)
			if isMemoryTarget(p) {
				return p, true
			}
			s.scan.written[p] = true
		}
	}
	return "", false
}

// runScript follows a shell, or source, that runs the file script or else its
// standard input.
func (s *shellScope) runScript(script string, fromStdin bool, stdin *string, piped bool) (string, bool) {
	// A script named at run time may be the standard input too.
	unknown := strings.Contains(s.expand(script), "$")
	if (fromStdin || unknown) && stdin != nil {
		if p, ok := scanShellWrites(*stdin, s.clone()); ok {
			return p, true
		}
	}
	// What a pipe carries, or a file this command wrote holds, is not read.
	if unknown || fromStdin && stdin == nil && piped || !fromStdin && s.scan.written[s.resolve(script)] {
		s.scan.unclear = true
	}
	return "", false
}

// stdinPaths name a process's standard input.
var stdinPaths = []string{"/dev/stdin", "/dev/fd/0", "/proc/self/fd/0"}

func (s shellScope) isStdin(word string) bool {
	return slices.Contains(stdinPaths, s.resolve(word))
}

// shellInput reports what a shell given args, which hold no -c, runs: the
// script file named, or its standard input.
func (s shellScope) shellInput(args []string) (script string, fromStdin bool) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-o" || a == "+o" || a == "-O" || a == "+O" || a == "--rcfile" || a == "--init-file":
			i++
		case a == "-" || strings.HasPrefix(a, "--") && a != "--":
		case strings.HasPrefix(a, "-") && strings.Contains(a, "s"):
			return "", true
		case strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+"):
		default:
			return a, s.isStdin(a)
		}
	}
	return "", true
}

func (s *shellScope) assign(words []string) {
	for _, w := range words {
		if m := shellAssignment.FindStringSubmatch(w); m != nil {
			s.vars[m[1]] = s.expand(m[2])
		}
	}
}

func (s *shellScope) chdir(args []string) {
	for len(args) > 0 && len(args[0]) > 1 && args[0][0] == '-' {
		args = args[1:]
	}
	switch {
	case len(args) == 0:
		s.cwd = "$HOME"
	case args[0] == "-": // the previous directory is not tracked
		s.cwd = ""
	default:
		s.cwd = s.resolve(args[0])
	}
}

// writtenFiles returns the arguments that name files program name writes.
// isMemory reports whether an argument names a memory path.
func writtenFiles(name string, args []string, isMemory func(string) bool) []string {
	switch name {
	case "tee", "touch", "truncate":
		return operands(args)
	case "dd":
		for _, a := range args {
			if of, ok := strings.CutPrefix(a, "of="); ok {
				return []string{of}
			}
		}
	case "cp", "mv", "ln", "install", "rsync":
		return copyTargets(name, args, isMemory)
	case "sed", "gsed", "perl":
		return inPlaceFiles(name, args)
	}
	return nil
}

// copyTargets returns the destination of a copy or move and, because the
// destination may be a directory, the path each source would take inside it.
func copyTargets(name string, args []string, isMemory func(string) bool) []string {
	var dest string
	var sources []string
	intoDir := false     // -t says the destination is a directory
	if name != "rsync" { // rsync -t preserves times
		for i, a := range args {
			if a == "--" { // what follows are operands, whatever they look like
				break
			}
			if dir, ok := strings.CutPrefix(a, "--target-directory="); ok {
				dest, sources, intoDir = dir, operands(args), true
				break
			}
			short := len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.HasSuffix(a, "t")
			if (short || a == "--target-directory") && i+1 < len(args) {
				dest, intoDir = args[i+1], true
				sources = operands(append(append([]string{}, args[:i]...), args[i+2:]...))
				break
			}
		}
	}
	if dest == "" {
		ops := operands(args)
		if len(ops) < 2 {
			return nil
		}
		dest, sources = ops[len(ops)-1], ops[:len(ops)-1]
	}
	targets := []string{dest}
	// A memory path given a sibling's name is a rename, as in "mv memory
	// memory.bak", not a copy into that sibling. Any other source is checked,
	// since the sibling may be an existing directory it lands in.
	if !intoDir && len(sources) == 1 && !strings.HasSuffix(dest, "/") && isMemory(sources[0]) &&
		path.Dir(path.Clean(sources[0])) == path.Dir(path.Clean(dest)) {
		return targets
	}
	for _, src := range sources {
		// rsync copies a directory's contents, not the directory, after "src/".
		if name == "rsync" && strings.HasSuffix(src, "/") {
			continue
		}
		targets = append(targets, dest+"/"+path.Base(src))
	}
	return targets
}

func operands(args []string) []string {
	var ops []string
	for i, a := range args {
		if a == "--" {
			return append(ops, args[i+1:]...)
		}
		if !strings.HasPrefix(a, "-") {
			ops = append(ops, a)
		}
	}
	return ops
}

// inPlaceFiles returns the files a sed or perl command edits in place, leaving
// out the script so a path it mentions is not taken for a file.
func inPlaceFiles(name string, args []string) []string {
	var files []string
	inPlace, scriptSeen := false, false
	// Flags whose value is the rest of the word, and those among them that
	// otherwise take the next word.
	valueFlags, separateValueFlags := "ef", "ef"
	if name == "perl" {
		valueFlags, separateValueFlags = "eEMmIFCDdVx", "eEMmI"
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--"):
			inPlace = inPlace || strings.HasPrefix(a, "--in-place")
			if strings.HasPrefix(a, "--expression") || strings.HasPrefix(a, "--file") {
				scriptSeen = true
				if !strings.Contains(a, "=") {
					i++
				}
			}
		case len(a) > 1 && a[0] == '-':
			// Read the cluster up to -i, whose backup suffix is the rest of
			// the word, or up to a flag that takes a value.
			for j := 1; j < len(a); j++ {
				if a[j] == 'i' {
					inPlace = true
					break
				}
				if strings.IndexByte(valueFlags, a[j]) < 0 {
					continue
				}
				script := a[j] == 'e' || a[j] == 'E' || (name != "perl" && a[j] == 'f')
				scriptSeen = scriptSeen || script
				if j == len(a)-1 && strings.IndexByte(separateValueFlags, a[j]) >= 0 {
					i++
				}
				break
			}
		case a == "": // the empty suffix of BSD sed -i ''
		case !scriptSeen:
			scriptSeen = true // the script, or perl's program file
		default:
			files = append(files, a)
		}
	}
	if !inPlace {
		return nil
	}
	return files
}
