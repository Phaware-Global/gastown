package reviewer

import (
	"fmt"
	"strings"
)

// OriginRefinery and OriginCrew identify which source dispatched a review.
const (
	OriginRefinery = "refinery"
	OriginCrew     = "crew"
)

// RequestSpec is the town-generated metadata that describes a review request.
// It deliberately carries NO attacker-influenced text (no PR body / diff) — only
// the PR number, the SHA/branch to review, the round, the origin, and the MR
// bead id when the refinery dispatched it.
type RequestSpec struct {
	PR      int
	HeadSHA string
	Branch  string
	Round   int    // 1-based; >= 2 is a re-review after a fix round
	Origin  string // OriginRefinery | OriginCrew
	MRID    string // set only for refinery-origin requests
}

// Subject is the one-line mail subject for the request.
func (s RequestSpec) Subject() string {
	return fmt.Sprintf("Review request: PR #%d (round %d)", s.PR, s.roundOrDefault())
}

func (s RequestSpec) roundOrDefault() int {
	if s.Round < 1 {
		return 1
	}
	return s.Round
}

// Body renders the structured review-request body the Reviewer session reads
// from its mailbox. priorThreads, when non-empty, is the deterministically
// assembled prior-round context (the fix loop's unresolved threads) appended for
// round >= 2 — the Reviewer must not gather it itself.
func (s RequestSpec) Body(priorThreads string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "REVIEW_REQUEST\n")
	fmt.Fprintf(&b, "pr: %d\n", s.PR)
	if s.HeadSHA != "" {
		fmt.Fprintf(&b, "head_sha: %s\n", s.HeadSHA)
	}
	if s.Branch != "" {
		fmt.Fprintf(&b, "branch: %s\n", s.Branch)
	}
	fmt.Fprintf(&b, "round: %d\n", s.roundOrDefault())
	fmt.Fprintf(&b, "origin: %s\n", s.originOrDefault())
	if s.MRID != "" {
		fmt.Fprintf(&b, "mr: %s\n", s.MRID)
	}
	if strings.TrimSpace(priorThreads) != "" {
		b.WriteString("\nPRIOR_ROUND_THREADS (do not relitigate unchanged code):\n")
		b.WriteString(strings.TrimRight(priorThreads, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

func (s RequestSpec) originOrDefault() string {
	if s.Origin == OriginRefinery || s.Origin == OriginCrew {
		return s.Origin
	}
	if s.MRID != "" {
		return OriginRefinery
	}
	return OriginCrew
}

// DefaultOrigin resolves the origin for a request given an explicit flag and
// whether an MR id is present: explicit value wins, else refinery when an MR is
// set, else crew.
func DefaultOrigin(explicit, mrID string) string {
	if explicit == OriginRefinery || explicit == OriginCrew {
		return explicit
	}
	if mrID != "" {
		return OriginRefinery
	}
	return OriginCrew
}
