package reviewer

import (
	"fmt"
	"strings"
)

// PerspectiveResult is the structured output a single perspective review pass
// (one subagent) returns: a required one-line verdict plus zero or more
// findings. It is the deterministic machine contract between a perspective pass
// and the consolidation step — the reviewer collects one of these per enabled
// perspective and consolidates them into the single posted review.
type PerspectiveResult struct {
	// Perspective is the lens that produced this result (e.g. "adversarial").
	Perspective string `json:"perspective"`
	// Verdict is the one-line per-perspective verdict. Required even with zero
	// findings, so the consolidated summary can account for every perspective
	// (the role contract forbids silence).
	Verdict string `json:"verdict"`
	// Findings are this pass's individual findings. May be empty.
	Findings []Finding `json:"findings"`
}

// ParsePerspectiveResult unmarshals and validates one perspective pass's output.
// Unknown fields are rejected (a malformed schema is an error, not silently
// dropped data), the verdict is required, and each finding is normalized through
// the same path/line/priority validation as ParseFindings. A finding with an
// empty perspective inherits the result's perspective.
func ParsePerspectiveResult(data []byte) (*PerspectiveResult, error) {
	var r PerspectiveResult
	if err := decodeStrictJSON(data, &r); err != nil {
		return nil, fmt.Errorf("parsing perspective result JSON: %w", err)
	}
	r.Perspective = strings.TrimSpace(r.Perspective)
	if r.Perspective == "" {
		return nil, fmt.Errorf("perspective result: perspective is required")
	}
	if strings.TrimSpace(r.Verdict) == "" {
		return nil, fmt.Errorf("perspective result (%s): verdict is required "+
			"(a perspective is never silent — say \"no findings\" explicitly)", r.Perspective)
	}
	for i := range r.Findings {
		// The execution contract requires every finding's perspective to match
		// the pass. Canonicalize to the pass perspective when empty OR a
		// case-variant (so downstream tags never differ only by casing); reject a
		// genuine mismatch rather than silently misattribute the finding.
		fp := strings.TrimSpace(r.Findings[i].Perspective)
		if fp == "" || strings.EqualFold(fp, r.Perspective) {
			r.Findings[i].Perspective = r.Perspective
		} else {
			return nil, fmt.Errorf("perspective result (%s): finding[%d] perspective %q "+
				"does not match the pass perspective", r.Perspective, i, fp)
		}
		if err := normalizeFinding(&r.Findings[i], fmt.Sprintf("perspective %s finding[%d]", r.Perspective, i)); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

// priorityRank orders priorities for "keep the most severe" dedup decisions.
func priorityRank(p string) int {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

// mergeText appends add to existing when add is non-empty and not already
// contained, separated by a blank line. Used so that when two perspectives raise
// the same finding, their differing explanations/suggestions are preserved
// rather than the second silently discarded.
func mergeText(existing, add string) string {
	add = strings.TrimSpace(add)
	if add == "" {
		return existing
	}
	existing = strings.TrimRight(existing, "\n")
	if strings.TrimSpace(existing) == "" {
		return add
	}
	// Compare against whole \n\n-separated blocks, not a raw substring search:
	// a distinct shorter explanation must not be swallowed just because it
	// happens to be a substring of a longer block from another perspective.
	for _, b := range strings.Split(existing, "\n\n") {
		if strings.TrimSpace(b) == add {
			return existing
		}
	}
	return existing + "\n\n" + add
}

// mergePerspectives unions two comma-separated perspective tags, preserving
// order and dropping duplicates, so a finding surfaced by two lenses is tagged
// "[adversarial, security]" rather than losing attribution.
func mergePerspectives(existing, add string) string {
	seen := map[string]bool{}
	var parts []string
	for _, src := range []string{existing, add} {
		for _, p := range strings.Split(src, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			// Dedup case-insensitively (preserving the first tag's casing) so
			// "adversarial" and "Adversarial" don't both appear.
			lowered := strings.ToLower(p)
			if seen[lowered] {
				continue
			}
			seen[lowered] = true
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", ")
}

// Consolidate deterministically merges per-perspective results into the single
// Findings payload that `gt reviewer post` consumes:
//
//   - The summary lists every perspective's verdict in input order, so a
//     perspective that found nothing is still explicitly accounted for.
//   - Findings are deduplicated by (path, line, case-folded title). When two
//     perspectives raise the same finding, the higher priority wins and the
//     perspective tags are unioned. First occurrence sets the position, so the
//     output order is stable.
//
// Doing dedup here, in tested Go, keeps it deterministic rather than leaving it
// to per-run reviewer judgment.
func Consolidate(results []PerspectiveResult, reviewedSHA string) *Findings {
	var sb strings.Builder
	sb.WriteString("Per-perspective verdicts:\n")
	for _, r := range results {
		fmt.Fprintf(&sb, "- [%s] %s\n", r.Perspective, strings.TrimSpace(r.Verdict))
	}

	type dedupKey struct {
		path  string
		line  int
		title string
	}
	index := map[dedupKey]int{}
	var out []Finding
	for _, r := range results {
		for _, f := range r.Findings {
			k := dedupKey{f.Path, f.Line, strings.ToLower(strings.TrimSpace(f.Title))}
			if idx, ok := index[k]; ok {
				if priorityRank(f.Priority) > priorityRank(out[idx].Priority) {
					out[idx].Priority = f.Priority
				}
				out[idx].Perspective = mergePerspectives(out[idx].Perspective, f.Perspective)
				// Preserve perspective-specific detail rather than discarding the
				// duplicate's body/suggestion.
				out[idx].Body = mergeText(out[idx].Body, f.Body)
				out[idx].Suggestion = mergeText(out[idx].Suggestion, f.Suggestion)
				continue
			}
			index[k] = len(out)
			out = append(out, f)
		}
	}

	return &Findings{
		Summary:     strings.TrimRight(sb.String(), "\n"),
		ReviewedSHA: reviewedSHA,
		Findings:    out,
	}
}
