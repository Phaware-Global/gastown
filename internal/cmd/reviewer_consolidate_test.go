package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/reviewer"
)

// captureReviewerStderr runs fn with os.Stderr redirected and returns what it
// wrote. Named distinctly from status_test.go's helper to avoid a collision.
func captureReviewerStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			}
			if rerr != nil {
				break
			}
		}
		done <- b.String()
	}()
	fn()
	_ = w.Close()
	os.Stderr = orig
	return <-done
}

func writeResult(t *testing.T, dir, name string, r reviewer.PerspectiveResult) string {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The verdict notice must reach EVERY invocation, not just --out mode. Posting
// is irreversible and the Reviewer cannot clear its own review, so the
// sanctioned `consolidate | post --findings -` pipe (which has no --out) must
// still reveal the event at the last reversible step.
func TestRunReviewerConsolidate_AnnouncesTheEventInBothModes(t *testing.T) {
	dir := t.TempDir()
	clean := writeResult(t, dir, "a.json", reviewer.PerspectiveResult{
		Perspective: "adversarial", Verdict: "no findings",
	})

	for _, tc := range []struct{ name, out string }{
		{"stdout mode", ""},
		{"--out mode", filepath.Join(dir, "findings.json")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reviewerConsolidateOut = tc.out
			reviewerConsolidateSHA = "deadbeef"
			t.Cleanup(func() { reviewerConsolidateOut, reviewerConsolidateSHA = "", "" })

			stderr := captureReviewerStderr(t, func() {
				if err := runReviewerConsolidate(nil, []string{clean}); err != nil {
					t.Fatalf("consolidate: %v", err)
				}
			})
			if !strings.Contains(stderr, "will post as APPROVE") {
				t.Errorf("stderr did not announce the event: %q", stderr)
			}
		})
	}
}

func TestRunReviewerConsolidate_OnlyNamesADispositionThatActuallyRaisedTheVerdict(t *testing.T) {
	dir := t.TempDir()

	// A disposition that RAISES the verdict above the severity tally: say so.
	raised := writeResult(t, dir, "raise.json", reviewer.PerspectiveResult{
		Perspective: "security", Verdict: "BLOCK: architectural", Disposition: "request_changes",
	})
	reviewerConsolidateOut, reviewerConsolidateSHA = "", "sha"
	t.Cleanup(func() { reviewerConsolidateOut, reviewerConsolidateSHA = "", "" })
	stderr := captureReviewerStderr(t, func() {
		if err := runReviewerConsolidate(nil, []string{raised}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stderr, "raised from APPROVE") {
		t.Errorf("an escalating disposition must be named as the cause: %q", stderr)
	}

	// A disposition the floor ABSORBED (it agrees with, or is below, the tally)
	// changed nothing — naming it as the cause of a block sends the reader to the
	// wrong lens.
	absorbed := writeResult(t, dir, "absorbed.json", reviewer.PerspectiveResult{
		Perspective: "perf", Verdict: "advisory", Disposition: "comment",
		Findings: []reviewer.Finding{{
			Path: "a.go", Line: 1, Priority: "high", Title: "boom", Body: "b", Perspective: "perf",
		}},
	})
	stderr = captureReviewerStderr(t, func() {
		if err := runReviewerConsolidate(nil, []string{absorbed}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stderr, "will post as REQUEST_CHANGES") {
		t.Errorf("expected the severity-derived block: %q", stderr)
	}
	if strings.Contains(stderr, "raised from") {
		t.Errorf("a disposition the floor absorbed must not be named as the cause: %q", stderr)
	}
}
