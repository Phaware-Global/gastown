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

// ConsolidatePasses consolidates verified passes with the coordinator's edits.
// Every posted finding names the raw findings it came from.
func ConsolidatePasses(passes []VerifiedPass, edits *Edits, reviewedSHA string, manifest DiffManifest) (*Findings, error) {
	results := make([]PerspectiveResult, len(passes))
	for i, p := range passes {
		results[i] = p.Result
	}
	fs, err := ConsolidateWithEdits(results, edits, reviewedSHA, manifest)
	if err != nil {
		return nil, err
	}
	for i, p := range passes {
		v := &fs.Summary.Verdicts[i]
		v.PassFinishedAt = p.Receipt.FinishedAt.UTC().Format(time.RFC3339)
		v.ExtraInstructions = p.Receipt.ExtraInstructions
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
