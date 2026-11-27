package cmd

import (
	"errors"
	"strings"
)

// A small shell reader for the env-dump guard. Unlike lexShell, which the
// memory-write guard uses to find written paths, it keeps what this guard
// judges: which variables each word expands, the commands substituted into it,
// pipeline boundaries and heredoc bodies.

// envWord is one word after quote removal.
type envWord struct {
	text string
	refs []string // variables whose values the shell expands into the word
	subs []string // scripts of command and process substitutions in the word
}

type envRedirect struct {
	fd     string // descriptor written before the operator, if any
	op     string // ">", ">>", ">&", "<", "<<<"
	target envWord
}

type envHeredoc struct {
	tag    string
	body   envWord // refs and subs stay empty when the tag is quoted
	quoted bool
}

type envCommand struct {
	words     []envWord
	redirects []envRedirect
	heredocs  []*envHeredoc
}

// envPipeline is the commands joined by | or |&.
type envPipeline []envCommand

var errUnterminated = errors.New("unterminated quote or substitution")

type envLexer struct {
	s   string
	pos int

	pipelines []envPipeline
	pipeline  envPipeline
	command   envCommand

	word       envWord
	text       strings.Builder
	inWord     bool
	quoted     bool
	redirect   *envRedirect // awaiting its target
	heredocTag bool         // the next word is a heredoc delimiter
	pending    []*envHeredoc
}

func (l *envLexer) at(i int) byte {
	if i < len(l.s) {
		return l.s[i]
	}
	return 0
}

func (l *envLexer) add(s string) {
	l.text.WriteString(s)
	l.inWord = true
}

func (l *envLexer) endWord() {
	if !l.inWord {
		return
	}
	w := l.word
	w.text = l.text.String()
	switch {
	case l.heredocTag:
		h := &envHeredoc{tag: w.text, quoted: l.quoted}
		l.command.heredocs = append(l.command.heredocs, h)
		l.pending = append(l.pending, h)
		l.heredocTag = false
	case l.redirect != nil:
		l.redirect.target = w
		l.command.redirects = append(l.command.redirects, *l.redirect)
		l.redirect = nil
	default:
		l.command.words = append(l.command.words, w)
	}
	l.word = envWord{}
	l.text.Reset()
	l.inWord, l.quoted = false, false
}

func (l *envLexer) endCommand() {
	l.endWord()
	c := l.command
	if len(c.words) > 0 || len(c.redirects) > 0 || len(c.heredocs) > 0 {
		l.pipeline = append(l.pipeline, c)
	}
	l.command = envCommand{}
	l.redirect = nil
}

func (l *envLexer) endPipeline() {
	l.endCommand()
	if len(l.pipeline) > 0 {
		l.pipelines = append(l.pipelines, l.pipeline)
	}
	l.pipeline = nil
}

// lexEnvScript splits a script into pipelines.
func lexEnvScript(script string) ([]envPipeline, error) {
	l := &envLexer{s: script}
	for l.pos < len(l.s) {
		if err := l.step(); err != nil {
			return nil, err
		}
	}
	l.endPipeline()
	return l.pipelines, nil
}

func (l *envLexer) step() error {
	c := l.s[l.pos]
	next := l.at(l.pos + 1)
	switch c {
	case '\\':
		if next != 0 && next != '\n' {
			l.add(string(next))
			l.quoted = true
		}
		l.pos += 2
	case '\'':
		end := strings.IndexByte(l.s[l.pos+1:], '\'')
		if end < 0 {
			return errUnterminated
		}
		l.add(l.s[l.pos+1 : l.pos+1+end])
		l.quoted = true
		l.pos += end + 2
	case '"':
		l.add("")
		l.quoted = true
		return l.doubleQuoted()
	case '$':
		return l.dollar(false)
	case '`':
		return l.backtick()
	case ' ', '\t', '\r':
		l.endWord()
		l.pos++
	case '#':
		if l.inWord {
			l.add("#")
			l.pos++
			return nil
		}
		for l.pos < len(l.s) && l.s[l.pos] != '\n' {
			l.pos++
		}
	case '\n':
		l.endPipeline()
		l.pos++
		return l.heredocBodies()
	case '(':
		if l.inWord && strings.HasSuffix(l.text.String(), "=") {
			return l.array()
		}
		l.endPipeline()
		l.pos++
	case ';', ')':
		l.endPipeline()
		l.pos++
	case '|':
		switch next {
		case '|':
			l.endPipeline()
			l.pos += 2
		case '&':
			l.endCommand()
			l.pos += 2
		default:
			l.endCommand()
			l.pos++
		}
	case '&':
		switch next {
		case '>':
			l.endWord()
			l.pos++ // the > that follows is the redirect
		case '&':
			l.endPipeline()
			l.pos += 2
		default:
			l.endPipeline()
			l.pos++
		}
	case '<', '>':
		return l.redirection()
	default:
		l.add(string([]byte{c}))
		l.pos++
	}
	return nil
}

func (l *envLexer) redirection() error {
	c := l.s[l.pos]
	if l.at(l.pos+1) == '(' { // process substitution
		return l.substitution(l.pos + 2)
	}
	fd := ""
	if l.inWord && !l.quoted && strings.Trim(l.text.String(), "0123456789") == "" {
		fd = l.text.String()
		l.text.Reset()
		l.word = envWord{}
		l.inWord = false
	}
	l.endWord()
	op := string(c)
	l.pos++
	if c == '<' {
		switch {
		case l.at(l.pos) == '<' && l.at(l.pos+1) == '<':
			op = "<<<"
			l.pos += 2
		case l.at(l.pos) == '<':
			l.pos++
			if l.at(l.pos) == '-' {
				l.pos++
			}
			l.heredocTag = true
			return nil
		case l.at(l.pos) == '>', l.at(l.pos) == '&':
			l.pos++
		}
	} else {
		switch l.at(l.pos) {
		case '>':
			op = ">>"
			l.pos++
		case '&':
			op = ">&"
			l.pos++
		}
		if l.at(l.pos) == '|' || l.at(l.pos) == '!' {
			l.pos++
		}
	}
	l.redirect = &envRedirect{fd: fd, op: op}
	return nil
}

func (l *envLexer) doubleQuoted() error {
	l.pos++
	for l.pos < len(l.s) {
		switch c := l.s[l.pos]; c {
		case '"':
			l.pos++
			return nil
		case '\\':
			next := l.at(l.pos + 1)
			if next != 0 && strings.IndexByte("\"\\$`\n", next) >= 0 {
				if next != '\n' {
					l.add(string(next))
				}
				l.pos += 2
				continue
			}
			l.add(`\`)
			l.pos++
		case '$':
			if err := l.dollar(true); err != nil {
				return err
			}
		case '`':
			if err := l.backtick(); err != nil {
				return err
			}
		default:
			l.add(string([]byte{c}))
			l.pos++
		}
	}
	return errUnterminated
}

func (l *envLexer) backtick() error {
	end := l.pos + 1
	for end < len(l.s) && l.s[end] != '`' {
		if l.s[end] == '\\' {
			end++
		}
		end++
	}
	if end >= len(l.s) {
		return errUnterminated
	}
	l.word.subs = append(l.word.subs, l.s[l.pos+1:end])
	l.add(l.s[l.pos : end+1])
	l.pos = end + 1
	return nil
}

// substitution records the script that starts at body and ends at the matching
// parenthesis. The script is lexed to find that parenthesis, so one in a
// quote, comment or heredoc body does not count.
func (l *envLexer) substitution(body int) error {
	inner := &envLexer{s: l.s, pos: body}
	for depth := 0; inner.pos < len(inner.s); {
		switch inner.s[inner.pos] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				end := inner.pos
				l.word.subs = append(l.word.subs, l.s[body:end])
				l.add(l.s[l.pos : end+1])
				l.pos = end + 1
				return nil
			}
			depth--
		}
		if err := inner.step(); err != nil {
			return err
		}
	}
	return errUnterminated
}

// array adds the (...) of an array assignment to the word; its elements are
// values, not commands.
func (l *envLexer) array() error {
	end, err := matchingClose(l.s, l.pos+1, '(', ')')
	if err != nil {
		return err
	}
	text := l.s[l.pos : end+1]
	l.add(text)
	l.pos = end + 1
	return l.expansions(text)
}

// matchingClose returns the index of the closer that ends the span starting at
// from, skipping quoted text.
func matchingClose(s string, from int, opener, closer byte) (int, error) {
	depth := 1
	var quote byte
	for i := from; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && quote != '\'':
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == opener:
			depth++
		case c == closer:
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, errUnterminated
}

func (l *envLexer) dollar(inDouble bool) error {
	switch next := l.at(l.pos + 1); {
	case next == '(' && l.at(l.pos+2) == '(': // arithmetic
		end, err := matchingClose(l.s, l.pos+2, '(', ')')
		if err != nil {
			return err
		}
		l.add(l.s[l.pos : end+1])
		l.pos = end + 1
	case next == '(':
		return l.substitution(l.pos + 2)
	case next == '{':
		end, err := matchingClose(l.s, l.pos+2, '{', '}')
		if err != nil {
			return err
		}
		if err := l.braced(l.s[l.pos+2 : end]); err != nil {
			return err
		}
		l.add(l.s[l.pos : end+1])
		l.pos = end + 1
	case next == '\'' && !inDouble: // $'...' is literal text
		l.pos++
	default:
		end := l.pos + 1
		for end < len(l.s) && isShellNameByte(l.s[end]) {
			end++
		}
		if end > l.pos+1 {
			l.word.refs = append(l.word.refs, l.s[l.pos+1:end])
		}
		l.add(l.s[l.pos:end])
		l.pos = end
	}
	return nil
}

func isShellNameByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// braced records what ${...} expands to the value of. Length (#), "is it set"
// (+ and :+) and name listings (!prefix*) do not expand a value.
func (l *envLexer) braced(inner string) error {
	if strings.HasPrefix(inner, "(") { // zsh expansion flags
		if end := strings.IndexByte(inner, ')'); end >= 0 {
			inner = inner[end+1:]
		}
	}
	if strings.HasPrefix(inner, "#") || strings.HasPrefix(inner, "+") {
		return nil
	}
	indirect := strings.HasPrefix(inner, "!")
	inner = strings.TrimLeft(inner, "!=~^")
	end := 0
	for end < len(inner) && isShellNameByte(inner[end]) {
		end++
	}
	name, rest := inner[:end], inner[end:]
	if indirect && (strings.HasSuffix(rest, "*") || strings.HasSuffix(rest, "@")) {
		return nil
	}
	if name != "" && !strings.HasPrefix(rest, "+") && !strings.HasPrefix(rest, ":+") {
		l.word.refs = append(l.word.refs, name)
	}
	// The default or replacement text can expand further values.
	return l.expansions(rest)
}

// expansions records the refs and subs in text that is not split into words.
func (l *envLexer) expansions(text string) error {
	inner := &envLexer{s: text}
	for inner.pos < len(inner.s) {
		var err error
		switch inner.s[inner.pos] {
		case '\\':
			inner.pos += 2
		case '$':
			err = inner.dollar(true)
		case '`':
			err = inner.backtick()
		default:
			inner.pos++
		}
		if err != nil {
			return err
		}
	}
	l.word.refs = append(l.word.refs, inner.word.refs...)
	l.word.subs = append(l.word.subs, inner.word.subs...)
	return nil
}

// heredocBodies consumes the bodies of the heredocs opened on the line that
// just ended.
func (l *envLexer) heredocBodies() error {
	for _, h := range l.pending {
		var body []string
		for l.pos < len(l.s) {
			line := l.s[l.pos:]
			if end := strings.IndexByte(line, '\n'); end >= 0 {
				line = line[:end]
			}
			l.pos += len(line) + 1
			if strings.TrimLeft(line, "\t") == h.tag {
				break
			}
			body = append(body, line)
		}
		h.body.text = strings.Join(body, "\n")
		if h.quoted {
			continue
		}
		expanded := &envLexer{}
		if err := expanded.expansions(h.body.text); err != nil {
			return err
		}
		h.body.refs, h.body.subs = expanded.word.refs, expanded.word.subs
	}
	l.pending = nil
	return nil
}
