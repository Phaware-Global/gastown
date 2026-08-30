// Package reviewer holds the rig-level Reviewer role's pure logic: parsing the
// findings payload that `gt reviewer post` consumes and rendering it into the
// GitHub review contract (priority-badged inline threads + a summary body) that
// the refinery's existing parsers already understand. Keeping this logic in its
// own package — free of git/gh/tmux side effects — makes the output contract
// unit-testable in isolation. (P23-2376.)
package reviewer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/steveyegge/gastown/internal/refinery"
)

// decodeStrictJSON decodes exactly one JSON value from data into v, rejecting
// unknown fields and any trailing non-whitespace content. The reviewer payloads
// (findings and per-perspective results) are strict machine contracts —
// "exactly one JSON object and nothing else" — so `<valid JSON>…garbage` must
// fail loudly rather than be silently accepted.
func decodeStrictJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("unexpected trailing content after the JSON value")
		}
		return fmt.Errorf("unexpected trailing content after the JSON value: %w", err)
	}
	return nil
}

// Finding is one review finding produced by a perspective pass.
type Finding struct {
	Path        string `json:"path"`                 // repo-relative file path
	Line        int    `json:"line"`                 // 1-based line in the changed file
	Priority    string `json:"priority"`             // "high" | "medium" | "low"
	Perspective string `json:"perspective"`          // lens that produced it (e.g. "adversarial")
	Title       string `json:"title"`                // one-line summary
	Body        string `json:"body"`                 // explanation, with codegraph evidence
	Suggestion  string `json:"suggestion,omitempty"` // concrete change or diff suggestion

	// RemediationPaths lists every repo-relative file that must be edited to
	// act on this finding. Optional, but it is what distinguishes a finding
	// this PR can fix from one that only points at it: a finding may anchor to
	// a changed line while every edit it asks for lives in files the diff never
	// touched. Consolidate classifies a finding by the weakest of its anchor
	// and these paths, so naming them honestly is how a pass keeps a real
	// concern from becoming a false merge blocker.
	RemediationPaths []string `json:"remediation_paths,omitempty"`

	// Scope is assigned by Consolidate against the diff manifest; it is not
	// read from the payload. Findings outside the diff are demoted to
	// non-blocking rather than dropped.
	Scope Scope `json:"scope,omitempty"`
}

// Findings is the full payload `gt reviewer post --findings <json>` consumes.
type Findings struct {
	// Summary is the structured review body: one verdict line per perspective
	// plus any out-of-scope opportunities. Required, and it follows a template —
	// see ReviewSummary, which also owns the content budgets and the markdown
	// rendering. The role contract forbids silence, so every perspective that
	// ran has a line here.
	Summary ReviewSummary `json:"summary"`
	// ReviewedSHA, when set, is appended to the summary so humans can see which
	// commit was reviewed even if upstream HEAD has since moved.
	ReviewedSHA string `json:"reviewed_sha,omitempty"`
	// Findings are the individual inline findings. May be empty (a clean review
	// still posts a summary).
	Findings []Finding `json:"findings"`
	// Disposition optionally ESCALATES the GitHub review event: "request_changes"
	// or "comment" (case-insensitive). "approve" is rejected — see
	// validDispositions. It is a floor, not an override: the submitted event is
	// the more blocking of this value and the severity-derived one, so it can
	// raise a verdict but never lower one (see ReviewEvent). When empty, the
	// event comes from finding severity alone.
	//
	// It exists so a pass can assert a blocking verdict it cannot anchor to a
	// diff line, which normalizeFinding's path+line requirement makes
	// inexpressible as a finding.
	Disposition string `json:"disposition,omitempty"`
}

// Length budgets for a single finding, in runes.
//
// The Reviewer had no budget at any layer — not in Finding, not in FormatBody,
// not in the execution contract, whose only volume guidance was a finding
// COUNT. Measured output on graphql-api averaged 3,102 characters per inline
// thread across 199 threads on one PR (617kB of review prose), peaking at
// 8,887. At that size a thread stops being a work item and becomes an essay
// the fixer must mine for the actual request.
//
// These are hard limits, enforced where the payload is parsed, for the same
// reason decodeStrictJSON rejects unknown fields: the findings payload is a
// strict machine contract, and silently truncating a body would cut a sentence
// — and possibly the actual instruction — mid-word. A rejected payload names
// the offending field and its size so the pass can trim and re-emit.
//
// The budgets are deliberately generous against what good findings need: a
// title is one line, a body is the failure scenario plus its evidence, and a
// suggestion is the change to make. Anything longer is usually a second finding
// wearing the first one's anchor.
const (
	MaxTitleLen      = 120
	MaxBodyLen       = 1200
	MaxSuggestionLen = 800
)

// validPriorities is the closed set of priorities the badge/parser pair models.
var validPriorities = map[string]bool{"high": true, "medium": true, "low": true}

// validDispositions maps the findings-payload disposition (lowercased) to the
// GitHub review event it selects. The closed set is enforced at the contract
// boundary: ParseFindings rejects any non-empty, unrecognized disposition, so a
// typo fails loudly rather than silently degrading a blocking verdict.
//
// "approve" is deliberately absent. Disposition exists to ESCALATE past the
// severity tally — to express a blocking objection that cannot be anchored to a
// diff line — and an approving disposition can only ever be a no-op (it already
// agrees with the tally) or a de-escalation. Excluding it makes the closed set
// identical to the one ParsePerspectiveResult enforces, so the two entry points
// describe one contract instead of disagreeing, and matches what the help text,
// the payload schema, and the role template all state.
var validDispositions = map[string]string{
	"request_changes": "REQUEST_CHANGES",
	"comment":         "COMMENT",
}

// ReviewEvent returns the GitHub review disposition for these findings:
// "APPROVE", "REQUEST_CHANGES", or "COMMENT". The result is the MORE BLOCKING
// of an explicit Disposition and the severity-derived event — a disposition can
// only escalate, never de-escalate. Severity derivation uses the highest
// priority present:
//
//	high       → REQUEST_CHANGES (blocking; must be addressed)
//	medium/low/none → APPROVE    (worth fixing, but not a merge gate)
//
// COMMENT is no longer derivable from severity. It remains reachable only as an
// explicit Disposition, and the contract narrows what it means: a pass that cut
// itself short sets "comment" so a clean tally does not read as a finished
// endorsement (see the execution contract's time budget). An unanchorable
// concern that must be answered before the PR merges is not a comment — it is
// "request_changes", because that is what "address this before merging" means.
//
// Medium findings used to derive COMMENT, which meant a PR carrying a few
// medium nits was never approved and never blocked: it sat in a non-committal
// state that neither the fix loop nor a human could act on, while the threads
// themselves already imposed the work. Severity now answers one question — does
// this block the merge? — and only a high finding says yes.
//
// The in-town Reviewer is a real reviewer: a clean or nits-only pass APPROVEs so
// its GitHub verdict reads as an approval rather than a non-committal comment.
// The Reviewer is deliberately NOT the rig's pr_approver, so this APPROVE is
// informational — human approval stays the merge gate (see the Reviewer
// runbook); it must not be wired into branch protection as a required approval.
//
// An explicit Disposition is validated by ParseFindings at the contract
// boundary, so the lookup-miss fallthrough below only fires for the empty
// (severity-derived) case on payloads from the sanctioned path.
func (fs *Findings) ReviewEvent() string {
	derived := fs.severityEvent()
	// Disposition is a FLOOR, never an override: the submitted event is the more
	// blocking of the explicit disposition and the severity tally.
	//
	// Letting the explicit value win outright is subtly wrong in both directions.
	// "approve" over a high finding is the obvious prompt-injection shape — the
	// Reviewer's primary input is the PR diff, attacker-influenced by
	// construction. But "comment" is worse in practice, because it needs no bad
	// actor at all: a perf lens legitimately escalating its own clean tally to
	// COMMENT would, under an override, silently downgrade a *different* lens's
	// high finding from REQUEST_CHANGES to COMMENT — averaging away exactly the
	// dissenting block that Consolidate's fold exists to preserve.
	//
	// Taking the maximum makes escalation the only possible effect, structurally,
	// for every disposition value rather than by special case.
	if ev, ok := validDispositions[strings.ToLower(strings.TrimSpace(fs.Disposition))]; ok {
		if eventRank(ev) > eventRank(derived) {
			return ev
		}
	}
	return derived
}

// eventRank orders review events by how blocking they are.
func eventRank(event string) int {
	switch event {
	case "COMMENT":
		return 1
	case "REQUEST_CHANGES":
		return 2
	}
	return 0 // APPROVE, or anything unrecognized
}

// SeverityEvent is the event finding severity alone implies, ignoring any
// disposition. Exported so callers can tell whether a disposition actually
// raised the verdict or merely agreed with the tally.
func (fs *Findings) SeverityEvent() string {
	return fs.severityEvent()
}

// severityEvent derives the review event from finding severity alone: a high
// finding blocks, and nothing else does.
//
// Only "high" is matched, so an empty or malformed priority contributes to
// APPROVE rather than blocking. That is safe at this layer because it is not
// the layer that validates: ParseFindings rejects any non-empty unrecognized
// priority outright, and normalizeFinding defaults an empty one to "medium" —
// which no longer changes the verdict either way. Every finding still posts as
// its own thread, and unresolved threads gate the merge for every rig,
// identity-agnostic and config-free; severity decides only whether the review
// itself withholds approval.
func (fs *Findings) severityEvent() string {
	for _, f := range fs.Findings {
		// Out-of-scope findings never gate the verdict. They are real and are
		// still posted, but a PR must not be blocked on work its own diff does
		// not contain — that demand belongs to a follow-up, not this merge.
		if f.Scope == ScopeOut {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(f.Priority), "high") {
			return "REQUEST_CHANGES"
		}
	}
	return "APPROVE"
}

// ParseFindings unmarshals and validates the findings payload. Priorities are
// normalized to lowercase; an empty priority defaults to "medium" (advisory),
// while a non-empty unrecognized priority is a hard error so a typo can't
// silently drop a finding's severity. Every finding must carry a path, a
// positive line, and a title.
func ParseFindings(data []byte) (*Findings, error) {
	var fs Findings
	if err := decodeStrictJSON(data, &fs); err != nil {
		return nil, fmt.Errorf("parsing findings JSON: %w", err)
	}
	if err := fs.Summary.Normalize("findings.summary"); err != nil {
		return nil, err
	}
	fs.Disposition = strings.ToLower(strings.TrimSpace(fs.Disposition))
	if fs.Disposition != "" {
		if _, ok := validDispositions[fs.Disposition]; !ok {
			// Name "approve" specifically: it is the value an agent is most
			// likely to reach for, and a bare "invalid" would not explain why
			// escalation is the only direction the override travels.
			if fs.Disposition == "approve" {
				return nil, fmt.Errorf("findings.disposition=approve is not accepted: the override may " +
					"only escalate a verdict, never de-escalate. A clean or nits-only payload already " +
					"derives APPROVE from severity — drop the disposition")
			}
			return nil, fmt.Errorf("findings.%s", dispositionError(fs.Disposition))
		}
	}
	for i := range fs.Findings {
		if err := normalizeFinding(&fs.Findings[i], fmt.Sprintf("findings[%d]", i)); err != nil {
			return nil, err
		}
	}
	// No de-escalation check is needed here: ReviewEvent takes the maximum of the
	// disposition and the severity tally, so a disposition ranking below the
	// tally is inert by construction rather than by validation. That matters
	// because comment-with-a-high-finding is a LEGITIMATE payload — one lens
	// escalating its own clean tally while another lens found something high —
	// and rejecting it would break correct usage. The floor resolves it to
	// REQUEST_CHANGES, which is the right answer.
	return &fs, nil
}

// normalizeFinding validates and canonicalizes a single finding in place: path,
// positive line, and title are required; priority is lowercased and must be in
// the closed set (empty defaults to "medium", a non-empty unknown is a hard
// error so a typo cannot silently drop severity). ctx is a caller-supplied
// prefix for error messages (e.g. "findings[3]" or "perspective security").
// Shared by ParseFindings and ParsePerspectiveResult so the two entry points
// cannot drift in what they accept.
func normalizeFinding(f *Finding, ctx string) error {
	f.Path = strings.TrimSpace(f.Path)
	if f.Path == "" {
		return fmt.Errorf("%s: path is required", ctx)
	}
	if f.Line <= 0 {
		return fmt.Errorf("%s (%s): line must be positive, got %d", ctx, f.Path, f.Line)
	}
	f.Title = strings.TrimSpace(f.Title)
	if f.Title == "" {
		return fmt.Errorf("%s (%s): title is required", ctx, f.Path)
	}
	p := strings.ToLower(strings.TrimSpace(f.Priority))
	if p == "" {
		p = "medium"
	} else if !validPriorities[p] {
		return fmt.Errorf("%s (%s): invalid priority %q (want high, medium, or low)",
			ctx, f.Path, f.Priority)
	}
	f.Priority = p
	f.Perspective = strings.TrimSpace(f.Perspective)
	f.Body = strings.TrimSpace(f.Body)
	f.Suggestion = strings.TrimSpace(f.Suggestion)
	// Enforce the length budgets last, so the sizes reported are the ones the
	// trimmed payload actually carries.
	for _, lim := range []struct {
		field string
		val   string
		max   int
	}{
		{"title", f.Title, MaxTitleLen},
		{"body", f.Body, MaxBodyLen},
		{"suggestion", f.Suggestion, MaxSuggestionLen},
	} {
		// Count runes, not bytes: a budget measured in bytes would silently
		// halve for non-ASCII findings.
		if n := len([]rune(lim.val)); n > lim.max {
			return fmt.Errorf("%s (%s): %s is %d characters, over the %d limit — "+
				"state the failure and the change to make; move supporting analysis "+
				"out or split it into a second finding on its own line",
				ctx, f.Path, lim.field, n, lim.max)
		}
	}
	return nil
}

// FormatBody renders a single finding into the inline-thread body shape the
// refinery's parseThreadPriority and review-fix dispatch expect:
//
//	![high](https://img.shields.io/badge/priority-high-red.svg)
//	**[adversarial]** <title>
//
//	<body>
//
//	Suggested fix:
//	<suggestion>
//
// The neutral shields.io badge is emitted by the shared refinery.PriorityBadge
// so the emitter and the parser cannot drift.
func (f Finding) FormatBody() string {
	var b strings.Builder
	if badge := refinery.PriorityBadge(f.Priority); badge != "" {
		b.WriteString(badge)
		b.WriteString("\n")
	}
	if f.Perspective != "" {
		fmt.Fprintf(&b, "**[%s]** %s\n", f.Perspective, f.Title)
	} else {
		fmt.Fprintf(&b, "**%s**\n", f.Title)
	}
	if strings.TrimSpace(f.Body) != "" {
		b.WriteString("\n")
		b.WriteString(strings.TrimRight(f.Body, "\n"))
		b.WriteString("\n")
	}
	if strings.TrimSpace(f.Suggestion) != "" {
		b.WriteString("\nSuggested fix:\n")
		b.WriteString(strings.TrimRight(f.Suggestion, "\n"))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// BuildComments renders every finding into a provider-agnostic inline review
// comment, preserving input order.
func (fs *Findings) BuildComments() []refinery.ReviewComment {
	out := make([]refinery.ReviewComment, 0, len(fs.Findings))
	for _, f := range fs.Findings {
		out = append(out, refinery.ReviewComment{
			Path: f.Path,
			Line: f.Line,
			Body: f.FormatBody(),
		})
	}
	return out
}

// SummaryBody renders the review's top-level body from the template, as
// GitHub-flavored markdown, in a fixed section order:
//
//	### Blockers        — derived: the findings that hold the merge
//	### Verdicts        — authored: one line per perspective
//	### Opportunities   — authored: out-of-scope follow-up candidates
//	footer              — derived: the priority tally and the reviewed SHA
//
// Blockers leads because it answers the question a reader opens the review
// with. It is derived from the findings rather than written, so it cannot
// disagree with the threads or cost the author budget, and it is omitted
// entirely when nothing blocks — the absence IS the "nothing blocks" signal.
//
// The reviewed SHA prefers reviewedSHA, then the payload's ReviewedSHA, so a
// human can see which commit was reviewed even if upstream HEAD has moved.
func (fs *Findings) SummaryBody(reviewedSHA string) string {
	var b strings.Builder
	if blockers := fs.blockers(); len(blockers) > 0 {
		b.WriteString("### Blockers\n\n")
		for _, f := range blockers {
			fmt.Fprintf(&b, "- `%s:%d` — %s", f.Path, f.Line, f.Title)
			if f.Perspective != "" {
				fmt.Fprintf(&b, " [%s]", f.Perspective)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	fs.Summary.render(&b)
	b.WriteString("\n---\n\n")
	b.WriteString(fs.countLine())
	sha := reviewedSHA
	if sha == "" {
		sha = fs.ReviewedSHA
	}
	if sha != "" {
		fmt.Fprintf(&b, " · **Reviewed SHA:** `%s`", sha)
	}
	return b.String()
}

// blockers returns the findings that actually hold the merge: high priority and
// inside the diff. Out-of-scope findings are excluded for the same reason
// severityEvent skips them — a PR must not be reported as blocked on work its
// own diff does not contain (Consolidate has already demoted them anyway).
func (fs *Findings) blockers() []Finding {
	var out []Finding
	for _, f := range fs.Findings {
		if f.Scope == ScopeOut {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(f.Priority), "high") {
			out = append(out, f)
		}
	}
	return out
}

// countLine summarizes finding counts by priority, e.g.
// "**Findings:** 3 (high: 1, medium: 1, low: 1)".
func (fs *Findings) countLine() string {
	counts := map[string]int{}
	for _, f := range fs.Findings {
		counts[f.Priority]++
	}
	if len(fs.Findings) == 0 {
		return "**Findings:** 0 — no findings."
	}
	parts := make([]string, 0, 3)
	for _, p := range []string{"high", "medium", "low"} {
		if counts[p] > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d", p, counts[p]))
		}
	}
	return fmt.Sprintf("**Findings:** %d (%s)", len(fs.Findings), strings.Join(parts, ", "))
}

// BuildReviewInput assembles the full SubmitReview payload from parsed findings.
func (fs *Findings) BuildReviewInput(commitSHA string) refinery.SubmitReviewInput {
	return refinery.SubmitReviewInput{
		CommitID: commitSHA,
		Body:     fs.SummaryBody(commitSHA),
		Comments: fs.BuildComments(),
		Event:    fs.ReviewEvent(),
	}
}
