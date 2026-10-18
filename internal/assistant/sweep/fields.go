package sweep

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Shape is one allowed value shape for a Fields entry. Everything that fails
// its key's shape is human-authored as far as the runner is concerned and is
// moved to External, where it is fenced.
type Shape int

const (
	ShapeInt Shape = iota + 1
	ShapeTime
	ShapeSHA
	ShapeJiraAccountID
	ShapeGitHubLogin
	ShapeBeadID
	ShapeEnum
)

const maxFieldLen = 64

var (
	shaRe           = regexp.MustCompile(`^[0-9a-f]{40}$`)
	jiraAccountRe   = regexp.MustCompile(`^(\d+:[0-9a-f-]{36}|[0-9a-f]{24})$`)
	githubLoginRe   = regexp.MustCompile(`^[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*$`)
	beadIDRe        = regexp.MustCompile(`^[a-z0-9]{1,8}-[a-z0-9.-]{1,40}$`)
	enumRe          = regexp.MustCompile(`^[a-z_]{1,32}$`)
	allowedShapeLen = map[Shape]int{ShapeGitHubLogin: 39}

	// Shapes of the parts of a Finding that print outside the fence.
	kindRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)             // Kind
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)            // Fields / External key names
	keyRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#@-]{0,127}$`) // Key (no whitespace, no "|")
)

const maxSuggestLen, maxURLLen = 200, 512

// suggestVocabulary is the closed word list a Suggest line may use besides
// the finding's own Key and its allowlisted Fields values. A port that
// needs a new word adds it here; provider text can never reach this line.
var suggestVocabulary = set(strings.Fields(`
a an and the to on onto for from with before after or why it its this
reply chase answer respond ask note status update decide reassign sling
onward close reason review re-request request re-ping re-approval
re-run rebase merge push fix tag release wave escalate ack convoy
blocker blocked hand off finish starting more waits add humans bots
list board move human reviewer approval approve ci check checks
draft stale open pull pr issue bead rig run start loop internal
config setting gt bd jira github assistant sweep
jira-move-to-board assistant.sweep.jira.humans assistant.sweep.jira.bots
assistant.sweep.github.humans`))

var tokenTrim = "`()[]{}.,:;\"'"

var spaceRe = regexp.MustCompile(`\s`)

// ValidShape reports whether v matches shape (and its length cap).
func ValidShape(shape Shape, v string) bool {
	if v == "" || len(v) > maxFieldLen {
		return false
	}
	if n, ok := allowedShapeLen[shape]; ok && len(v) > n {
		return false
	}
	switch shape {
	case ShapeInt:
		_, err := strconv.ParseInt(v, 10, 64)
		return err == nil
	case ShapeTime:
		_, err := time.Parse(time.RFC3339, v)
		return err == nil
	case ShapeSHA:
		return shaRe.MatchString(v)
	case ShapeJiraAccountID:
		return jiraAccountRe.MatchString(v)
	case ShapeGitHubLogin:
		return githubLoginRe.MatchString(v)
	case ShapeBeadID:
		return beadIDRe.MatchString(v)
	case ShapeEnum:
		return enumRe.MatchString(v)
	}
	return false
}

// enforceSchema makes a finding safe to print: every Fields entry that is
// undeclared or fails its shape moves into External, and so does any part
// that prints outside the fence (Kind, Key, URL, Suggest, key names) when it
// fails its own shape. Nothing is dropped: a moved value that would collide
// with an existing External entry lands under "<key>.field", and a value
// with an unprintable key name lands under "x<n>" with the name folded into
// the fenced text.
func enforceSchema(schema map[string]Shape, f *Finding) {
	for k, v := range f.Fields {
		if !nameRe.MatchString(k) {
			delete(f.Fields, k)
			f.addExternal(nextName(f), k+": "+v)
			continue
		}
		if shape, declared := schema[k]; declared && ValidShape(shape, v) {
			continue
		}
		delete(f.Fields, k)
		f.addExternal(k, v)
	}
	for k, v := range f.External {
		if !nameRe.MatchString(k) {
			delete(f.External, k)
			f.addExternal(nextName(f), k+": "+v)
		}
	}
	if !kindRe.MatchString(f.Kind) {
		f.addExternal("kind", f.Kind)
		f.Kind = "invalid-kind"
	}
	if !keyRe.MatchString(f.Key) {
		sum := sha256.Sum256([]byte(f.Key))
		f.addExternal("key", f.Key)
		f.Key = "invalid-key-" + hex.EncodeToString(sum[:6])
	}
	if f.URL != "" && !validURL(f.URL) {
		f.addExternal("url", f.URL)
		f.URL = ""
	}
	if f.Suggest != "" && !allowedSuggest(f) {
		f.addExternal("suggest", f.Suggest)
		f.Suggest = ""
	}
	switch f.Severity {
	case SeverityUrgent, SeverityActionable, SeverityInfo:
	case "":
		f.Severity = SeverityInfo
	default:
		f.addExternal("severity", f.Severity)
		f.Severity = SeverityInfo
	}
}

// allowedSuggest: single printable line whose every word is in the
// vocabulary, is the finding's Key, or is one of its (already validated)
// Fields values. Surrounding punctuation and backticks are ignored.
func allowedSuggest(f *Finding) bool {
	if !printable(f.Suggest, maxSuggestLen) {
		return false
	}
	for _, tok := range strings.Fields(f.Suggest) {
		tok = strings.Trim(tok, tokenTrim)
		if tok == "" || tok == f.Key || suggestVocabulary[tok] {
			continue
		}
		known := false
		for _, v := range f.Fields {
			if tok == v {
				known = true
				break
			}
		}
		if !known {
			return false
		}
	}
	return true
}

func set(items []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range items {
		if s = strings.TrimSpace(s); s != "" {
			m[s] = true
		}
	}
	return m
}

// addExternal stores v under k without overwriting an existing entry.
func (f *Finding) addExternal(k, v string) {
	if f.External == nil {
		f.External = map[string]string{}
	}
	for _, ok := f.External[k]; ok; _, ok = f.External[k] {
		k += ".field"
	}
	f.External[k] = v
}

func nextName(f *Finding) string {
	for i := 1; ; i++ {
		if _, ok := f.External[fmt.Sprintf("x%d", i)]; !ok {
			return fmt.Sprintf("x%d", i)
		}
	}
}

// printable: bounded, single line, no control characters.
func printable(s string, max int) bool {
	if len(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return false
		}
	}
	return true
}

func validURL(u string) bool {
	if !printable(u, maxURLLen) || spaceRe.MatchString(u) {
		return false
	}
	parsed, err := url.Parse(u)
	return err == nil && parsed.Scheme == "https" && parsed.Host != ""
}
