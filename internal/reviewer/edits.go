package reviewer

import (
	"fmt"
	"strconv"
	"strings"
)

// Edits are the coordinator's changes to the raw perspective results. A finding is
// addressed by its raw id, "perspective:N", its 1-based position in that
// perspective's result. Edits are carried in the payload so posting can reproduce it.
type Edits struct {
	Drop            []DropEdit        `json:"drop,omitempty"`
	DropOpportunity []string          `json:"drop_opportunity,omitempty"` // "perspective:N"
	Merge           [][]string        `json:"merge,omitempty"`            // the first id gives the anchor
	Reword          []RewordEdit      `json:"reword,omitempty"`
	Note            []NoteEdit        `json:"note,omitempty"`
	Reanchor        []ReanchorEdit    `json:"reanchor,omitempty"`
	Verdict         map[string]string `json:"verdict,omitempty"`
	BaseSHA         string            `json:"base_sha,omitempty"` // overridden diff base
}

// DropEdit removes a finding for a stated reason.
type DropEdit struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// RewordEdit replaces the non-empty fields of a finding.
type RewordEdit struct {
	ID         string `json:"id"`
	Title      string `json:"title,omitempty"`
	Body       string `json:"body,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

// NoteEdit adds the reviewer's own text to a finding.
type NoteEdit struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// ReanchorEdit moves a finding to another line of its file.
type ReanchorEdit struct {
	ID   string `json:"id"`
	Line int    `json:"line"`
}

// DroppedFinding is a raw finding the review leaves out, as listed in its body.
type DroppedFinding struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Priority string `json:"priority"`
	Title    string `json:"title"`
	Reason   string `json:"reason"`
}

// MaxReasonLen bounds a drop reason, in runes.
const MaxReasonLen = 160

// IsZero reports whether no edit was made.
func (e *Edits) IsZero() bool {
	return e == nil || len(e.Drop)+len(e.DropOpportunity)+len(e.Merge)+len(e.Reword)+
		len(e.Note)+len(e.Reanchor)+len(e.Verdict) == 0 && e.BaseSHA == ""
}

// ParseEdits decodes an edits file.
func ParseEdits(data []byte) (*Edits, error) {
	var e Edits
	if err := decodeStrictJSON(data, &e); err != nil {
		return nil, fmt.Errorf("parsing edits JSON: %w", err)
	}
	return &e, nil
}

// describe lists the edits the body does not already show next to what they
// changed.
func (e *Edits) describe() string {
	if e == nil {
		return ""
	}
	var parts []string
	for _, group := range e.Merge {
		parts = append(parts, "merged "+strings.Join(group, "+"))
	}
	for _, r := range e.Reanchor {
		parts = append(parts, fmt.Sprintf("re-anchored %s to line %d", r.ID, r.Line))
	}
	if len(e.DropOpportunity) > 0 {
		parts = append(parts, "dropped opportunities "+strings.Join(e.DropOpportunity, ", "))
	}
	if e.BaseSHA != "" {
		parts = append(parts, "set the diff base to "+e.BaseSHA)
	}
	return strings.Join(parts, "; ")
}

func oneLine(s string) bool { return !strings.ContainsAny(s, "\n\r") }

// RawID names the i-th (0-based) finding of a perspective's raw result.
func RawID(perspective string, i int) string { return fmt.Sprintf("%s:%d", perspective, i+1) }

type findingPos struct{ result, finding int }

// applyEdits returns edited copies of the results, each finding tagged with its
// raw id, plus the findings left out. An edit that matches nothing is an error,
// so a typo cannot pass for a change that was made.
func applyEdits(results []PerspectiveResult, e *Edits) ([]PerspectiveResult, []DroppedFinding, error) {
	if e == nil {
		e = &Edits{}
	}
	out := append([]PerspectiveResult(nil), results...)
	byName := map[string]int{}
	byID := map[string]findingPos{}
	for ri := range out {
		byName[out[ri].Perspective] = ri
		out[ri].Findings = append([]Finding(nil), out[ri].Findings...)
		for fi := range out[ri].Findings {
			id := RawID(out[ri].Perspective, fi)
			out[ri].Findings[fi].Sources = []string{id}
			byID[id] = findingPos{ri, fi}
		}
	}

	dropped := map[string]string{} // id -> reason
	find := func(id string) (*Finding, error) {
		pos, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("no raw finding %q (ids are perspective:N, N from 1)", id)
		}
		if _, gone := dropped[id]; gone {
			return nil, fmt.Errorf("finding %s is dropped and also edited", id)
		}
		return &out[pos.result].Findings[pos.finding], nil
	}

	for _, d := range e.Drop {
		if _, err := find(d.ID); err != nil {
			return nil, nil, err
		}
		reason := strings.TrimSpace(d.Reason)
		if reason == "" || !oneLine(reason) || len([]rune(reason)) > MaxReasonLen {
			return nil, nil, fmt.Errorf("drop %s: a one-line reason of at most %d characters is required",
				d.ID, MaxReasonLen)
		}
		dropped[d.ID] = reason
	}

	reworded := map[string]bool{}
	for _, r := range e.Reword {
		f, err := find(r.ID)
		if err != nil {
			return nil, nil, err
		}
		if reworded[r.ID] {
			return nil, nil, fmt.Errorf("finding %s is reworded twice", r.ID)
		}
		reworded[r.ID] = true
		var fields []string
		for _, field := range []struct {
			name, value string
			target      *string
		}{
			{"title", r.Title, &f.Title},
			{"body", r.Body, &f.Body},
			{"suggestion", r.Suggestion, &f.Suggestion},
		} {
			if v := strings.TrimSpace(field.value); v != "" {
				*field.target = v
				fields = append(fields, field.name)
			}
		}
		if len(fields) == 0 {
			return nil, nil, fmt.Errorf("reword %s changes nothing", r.ID)
		}
		if !oneLine(f.Title) {
			return nil, nil, fmt.Errorf("reword %s: a title is one line", r.ID)
		}
		f.Reworded = []string{r.ID + " " + strings.Join(fields, ", ")}
	}

	for _, n := range e.Note {
		f, err := find(n.ID)
		if err != nil {
			return nil, nil, err
		}
		text := strings.TrimSpace(n.Text)
		if text == "" {
			return nil, nil, fmt.Errorf("note on %s is empty", n.ID)
		}
		if n := len([]rune(mergeText(f.ReviewerNote, text))); n > MaxNoteLen {
			return nil, nil, fmt.Errorf("note is %d characters, over the %d limit", n, MaxNoteLen)
		}
		f.ReviewerNote = mergeText(f.ReviewerNote, text)
	}

	for _, r := range e.Reanchor {
		f, err := find(r.ID)
		if err != nil {
			return nil, nil, err
		}
		if r.Line < 1 {
			return nil, nil, fmt.Errorf("reanchor %s: line must be positive", r.ID)
		}
		f.Line = r.Line
	}

	// A merge moves the others onto the first one's anchor, where consolidation
	// folds findings together.
	merged := map[string]bool{}
	for _, group := range e.Merge {
		if len(group) < 2 {
			return nil, nil, fmt.Errorf("a merge needs at least two ids, got %v", group)
		}
		var anchor *Finding
		for _, id := range group {
			f, err := find(id)
			if err != nil {
				return nil, nil, err
			}
			if merged[id] {
				return nil, nil, fmt.Errorf("finding %s is merged twice", id)
			}
			merged[id] = true
			if anchor == nil {
				anchor = f
				continue
			}
			f.Path, f.Line = anchor.Path, anchor.Line
		}
	}

	for name, text := range e.Verdict {
		ri, ok := byName[name]
		if !ok {
			return nil, nil, fmt.Errorf("verdict edit: no pass for perspective %q", name)
		}
		text = strings.TrimSpace(text)
		if text == "" || !oneLine(text) {
			return nil, nil, fmt.Errorf("verdict edit for %s: want one non-empty line", name)
		}
		out[ri].Verdict = text
	}

	droppedOpp := map[string]bool{}
	for _, ref := range e.DropOpportunity {
		name, num, _ := strings.Cut(ref, ":")
		ri, ok := byName[name]
		n, err := strconv.Atoi(num)
		if !ok || err != nil || n < 1 || n > len(out[ri].Opportunities) {
			return nil, nil, fmt.Errorf("drop_opportunity %q matches nothing (want perspective:N)", ref)
		}
		droppedOpp[RawID(name, n-1)] = true
	}

	var left []DroppedFinding
	for ri := range out {
		name := out[ri].Perspective
		var findings []Finding
		for fi, f := range out[ri].Findings {
			id := RawID(name, fi)
			if reason, gone := dropped[id]; gone {
				left = append(left, DroppedFinding{ID: id, Path: f.Path, Line: f.Line,
					Priority: f.Priority, Title: f.Title, Reason: reason})
				continue
			}
			findings = append(findings, f)
		}
		var opportunities []string
		for oi, o := range out[ri].Opportunities {
			if !droppedOpp[RawID(name, oi)] {
				opportunities = append(opportunities, o)
			}
		}
		out[ri].Findings, out[ri].Opportunities = findings, opportunities
	}
	return out, left, nil
}

// ConsolidateWithEdits consolidates raw results with the coordinator's edits.
// Every finding names the raw findings it came from.
func ConsolidateWithEdits(results []PerspectiveResult, edits *Edits, reviewedSHA string, manifest DiffManifest) (*Findings, error) {
	edited, dropped, err := applyEdits(results, edits)
	if err != nil {
		return nil, err
	}
	fs := Consolidate(edited, reviewedSHA, manifest)
	if edits != nil {
		for i, r := range results {
			_, fs.Summary.Verdicts[i].Reworded = edits.Verdict[r.Perspective]
		}
	}
	fs.Dropped = dropped
	if !edits.IsZero() {
		fs.Edits = edits
	}
	return fs, nil
}
