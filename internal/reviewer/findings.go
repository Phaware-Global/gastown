// Package reviewer holds the rig-level Reviewer role's pure logic: parsing the
// findings payload that `gt reviewer post` consumes and rendering it into the
// GitHub review contract (priority-badged inline threads + a summary body) that
// the refinery's existing parsers already understand. Keeping this logic in its
// own package — free of git/gh/tmux side effects — makes the output contract
// unit-testable in isolation. (P23-2376.)
package reviewer

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/refinery"
)

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
}

// validPriorities is the closed set of priorities the badge/parser pair models.
var validPriorities = map[string]bool{"high": true, "medium": true, "low": true}

// ParseFindings unmarshals and validates the findings payload. Priorities are
// normalized to lowercase; an empty priority defaults to "medium" (advisory),
// while a non-empty unrecognized priority is a hard error so a typo can't
// silently drop a finding's severity. Every finding must carry a path, a
// positive line, and a title.
func ParseFindings(data []byte) (*Findings, error) {
	var fs Findings
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fs); err != nil {
		return nil, fmt.Errorf("parsing findings JSON: %w", err)
	}
	if strings.TrimSpace(fs.Summary) == "" {
		return nil, fmt.Errorf("findings.summary is required (the review must never be silent)")
	}
	for i := range fs.Findings {
		f := &fs.Findings[i]
		if strings.TrimSpace(f.Path) == "" {
			return nil, fmt.Errorf("findings[%d]: path is required", i)
		}
		if f.Line <= 0 {
			return nil, fmt.Errorf("findings[%d] (%s): line must be positive, got %d", i, f.Path, f.Line)
		}
		if strings.TrimSpace(f.Title) == "" {
			return nil, fmt.Errorf("findings[%d] (%s): title is required", i, f.Path)
		}
		p := strings.ToLower(strings.TrimSpace(f.Priority))
		if p == "" {
			p = "medium"
		} else if !validPriorities[p] {
			return nil, fmt.Errorf("findings[%d] (%s): invalid priority %q (want high, medium, or low)",
				i, f.Path, f.Priority)
		}
		f.Priority = p
		f.Perspective = strings.TrimSpace(f.Perspective)
	}
	return &fs, nil
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
	}
}
