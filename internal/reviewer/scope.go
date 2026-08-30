package reviewer

import (
	"bufio"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Scope classifies a finding against the diff under review.
//
// The execution contract has always carried the rule — "raise findings only on
// lines this PR introduced or changed; pre-existing code is out of scope unless
// the diff changed its behavior" — but nothing enforced it. normalizeFinding
// checked that a path was non-empty and a line positive, and Consolidate had no
// concept of the diff at all, so the one rule governing scope was the one rule
// with no mechanical backing.
type Scope string

const (
	// ScopeUnknown means no manifest was supplied, so scope could not be
	// determined. Nothing is demoted: absent evidence is not evidence of a
	// violation, and a reviewer running without a diff must not silently
	// downgrade every finding it makes.
	ScopeUnknown Scope = ""
	// ScopeIn means the finding anchors to a line this diff changed and asks
	// for no work outside it.
	ScopeIn Scope = "in_scope"
	// ScopeAdjacent means the finding anchors to a file this diff touched, but
	// to a line it did not change. Worth saying; not worth blocking on.
	ScopeAdjacent Scope = "adjacent"
	// ScopeOut means the finding anchors outside the diff, or demands work in
	// files the diff never touched.
	ScopeOut Scope = "out_of_scope"
)

// lineRange is an inclusive run of changed lines in a post-image file.
type lineRange struct{ start, end int }

// DiffManifest records which lines of which files a diff actually changed. A
// nil manifest means "unknown" and classifies everything as ScopeUnknown.
type DiffManifest map[string][]lineRange

// hunkHeader matches a unified-diff hunk header's post-image span, e.g.
// "@@ -12,3 +14,6 @@ func foo() {". The count is optional (git omits ",1").
var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// ParseDiffManifest builds a manifest from unified diff text, as produced by
// `git diff -U0 <base>..<head>`.
//
// Zero context (-U0) is what makes the result meaningful: with the default three
// lines of context, every hunk header claims six more lines than the diff
// actually changed, and findings on untouched neighboring code would classify
// as in-scope.
//
// A pure parser rather than a git call so the classification is unit-testable
// without a repository; the caller runs git and passes the text in.
func ParseDiffManifest(diff string) DiffManifest {
	m := DiffManifest{}
	var current string
	sc := bufio.NewScanner(strings.NewReader(diff))
	// Diff lines can be long (minified files, lock files); the default 64kB
	// token limit would truncate the scan and silently lose later files.
	sc.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "+++ "):
			// "+++ b/path/to/file" — the post-image path. "/dev/null" marks a
			// deletion, which has no post-image lines to anchor a finding to.
			p := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
			if p == "/dev/null" {
				current = ""
				continue
			}
			current = normalizeDiffPath(p)
			if current != "" {
				// Register the file even if a later hunk never lands, so a
				// pure-rename or mode change still counts as touched.
				if _, ok := m[current]; !ok {
					m[current] = nil
				}
			}
		case strings.HasPrefix(line, "@@"):
			if current == "" {
				continue
			}
			match := hunkHeader.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			start, err := strconv.Atoi(match[1])
			if err != nil {
				continue
			}
			count := 1
			if match[2] != "" {
				if c, cerr := strconv.Atoi(match[2]); cerr == nil {
					count = c
				}
			}
			if count <= 0 {
				// A pure deletion (+N,0) changes no post-image line. Anchor a
				// zero-width marker at the deletion point so a finding about
				// the removal still classifies as in-scope rather than
				// out-of-scope.
				m[current] = append(m[current], lineRange{start: start, end: start})
				continue
			}
			m[current] = append(m[current], lineRange{start: start, end: start + count - 1})
		}
	}
	return m
}

// normalizeDiffPath strips the a/ or b/ prefix git puts on diff paths and
// removes any quoting, yielding a repo-relative path comparable to the one a
// finding carries.
func normalizeDiffPath(p string) string {
	p = strings.TrimSpace(p)
	// git quotes paths containing unusual bytes; drop the quotes so the path
	// compares equal to the unquoted form a finding uses.
	if len(p) >= 2 && strings.HasPrefix(p, `"`) && strings.HasSuffix(p, `"`) {
		if unq, err := strconv.Unquote(p); err == nil {
			p = unq
		} else {
			p = strings.Trim(p, `"`)
		}
	}
	// A tab separates the path from git's trailing metadata on some lines.
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	for _, prefix := range []string{"a/", "b/"} {
		if strings.HasPrefix(p, prefix) {
			return strings.TrimPrefix(p, prefix)
		}
	}
	return p
}

// Touches reports whether the diff changed this file at all.
func (m DiffManifest) Touches(p string) bool {
	if m == nil {
		return false
	}
	_, ok := m[path.Clean(strings.TrimSpace(p))]
	return ok
}

// ChangedAt reports whether the diff changed this specific line of this file.
func (m DiffManifest) ChangedAt(p string, line int) bool {
	if m == nil {
		return false
	}
	for _, r := range m[path.Clean(strings.TrimSpace(p))] {
		if line >= r.start && line <= r.end {
			return true
		}
	}
	return false
}

// Classify places a finding relative to the diff.
//
// The anchor alone is not enough, and that is the whole point. A finding can
// anchor to a changed line while every edit it demands lives in files the PR
// never touched — an in-scope anchor laundering an out-of-scope demand. It
// passes every existing check and becomes a merge-blocking thread. Observed on
// graphql-api PR #112, where a HIGH anchored on a changed file and then stated
// in its own body that two of the three files it wanted edited were "write and
// scheduling paths the PR never mentions" and that the spec it wanted extended
// "is not in the diff".
//
// So the classification is the WEAKEST of the anchor's scope and every
// remediation path's scope. A finding is only in-scope if acting on it is.
func (m DiffManifest) Classify(f Finding) Scope {
	if m == nil {
		return ScopeUnknown
	}

	result := ScopeIn
	switch {
	case !m.Touches(f.Path):
		result = ScopeOut
	case !m.ChangedAt(f.Path, f.Line):
		result = ScopeAdjacent
	}

	// RemediationPaths is optional: a pass that omits it is classified on its
	// anchor alone. When present, any path outside the diff caps the finding at
	// out-of-scope no matter how good the anchor is.
	for _, rp := range f.RemediationPaths {
		rp = strings.TrimSpace(rp)
		if rp == "" {
			continue
		}
		if !m.Touches(rp) {
			return ScopeOut
		}
	}
	return result
}

// OutOfScopeNotice prefixes a demoted finding's body so a reader can see why it
// is not blocking without cross-referencing the diff.
//
// It takes the manifest because Classify returns ScopeOut on a DISJUNCTION —
// the anchor is outside, OR any single remediation path is. So a demoted
// finding can carry a mix of in-diff and out-of-diff paths, and naming all of
// them as "outside this PR's diff" would steer a fixer away from a change they
// could land here. Two shapes made that wrong: a mixed path list, and an
// out-of-diff anchor whose remediation is entirely in-diff (where every named
// path is in fact touched and the anchor — the real reason — went unmentioned).
func OutOfScopeNotice(m DiffManifest, f Finding) string {
	var outside []string
	for _, rp := range f.RemediationPaths {
		if rp = strings.TrimSpace(rp); rp == "" {
			continue
		}
		if !m.Touches(rp) {
			outside = append(outside, rp)
		}
	}

	anchorOutside := !m.Touches(f.Path)

	switch {
	case len(outside) > 0 && anchorOutside:
		return fmt.Sprintf("Non-blocking: this anchors outside the lines this PR changed, "+
			"and acting on it requires changes outside the diff (%s). "+
			"Track it as follow-up work rather than blocking this merge.",
			strings.Join(outside, ", "))
	case len(outside) > 0:
		return fmt.Sprintf("Non-blocking: acting on this requires changes outside this PR's diff (%s). "+
			"Track it as follow-up work rather than blocking this merge.",
			strings.Join(outside, ", "))
	default:
		// Either the anchor is outside the diff, or it sits on an untouched
		// line of a touched file. Naming no paths is correct here: every
		// remediation path this finding declared is inside the diff.
		return "Non-blocking: this anchors outside the lines this PR changed. " +
			"Track it as follow-up work rather than blocking this merge."
	}
}
