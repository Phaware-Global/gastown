package reviewer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A review is only as good as the passes behind it, so consolidating and posting
// require proof that each enabled perspective's result came out of the pass
// runner: the runner writes the result and a sidecar receipt binding it to the
// PR, the commit, the prompt and the run's times.
//
// The receipt is unsigned: a session with shell access can compute the hashes.
// It stops a coordinator that writes or edits results instead of waiting for
// the passes, not one that sets out to forge evidence.

// PassReceipt is the runner's record of one completed perspective pass.
type PassReceipt struct {
	PassID       string `json:"pass_id"`
	Perspective  string `json:"perspective"`
	PR           int    `json:"pr"`
	SHA          string `json:"sha"`
	Round        int    `json:"round"`
	PromptSHA256 string `json:"prompt_sha256"`
	ResultSHA256 string `json:"result_sha256"`
	// ExtraInstructions records that the caller added text to the prompt.
	ExtraInstructions bool      `json:"extra_instructions,omitempty"`
	StartedAt         time.Time `json:"started_at"`
	FinishedAt        time.Time `json:"finished_at"`
}

// Pass artifact names. Receipts do not share the result prefix, so a
// "perspective-*.json" glob matches results only.
func passPromptPath(dir, name string) string { return filepath.Join(dir, "prompt-"+name+".txt") }
func passResultPath(dir, name string) string {
	return filepath.Join(dir, "perspective-"+name+".json")
}
func passReceiptPath(dir, name string) string { return filepath.Join(dir, "receipt-"+name+".json") }
func passRejectedPath(dir, name string) string {
	return filepath.Join(dir, "rejected-"+name+".txt")
}

// RemovePassArtifacts deletes the result and receipt of each named perspective,
// so nothing from an earlier review can stand in for a pass that has not run.
func RemovePassArtifacts(dir string, names []string) error {
	for _, name := range names {
		for _, p := range []string{passReceiptPath(dir, name), passResultPath(dir, name)} {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("removing stale pass artifact %s: %w", p, err)
			}
		}
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// PassSpec is the input to one pass.
type PassSpec struct {
	Perspective       string
	PR                int
	SHA               string
	Round             int
	Prompt            string
	ExtraInstructions bool
}

// PassInvoker runs one headless agent on a prompt and returns its final answer.
// It must return an error unless the agent ran to completion.
type PassInvoker func(ctx context.Context, prompt string) (string, error)

// RunPass runs one perspective pass and writes its result and receipt into dir.
// A pass that fails, or answers outside the PerspectiveResult contract twice,
// leaves no receipt.
func RunPass(ctx context.Context, dir string, spec PassSpec, invoke PassInvoker) (*PassReceipt, error) {
	if err := RemovePassArtifacts(dir, []string{spec.Perspective}); err != nil {
		return nil, err
	}
	if err := os.WriteFile(passPromptPath(dir, spec.Perspective), []byte(spec.Prompt), 0o644); err != nil { //nolint:gosec // operator-facing artifact
		return nil, fmt.Errorf("writing prompt: %w", err)
	}
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("generating pass id: %w", err)
	}
	started := time.Now().UTC()

	var data []byte
	prompt := spec.Prompt
	for attempt := 1; ; attempt++ {
		answer, err := invoke(ctx, prompt)
		if err != nil {
			return nil, fmt.Errorf("%s pass did not complete: %w", spec.Perspective, err)
		}
		data = extractJSONObject(answer)
		_, verr := parsePassResult(data, spec.Perspective)
		if verr == nil {
			break
		}
		_ = os.WriteFile(passRejectedPath(dir, spec.Perspective), []byte(answer), 0o644) //nolint:gosec // debugging aid
		if attempt == 2 {
			return nil, fmt.Errorf("%s pass returned an invalid result twice (raw answer in %s): %w",
				spec.Perspective, passRejectedPath(dir, spec.Perspective), verr)
		}
		prompt = fmt.Sprintf("%s\n\n---\nYour previous answer was rejected: %v\n"+
			"Do not review again. Re-emit the same review, corrected to satisfy the output "+
			"contract above, as one JSON object and nothing else.\n\nPrevious answer:\n%s\n",
			spec.Prompt, verr, answer)
	}

	if err := os.WriteFile(passResultPath(dir, spec.Perspective), data, 0o644); err != nil { //nolint:gosec // operator-facing artifact
		return nil, fmt.Errorf("writing result: %w", err)
	}
	receipt := &PassReceipt{
		PassID:            hex.EncodeToString(id),
		Perspective:       spec.Perspective,
		PR:                spec.PR,
		SHA:               spec.SHA,
		Round:             spec.Round,
		PromptSHA256:      sha256Hex([]byte(spec.Prompt)),
		ResultSHA256:      sha256Hex(data),
		ExtraInstructions: spec.ExtraInstructions,
		StartedAt:         started,
		FinishedAt:        time.Now().UTC(),
	}
	encoded, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding receipt: %w", err)
	}
	if err := os.WriteFile(passReceiptPath(dir, spec.Perspective), append(encoded, '\n'), 0o644); err != nil { //nolint:gosec // operator-facing artifact
		return nil, fmt.Errorf("writing receipt: %w", err)
	}
	return receipt, nil
}

// extractJSONObject drops any code fence or prose around the answer's object.
func extractJSONObject(answer string) []byte {
	start, end := strings.Index(answer, "{"), strings.LastIndex(answer, "}")
	if start < 0 || end < start {
		return []byte(answer)
	}
	return []byte(answer[start:end+1] + "\n")
}

func parsePassResult(data []byte, perspective string) (*PerspectiveResult, error) {
	r, err := ParsePerspectiveResult(data)
	if err != nil {
		return nil, err
	}
	if r.Perspective != perspective {
		return nil, fmt.Errorf("result names perspective %q, the pass is %q", r.Perspective, perspective)
	}
	return r, nil
}

// VerifiedPass is a result whose receipt checked out.
type VerifiedPass struct {
	Result  PerspectiveResult
	Receipt PassReceipt
}

// VerifyPasses loads the result of every named perspective from dir and fails
// unless each one is backed by a matching receipt for this PR and commit. All
// problems are reported together, and none of the results are returned if any
// exist.
func VerifyPasses(dir string, names []string, pr int, sha string) ([]VerifiedPass, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("no enabled perspectives: nothing to verify")
	}
	var out []VerifiedPass
	var problems []error
	for _, name := range names {
		p, err := verifyPass(dir, name, pr, sha)
		if err != nil {
			problems = append(problems, fmt.Errorf("perspective %q: %w", name, err))
			continue
		}
		out = append(out, *p)
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("pass verification failed:\n%w\n\n"+
			"Wait for `gt reviewer run` to finish, or re-run the failed perspective with it. "+
			"Result files must not be written or edited by hand", errors.Join(problems...))
	}
	return out, nil
}

func verifyPass(dir, name string, pr int, sha string) (*VerifiedPass, error) {
	raw, err := os.ReadFile(passReceiptPath(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no receipt: the pass has not completed")
	}
	if err != nil {
		return nil, fmt.Errorf("reading receipt: %w", err)
	}
	var rc PassReceipt
	if err := decodeStrictJSON(raw, &rc); err != nil {
		return nil, fmt.Errorf("parsing receipt: %w", err)
	}
	switch {
	case rc.Perspective != name:
		return nil, fmt.Errorf("receipt belongs to perspective %q", rc.Perspective)
	case rc.PR != pr:
		return nil, fmt.Errorf("receipt is for PR #%d, not #%d", rc.PR, pr)
	case !SameCommit(rc.SHA, sha):
		return nil, fmt.Errorf("receipt is for commit %q, not %q", rc.SHA, sha)
	case rc.PassID == "":
		return nil, fmt.Errorf("receipt has no pass id")
	case rc.StartedAt.IsZero() || !rc.FinishedAt.After(rc.StartedAt):
		return nil, fmt.Errorf("incomplete pass: receipt end time is not after its start time")
	}

	promptPath := passPromptPath(dir, name)
	info, err := os.Stat(promptPath)
	if err != nil {
		return nil, fmt.Errorf("reading prompt: %w", err)
	}
	// Truncated to tolerate coarse filesystem timestamps.
	if rc.StartedAt.Before(info.ModTime().Truncate(time.Second)) {
		return nil, fmt.Errorf("receipt is older than the prompt: the pass did not run on it")
	}
	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		return nil, fmt.Errorf("reading prompt: %w", err)
	}
	if sha256Hex(prompt) != rc.PromptSHA256 {
		return nil, fmt.Errorf("prompt hash mismatch: the prompt changed after the pass ran")
	}

	data, err := os.ReadFile(passResultPath(dir, name))
	if err != nil {
		return nil, fmt.Errorf("reading result: %w", err)
	}
	if sha256Hex(data) != rc.ResultSHA256 {
		return nil, fmt.Errorf("result hash mismatch: the result was changed after the pass wrote it")
	}
	result, err := parsePassResult(data, name)
	if err != nil {
		return nil, err
	}
	return &VerifiedPass{Result: *result, Receipt: rc}, nil
}

// SameCommit reports whether two hex object names (full or abbreviated to at
// least seven characters) name the same commit.
func SameCommit(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= 7 && strings.HasPrefix(b, a)
}

// Edits are the coordinator's changes to the raw pass results. A finding is
// addressed by its raw id, "perspective:N", its 1-based position in that pass's
// result file. Edits are carried in the payload so posting can reproduce it.
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

// ConsolidatePasses consolidates verified passes with the coordinator's edits.
// Every posted finding names the raw findings it came from.
func ConsolidatePasses(passes []VerifiedPass, edits *Edits, reviewedSHA string, manifest DiffManifest) (*Findings, error) {
	results := make([]PerspectiveResult, len(passes))
	for i, p := range passes {
		results[i] = p.Result
	}
	results, dropped, err := applyEdits(results, edits)
	if err != nil {
		return nil, err
	}
	fs := Consolidate(results, reviewedSHA, manifest)
	for i, p := range passes {
		v := &fs.Summary.Verdicts[i]
		v.PassFinishedAt = p.Receipt.FinishedAt.UTC().Format(time.RFC3339)
		v.ExtraInstructions = p.Receipt.ExtraInstructions
		if edits != nil {
			_, v.Reworded = edits.Verdict[p.Result.Perspective]
		}
	}
	fs.Dropped = dropped
	if !edits.IsZero() {
		fs.Edits = edits
	}
	return fs, nil
}

// UntracedFinding returns an error for the first finding that does not come from
// the raw results of these passes.
func UntracedFinding(passes []VerifiedPass, findings []Finding) error {
	raw := map[string]bool{}
	for _, p := range passes {
		for i := range p.Result.Findings {
			raw[RawID(p.Result.Perspective, i)] = true
		}
	}
	for _, f := range findings {
		traced := len(f.Sources) > 0
		for _, id := range f.Sources {
			traced = traced && raw[id]
		}
		if !traced {
			return fmt.Errorf("finding %s:%d %q has no raw source: no pass raised %v",
				f.Path, f.Line, f.Title, f.Sources)
		}
	}
	return nil
}
