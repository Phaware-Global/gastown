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
	// Disposition optionally escalates this pass's verdict beyond what its
	// findings' severity implies: "request_changes" or "comment".
	//
	// This exists because normalizeFinding requires every finding to anchor to a
	// path and a positive line, so an objection that is architectural — real, but
	// with no single diff line to attach it to — cannot be expressed as a
	// finding at all. Without this field such a pass could only put "BLOCK: do
	// not merge" in its free-text Verdict, which no code reads, and the review
	// would post as APPROVE (zero findings) with "do not merge" as its body.
	//
	// Escalation only: "approve" is rejected here, because a pass de-escalating
	// its own verdict has no legitimate use and is the shape a prompt injection
	// in the reviewed diff would take.
	Disposition string `json:"disposition,omitempty"`
}

// dispositionError explains a rejected disposition, naming OMISSION first.
//
// The order matters. The message is the only error-recovery guidance a
// perspective pass has — its role template has no step for a rejected result —
// so a message that enumerates only the two accepted values steers the repair
// toward picking one of them, which reproduces a false block (REQUEST_CHANGES
// with zero threads, unclearable). Omission is the correct repair in almost
// every case that reaches here.
func dispositionError(d string) string {
	trimmed := strings.TrimSpace(d)
	if strings.HasPrefix(trimmed, "<") && strings.HasSuffix(trimmed, ">") {
		return fmt.Sprintf("disposition %q is the schema PLACEHOLDER copied literally — "+
			"drop the field entirely unless you are escalating", d)
	}
	return fmt.Sprintf("disposition %q is invalid: omit the field entirely unless you are "+
		"escalating. The only accepted values are request_changes and comment — a perspective "+
		"may escalate its verdict, never de-escalate", d)
}

// dispositionRank orders dispositions by how blocking they are, so consolidating
// several perspectives can take the most blocking one. Unknown/empty is 0.
func dispositionRank(d string) int {
	switch strings.ToLower(strings.TrimSpace(d)) {
	case "comment":
		return 1
	case "request_changes":
		return 2
	}
	return 0
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
	// The verdict becomes one line in the consolidated summary; a newline would
	// break the "one line per perspective" contract and the badge parser.
	if strings.ContainsAny(r.Verdict, "\n\r") {
		return nil, fmt.Errorf("perspective result (%s): verdict must be a single line (no newlines)", r.Perspective)
	}
	r.Verdict = strings.TrimSpace(r.Verdict)
	// Escalation-only: a pass may raise its verdict above what its findings
	// imply, never lower it. "approve" is rejected rather than ignored so the
	// contract violation is visible instead of silently dropped.
	r.Disposition = strings.ToLower(strings.TrimSpace(r.Disposition))
	if r.Disposition != "" && dispositionRank(r.Disposition) == 0 {
		return nil, fmt.Errorf("perspective result (%s): %s", r.Perspective, dispositionError(r.Disposition))
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
//   - Findings are deduplicated by (path, line) — NOT by title. Four lenses
//     describing one defect in four different sentences are one defect and need
//     one fix, but a title-keyed dedup saw four distinct findings and emitted
//     four blocking threads (observed on graphql-api PR #114: 1 high + 3 medium
//     all on line 88, all the same organizationMemberships[0] bug). Every
//     distinct title is preserved in the merged body, so no lens's framing is
//     lost — only the duplicate threads are. The higher priority wins and the
//     perspective tags are unioned. First occurrence sets the position, so the
//     output order is stable.
//   - Low-priority findings on the same file are then collapsed into a single
//     "nits" thread per file. Every unresolved thread blocks the refinery's fix
//     loop regardless of severity, so a scatter of individually-trivial nits
//     costs exactly as much to clear as a scatter of real defects.
//
// Doing dedup here, in tested Go, keeps it deterministic rather than leaving it
// to per-run reviewer judgment.
func Consolidate(results []PerspectiveResult, reviewedSHA string, manifest DiffManifest) *Findings {
	var sb strings.Builder
	sb.WriteString("Per-perspective verdicts:\n")
	for _, r := range results {
		fmt.Fprintf(&sb, "- [%s] %s\n", r.Perspective, strings.TrimSpace(r.Verdict))
	}

	type dedupKey struct {
		path string
		line int
	}
	index := map[dedupKey]int{}
	var out []Finding
	for _, r := range results {
		for _, f := range r.Findings {
			k := dedupKey{f.Path, f.Line}
			if idx, ok := index[k]; ok {
				// The surviving thread takes the most severe priority, so a high
				// from any lens is never softened by a medium from another.
				if priorityRank(f.Priority) > priorityRank(out[idx].Priority) {
					out[idx].Priority = f.Priority
					// Title follows severity: the most severe framing of the
					// defect is the one a fixer should read first.
					if !strings.EqualFold(strings.TrimSpace(f.Title), strings.TrimSpace(out[idx].Title)) {
						out[idx].Body = mergeText(out[idx].Body, "Also flagged as: "+out[idx].Title)
						out[idx].Title = f.Title
					}
				} else if !strings.EqualFold(strings.TrimSpace(f.Title), strings.TrimSpace(out[idx].Title)) {
					// A distinct framing from an equal-or-lower lens is kept in
					// the body. Dropping it would lose the reason that lens
					// flagged the line at all.
					out[idx].Body = mergeText(out[idx].Body, "Also flagged as: "+f.Title)
				}
				out[idx].Perspective = mergePerspectives(out[idx].Perspective, f.Perspective)
				// Union the remediation paths. This one is load-bearing, not
				// bookkeeping: Classify caps a finding at out-of-scope when ANY
				// remediation path falls outside the diff, so dropping the
				// duplicate's paths would let a lens that omitted them mask a
				// lens that named them honestly — and the merged finding would
				// post as a blocking demand for work in untouched files. Whether
				// that happened would depend on subagent arrival order.
				out[idx].RemediationPaths = mergeRemediationPaths(
					out[idx].RemediationPaths, f.RemediationPaths)
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

	// Classify against the diff before anything reads severity. A nil manifest
	// leaves every finding ScopeUnknown and changes nothing, so a reviewer
	// running without diff data behaves exactly as it did before.
	for i := range out {
		out[i].Scope = manifest.Classify(out[i])
		if out[i].Scope == ScopeOut {
			// Demote rather than drop. The finding may well be correct — it is
			// simply not this PR's to fix, so it is posted as advisory and the
			// body says why.
			out[i].Priority = "low"
			out[i].Body = mergeText(OutOfScopeNotice(manifest, out[i]), out[i].Body)
		}
	}

	out = collapseLowFindings(out)

	// Fold the per-perspective dispositions by taking the most blocking one: if
	// any single lens says "request changes", the consolidated review must say
	// so, regardless of what the other lenses concluded. A dissenting block is
	// never averaged away.
	disposition := ""
	for _, r := range results {
		if dispositionRank(r.Disposition) > dispositionRank(disposition) {
			disposition = strings.ToLower(strings.TrimSpace(r.Disposition))
		}
	}

	// A request_changes disposition is the one channel that blocks a merge
	// without creating a thread, which means nothing in town can clear it: the
	// fix loop is thread-driven, so an unanchored block needs an operator. The
	// contract invites passes to reach for it precisely when they cannot anchor
	// an objection — "an architectural objection, a concern about the change as
	// a whole" — which is also the exact shape of an out-of-scope demand.
	//
	// So it is honoured only when the round also found something blocking
	// INSIDE the diff. With a manifest present and no in-scope blocking finding,
	// it is softened to comment: the objection still posts, in full, and still
	// raises the verdict above a bare approve — it just no longer creates an
	// unclearable block over work this PR does not contain.
	if disposition == "request_changes" && manifest != nil && !hasBlockingInScope(out) {
		disposition = "comment"
	}

	return &Findings{
		Summary:     strings.TrimRight(sb.String(), "\n"),
		ReviewedSHA: reviewedSHA,
		Findings:    out,
		Disposition: disposition,
	}
}

// collapseLowFindings merges a file's low-priority findings into one thread.
//
// Severity governs the GitHub review event but NOT what blocks: the refinery's
// fix loop polls unresolved threads and is author- and severity-agnostic ("every
// unresolved thread counts"), so a low nit costs a full resolve cycle exactly
// like a high. A round that scatters seven lows across a file therefore imposes
// seven round-trips for issues the review itself calls non-blocking.
//
// Collapsing is per-file rather than global so each thread still lands on the
// file it concerns, and the surviving anchor is the earliest line so the thread
// sorts naturally against the others. Nothing is discarded — every title, body
// and suggestion is folded into the merged body.
//
// Findings at or above medium are untouched: they are individually actionable
// and a fixer needs them anchored where they occur.
func collapseLowFindings(in []Finding) []Finding {
	// Count lows per path first. A single low on a file is left exactly as it
	// is: wrapping one finding in "nits" machinery would only obscure it.
	lowCount := map[string]int{}
	for _, f := range in {
		if strings.EqualFold(f.Priority, "low") {
			lowCount[f.Path]++
		}
	}

	merged := map[string]int{} // path -> index in out of that path's nits thread
	var out []Finding
	for _, f := range in {
		if !strings.EqualFold(f.Priority, "low") || lowCount[f.Path] < 2 {
			out = append(out, f)
			continue
		}
		idx, ok := merged[f.Path]
		if !ok {
			// Seed the collapsed thread from the first low on this path,
			// preserving its anchor.
			seed := f
			seed.Body = mergeText("", nitEntry(f))
			seed.Suggestion = ""
			out = append(out, seed)
			merged[f.Path] = len(out) - 1
			continue
		}
		if f.Line < out[idx].Line {
			out[idx].Line = f.Line
		}
		out[idx].Perspective = mergePerspectives(out[idx].Perspective, f.Perspective)
		out[idx].Body = mergeText(out[idx].Body, nitEntry(f))
	}

	// Title the collapsed threads once their membership is final.
	for path, idx := range merged {
		out[idx].Title = fmt.Sprintf("Nits: %d non-blocking findings in this file", lowCount[path])
	}
	return out
}

// nitEntry renders one low finding as a bullet inside a collapsed nits thread,
// keeping its own line number so the reader can still locate it.
func nitEntry(f Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- line %d", f.Line)
	if f.Perspective != "" {
		fmt.Fprintf(&b, " [%s]", f.Perspective)
	}
	fmt.Fprintf(&b, ": %s", strings.TrimSpace(f.Title))
	if body := strings.TrimSpace(f.Body); body != "" {
		fmt.Fprintf(&b, "\n  %s", strings.ReplaceAll(body, "\n", "\n  "))
	}
	if sug := strings.TrimSpace(f.Suggestion); sug != "" {
		fmt.Fprintf(&b, "\n  Suggested fix: %s", strings.ReplaceAll(sug, "\n", "\n  "))
	}
	return b.String()
}

// hasBlockingInScope reports whether any finding both blocks (high) and lands
// inside the diff. ScopeUnknown counts as in-scope: without a manifest there is
// no evidence of a violation, and absent evidence must not silently disarm a
// blocking verdict.
func hasBlockingInScope(findings []Finding) bool {
	for _, f := range findings {
		if f.Scope == ScopeOut {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(f.Priority), "high") {
			return true
		}
	}
	return false
}

// mergeRemediationPaths unions two remediation-path lists, preserving first-seen
// order and dropping blanks and duplicates.
//
// Comparison is case-sensitive and exact, matching DiffManifest lookups: paths
// come from the same repo-relative namespace on both sides, and case-folding
// here would let two genuinely different paths on a case-sensitive filesystem
// collapse into one.
func mergeRemediationPaths(existing, add []string) []string {
	seen := make(map[string]bool, len(existing)+len(add))
	var out []string
	for _, src := range [][]string{existing, add} {
		for _, p := range src {
			p = strings.TrimSpace(p)
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
