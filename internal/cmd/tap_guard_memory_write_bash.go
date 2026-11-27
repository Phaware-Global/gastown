package cmd

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

// This file finds the files a Bash command writes, so the memory-write guard
// can judge them. It follows the ordinary ways an agent writes a file from the
// shell: redirects, tee, in-place editors, and the copy, move and create
// commands, including through cd, simple variables, command substitutions and
// scripts fed to a shell. It is not a shell: a path computed at run time, or a
// write made by another program (a Python or Node script, say), is not seen.

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

// lexShell splits a command line into tokens. Comments are dropped and heredoc
// bodies are set aside, so text that merely mentions a write is not read as one.
func lexShell(s string) []shellToken {
	toks, _ := lexCommands(s, false)
	return toks
}

type heredoc struct {
	tag    string
	quoted bool // a quoted delimiter keeps the body from being expanded
	body   *string
}

// lexCommands lexes s and returns the tokens and how much of s they cover. When
// nested, s follows a "$(" and lexing stops at the ")" that closes it.
func lexCommands(s string, nested bool) ([]shellToken, int) {
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
		subs, last, ok := lexSubstitution(s, i, inQuotes)
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
			inWord, quoted = true, true
		case '$', '`':
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
				body, last, ok := heredocBody(s, i, h.tag, nested)
				if !ok {
					// No delimiter: the "<<" was most likely a shift, so the
					// lines after it are still commands.
					break
				}
				*h.body, i = body, last
				if !h.quoted {
					toks = append(toks, expansions(body)...)
				}
			}
			heredocs = nil
		case '(':
			if end := arithmeticEnd(s, i); end >= 0 {
				// In (( )) the operators < > and << are arithmetic.
				flush()
				toks = append(toks, expansions(s[i+2:end-1])...)
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
	return toks, i
}

// lexSubstitution lexes the $(...), $((...)) or `...` that starts at s[i]. It
// returns the commands run as sub tokens and the index of the closing byte.
func lexSubstitution(s string, i int, inQuotes bool) ([]shellToken, int, bool) {
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
		return subshell(lexShell(body.String())), j, true
	case !strings.HasPrefix(s[i:], "$("):
		return nil, i, false
	}
	if end := arithmeticEnd(s, i+1); end >= 0 {
		return expansions(s[i+3 : end-1]), end, true
	}
	toks, n := lexCommands(s[i+2:], true)
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
func expansions(text string) []shellToken {
	var toks []shellToken
	for i := 0; i < len(text); i++ {
		if text[i] == '\\' {
			i++
			continue
		}
		if subs, last, ok := lexSubstitution(text, i, false); ok {
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
// holding tag alone. It returns the body and the index of the last byte used.
func heredocBody(s string, i int, tag string, nested bool) (string, int, bool) {
	for start := i + 1; start < len(s); {
		line := s[start:]
		if end := strings.IndexByte(line, '\n'); end >= 0 {
			line = line[:end]
		}
		trimmed := strings.TrimLeft(line, "\t")
		if trimmed == tag {
			return s[i+1 : start], start + len(line), true
		}
		// Inside $( ) the ")" may share the delimiter's line.
		if nested && strings.HasPrefix(trimmed, tag+")") {
			return s[i+1 : start], start + len(line) - len(trimmed) + len(tag) - 1, true
		}
		start += len(line) + 1
	}
	return "", i, false
}

// shellScope is the state a command inherits from the ones before it.
type shellScope struct {
	cwd  string
	vars map[string]string
}

func (s shellScope) clone() shellScope {
	vars := make(map[string]string, len(s.vars))
	for k, v := range s.vars {
		vars[k] = v
	}
	return shellScope{cwd: s.cwd, vars: vars}
}

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

// bashMemoryWrite returns the first Claude Code memory path that command,
// started in cwd, writes.
func bashMemoryWrite(command, cwd string) (string, bool) {
	return scanShellWrites(command, shellScope{cwd: cwd, vars: map[string]string{}})
}

func scanShellWrites(command string, scope shellScope) (string, bool) {
	return scanShellTokens(lexShell(command), scope)
}

func scanShellTokens(toks []shellToken, scope shellScope) (string, bool) {
	var (
		outer []shellScope // scopes saved at each open parenthesis
		words []string     // the current command's words
		redir []string     // files the current command redirects into
		stdin *string      // text the current command is fed
		op    string       // redirect operator awaiting its target
	)
	end := func() (string, bool) {
		defer func() { words, redir, stdin, op = nil, nil, nil, "" }()
		// The shell opens redirects before the command runs.
		for _, r := range redir {
			if p := scope.resolve(r); isMemoryTarget(p) {
				return p, true
			}
		}
		return scope.run(words, stdin)
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
			default:
				words = append(words, t.text)
			}
			op = ""
		case t.text == opWrite || t.text == opRedirect || t.text == opHereString:
			op = t.text
		default:
			if p, ok := end(); ok {
				return p, true
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

// run applies one simple command, fed stdin if non-nil, to the scope and reports
// a memory write.
func (s *shellScope) run(words []string, stdin *string) (string, bool) {
	name, args := s.commandName(words)
	switch name {
	case "": // assignments with no command set variables
		s.assign(words)
	case "export", "local", "declare", "typeset", "readonly":
		s.assign(args)
	case "cd", "pushd":
		s.chdir(args)
	case "bash", "sh", "zsh", "dash", "ksh":
		for i := 0; i+1 < len(args); i++ {
			if f := args[i]; strings.HasPrefix(f, "-") && !strings.HasPrefix(f, "--") && strings.HasSuffix(f, "c") {
				return scanShellWrites(args[i+1], s.clone())
			}
		}
		if stdin != nil && readsScriptFromStdin(args) {
			return scanShellWrites(*stdin, s.clone())
		}
	case "eval":
		return scanShellWrites(strings.Join(args, " "), s.clone())
	default:
		expanded := make([]string, len(args))
		for i, a := range args {
			expanded[i] = s.expand(a)
		}
		for _, target := range writtenFiles(name, expanded) {
			if p := s.resolve(target); isMemoryTarget(p) {
				return p, true
			}
		}
	}
	return "", false
}

// readsScriptFromStdin reports whether a shell given args, which hold no -c,
// runs its standard input rather than a script file.
func readsScriptFromStdin(args []string) bool {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-o" || a == "+o" || a == "-O" || a == "+O" || a == "--rcfile" || a == "--init-file":
			i++
		case a == "-" || strings.HasPrefix(a, "--") && a != "--":
		case strings.HasPrefix(a, "-") && strings.Contains(a, "s"):
			return true
		case strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+"):
		default:
			return false
		}
	}
	return true
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
func writtenFiles(name string, args []string) []string {
	switch name {
	case "tee":
		return operands(args)
	case "touch": // -r, -d, -t and -A take a value that is not written
		return operands(dropFlagValues(args, "rdtA", "--reference", "--date"))
	case "truncate":
		return operands(dropFlagValues(args, "rs", "--reference", "--size"))
	case "dd":
		for _, a := range args {
			if of, ok := strings.CutPrefix(a, "of="); ok {
				return []string{of}
			}
		}
	case "cp", "mv", "ln", "install", "rsync":
		return copyTargets(name, args)
	case "sed", "gsed", "perl":
		return inPlaceFiles(name, args)
	}
	return nil
}

// copyTargets returns the destination of a copy or move and, because the
// destination may be a directory, the path each source would take inside it.
func copyTargets(name string, args []string) []string {
	var dest string
	var sources []string
	if name != "rsync" { // rsync -t preserves times
		for i, a := range args {
			if dir, ok := strings.CutPrefix(a, "--target-directory="); ok {
				dest, sources = dir, operands(args)
				break
			}
			short := len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.HasSuffix(a, "t")
			if (short || a == "--target-directory") && i+1 < len(args) {
				dest = args[i+1]
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
	for _, src := range sources {
		// rsync copies a directory's contents, not the directory, after "src/".
		if name == "rsync" && strings.HasSuffix(src, "/") {
			continue
		}
		targets = append(targets, dest+"/"+path.Base(src))
	}
	return targets
}

// dropFlagValues removes the separate-word values of the short flags in letters
// and of the long flags named.
func dropFlagValues(args []string, letters string, long ...string) []string {
	var kept []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return append(kept, args[i:]...)
		}
		kept = append(kept, a)
		switch {
		case slices.Contains(long, a):
			i++
		case len(a) > 1 && a[0] == '-' && a[1] != '-':
			// The rest of the word after a value flag is its value.
			if at := strings.IndexAny(a[1:], letters); at == len(a)-2 {
				i++
			}
		}
	}
	return kept
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
