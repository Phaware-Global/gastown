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

// The pass runner writes each perspective's raw result and a sidecar receipt
// binding it to the PR, the commit, the prompt and the run's times, so there is
// evidence that a result came out of a pass that ran to completion. Nothing
// requires a receipt yet.
//
// The receipt is unsigned: a session with shell access can compute the hashes.

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

// SameCommit reports whether two hex object names (full or abbreviated to at
// least seven characters) name the same commit.
func SameCommit(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= 7 && strings.HasPrefix(b, a)
}
