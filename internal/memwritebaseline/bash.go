// Copied from origin/main at 704895b2d, with only the package clause changed.
// Do not edit: see doc.go.

package memwritebaseline

import (
	"path"
	"regexp"
	"strings"
)

// This file finds the files a Bash command writes, so the memory-write guard
// can judge them. It follows the ordinary ways an agent writes a file from the
// shell: redirects, tee, in-place editors, and the copy, move and create
// commands, including through cd and simple variables. It is not a shell: a
// path computed at run time, or a write made by another program (a Python or
// Node script, say), is not seen.

// shellToken is one word, after quote removal, or one operator.
type shellToken struct {
	text string
	op   bool
}

// Operators lexShell emits besides the command separators. Every redirect that
// writes a file is opWrite; every other redirect is opRedirect. Both take the
// following word as their target.
const (
	opWrite    = ">"
	opRedirect = "<"
)

// lexShell splits a command line into tokens. Comments and heredoc bodies are
// dropped, so text that merely mentions a write is not read as one.
func lexShell(s string) []shellToken {
	var (
		toks     []shellToken
		word     strings.Builder
		inWord   bool
		quoted   bool
		heredocs []string // delimiters whose bodies start at the next newline
		wantTag  bool     // the next word is a heredoc delimiter
	)
	flush := func() {
		if !inWord {
			return
		}
		if wantTag {
			heredocs = append(heredocs, word.String())
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

	for i := 0; i < len(s); i++ {
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
				if s[i] == '\\' && strings.IndexByte("\"\\$`\n", at(i+1)) >= 0 {
					i++
				}
				if i < len(s) {
					word.WriteByte(s[i])
				}
			}
			inWord, quoted = true, true
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
			for _, tag := range heredocs {
				for i+1 < len(s) {
					line := s[i+1:]
					if end := strings.IndexByte(line, '\n'); end >= 0 {
						line = line[:end]
					}
					i += len(line) + 1
					if strings.TrimLeft(line, "\t") == tag {
						break
					}
				}
			}
			heredocs = nil
		case ';', '(', ')', '`':
			emit(string(c))
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
				case at(i+1) == '<' && at(i+2) == '<': // here-string
					i += 2
					emit(opRedirect)
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
	return toks
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
	var (
		outer []shellScope // scopes saved at each open parenthesis
		words []string     // the current command's words
		redir []string     // files the current command redirects into
		op    string       // redirect operator awaiting its target
	)
	end := func() (string, bool) {
		defer func() { words, redir, op = nil, nil, "" }()
		// The shell opens redirects before the command runs.
		for _, r := range redir {
			if p := scope.resolve(r); isMemoryTarget(p) {
				return p, true
			}
		}
		return scope.run(words)
	}

	for _, t := range lexShell(command) {
		switch {
		case !t.op:
			switch op {
			case opWrite:
				redir = append(redir, t.text)
			case opRedirect:
			default:
				words = append(words, t.text)
			}
			op = ""
		case t.text == opWrite || t.text == opRedirect:
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

// run applies one simple command to the scope and reports a memory write.
func (s *shellScope) run(words []string) (string, bool) {
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
	case "eval":
		return scanShellWrites(strings.Join(args, " "), s.clone())
	default:
		for _, target := range writtenFiles(name, args) {
			if p := s.resolve(target); isMemoryTarget(p) {
				return p, true
			}
		}
	}
	return "", false
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
	case "tee", "touch", "truncate":
		return operands(args)
	case "dd":
		for _, a := range args {
			if of, ok := strings.CutPrefix(a, "of="); ok {
				return []string{of}
			}
		}
	case "cp", "mv", "ln", "install", "rsync":
		if name != "rsync" { // rsync -t preserves times
			for i, a := range args {
				if dir, ok := strings.CutPrefix(a, "--target-directory="); ok {
					return []string{dir}
				}
				if (a == "-t" || a == "--target-directory") && i+1 < len(args) {
					return []string{args[i+1]}
				}
			}
		}
		if ops := operands(args); len(ops) > 1 {
			return ops[len(ops)-1:]
		}
	case "sed", "gsed", "perl":
		return inPlaceFiles(name, args)
	}
	return nil
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
			flags := a[1:]
			// -i takes any rest of the word as the backup suffix.
			if at := strings.IndexByte(flags, 'i'); at >= 0 {
				inPlace = true
				flags = flags[:at+1]
			}
			// -e (and perl's -E) and sed's -f take the script as the next word.
			last := flags[len(flags)-1]
			if last == 'e' || (name == "perl" && last == 'E') || (name != "perl" && last == 'f') {
				scriptSeen = true
				i++
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
