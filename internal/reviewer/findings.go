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
}

// Findings is the full payload `gt reviewer post --findings <json>` consumes.
type Findings struct {
	// Summary is the per-perspective verdict + counts block that becomes the
	// review's top-level body. Required: the role contract forbids silence.
	Summary string `json:"summary"`
	// ReviewedSHA, when set, is appended to the summary so humans can see which
	// commit was reviewed even if upstream HEAD has since moved.
	ReviewedSHA string `json:"reviewed_sha,omitempty"`
	// Findings are the individual inline findings. May be empty (a clean review
	// still posts a summary).
	Findings []Finding `json:"findings"`
	// Disposition optionally overrides the GitHub review event the Reviewer
	// submits: "approve", "request_changes", or "comment" (case-insensitive).
	// When empty, the event is derived from finding severity (see ReviewEvent).
	// Lets a perspective pass assert a blocking verdict explicitly while keeping
	// a deterministic default when the agent omits it.
	Disposition string `json:"disposition,omitempty"`
}

// validPriorities is the closed set of priorities the badge/parser pair models.
var validPriorities = map[string]bool{"high": true, "medium": true, "low": true}

// validDispositions maps the findings-payload disposition (lowercased) to the
// GitHub review event it selects. The closed set is enforced at the contract
// boundary: ParseFindings rejects any non-empty, unrecognized disposition, so a
// typo fails loudly rather than silently degrading a blocking verdict.
var validDispositions = map[string]string{
	"approve":         "APPROVE",
	"request_changes": "REQUEST_CHANGES",
	"comment":         "COMMENT",
}

// ReviewEvent returns the GitHub review disposition for these findings:
// "APPROVE", "REQUEST_CHANGES", or "COMMENT". An explicit Disposition wins;
// otherwise it is derived from the highest severity present:
//
//	high   → REQUEST_CHANGES (blocking; must be addressed)
//	medium → COMMENT         (advisory; worth fixing, not a block)
//	low / none → APPROVE      (nits-only or clean — the Reviewer endorses it)
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
	if ev, ok := validDispositions[strings.ToLower(strings.TrimSpace(fs.Disposition))]; ok {
		return ev
	}
	hasMedium := false
	for _, f := range fs.Findings {
		// Normalize defensively for Findings built outside ParseFindings (which
		// rejects bad priorities). Only an explicit "low" permits APPROVE; an
		// empty priority is "medium" (advisory) per normalizeFinding, and any
		// unrecognized value is treated as medium too, so a malformed/typo
		// priority can never yield an accidental APPROVE.
		switch strings.ToLower(strings.TrimSpace(f.Priority)) {
		case "high":
			return "REQUEST_CHANGES"
		case "low":
			// nits-only — non-blocking, permits APPROVE
		default: // "medium", empty, or any unrecognized priority
			hasMedium = true
		}
	}
	if hasMedium {
		return "COMMENT"
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
	if strings.TrimSpace(fs.Summary) == "" {
		return nil, fmt.Errorf("findings.summary is required (the review must never be silent)")
	}
	fs.Disposition = strings.ToLower(strings.TrimSpace(fs.Disposition))
	if fs.Disposition != "" {
		if _, ok := validDispositions[fs.Disposition]; !ok {
			return nil, fmt.Errorf("findings.disposition %q is invalid (want approve, request_changes, or comment)", fs.Disposition)
		}
	}
	for i := range fs.Findings {
		if err := normalizeFinding(&fs.Findings[i], fmt.Sprintf("findings[%d]", i)); err != nil {
			return nil, err
		}
	}
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

// SummaryBody returns the review's top-level body: the agent-authored summary,
// a priority count line, and the reviewed SHA (preferring reviewedSHA, then the
// payload's ReviewedSHA). The count line gives humans and the refinery an
// at-a-glance severity tally without re-parsing every thread.
func (fs *Findings) SummaryBody(reviewedSHA string) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(fs.Summary, "\n"))
	b.WriteString("\n\n")
	b.WriteString(fs.countLine())
	sha := reviewedSHA
	if sha == "" {
		sha = fs.ReviewedSHA
	}
	if sha != "" {
		fmt.Fprintf(&b, "\nReviewed SHA: %s", sha)
	}
	return b.String()
}

// countLine summarizes finding counts by priority, e.g.
// "Findings: 3 (high: 1, medium: 1, low: 1)".
func (fs *Findings) countLine() string {
	counts := map[string]int{}
	for _, f := range fs.Findings {
		counts[f.Priority]++
	}
	if len(fs.Findings) == 0 {
		return "Findings: 0 — no findings."
	}
	parts := make([]string, 0, 3)
	for _, p := range []string{"high", "medium", "low"} {
		if counts[p] > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d", p, counts[p]))
		}
	}
	return fmt.Sprintf("Findings: %d (%s)", len(fs.Findings), strings.Join(parts, ", "))
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
