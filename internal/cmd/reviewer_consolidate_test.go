package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	// restore is both deferred and called explicitly on the happy path,
	// guarded by sync.Once. fn() may call t.Fatalf, which runs
	// runtime.Goexit and unwinds only through deferred calls — without the
	// defer, a Fatalf inside fn() skips the restore and leaves os.Stderr
	// pointed at this pipe for the rest of the test binary. But the restore
	// must also run BEFORE the blocking `<-done` receive below (which only
	// unblocks once w is closed), so the happy path can't rely on defer
	// alone or it would deadlock waiting on a pipe it never closed.
	var once sync.Once
	restore := func() {
		once.Do(func() {
			os.Stderr = orig
			_ = w.Close()
		})
	}
	defer restore()
	fn()
	restore()
	return <-done
}

const consolidateTestSHA = "0123456789abcdef"

func runResultPass(t *testing.T, dir string, r reviewer.PerspectiveResult) {
	t.Helper()
	if r.Findings == nil {
		r.Findings = []reviewer.Finding{}
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	runStubPass(t, dir, r.Perspective, consolidateTestSHA, string(data))
}

func setConsolidateFlags(t *testing.T, out string) {
	t.Helper()
	reviewerConsolidateOut, reviewerConsolidateSHA, reviewerConsolidatePR = out, consolidateTestSHA, 7
	t.Cleanup(func() { reviewerConsolidateOut, reviewerConsolidateSHA, reviewerConsolidatePR = "", "", 0 })
}

// The verdict notice must reach EVERY invocation, not just --out mode. Posting
// is irreversible and the Reviewer cannot clear its own review, so the
// sanctioned `consolidate | post --findings -` pipe (which has no --out) must
// still reveal the event at the last reversible step.
func TestRunReviewerConsolidate_AnnouncesTheEventInBothModes(t *testing.T) {
	dir := stubReviewerPasses(t, "adversarial")
	runResultPass(t, dir, reviewer.PerspectiveResult{Perspective: "adversarial", Verdict: "no findings"})

	for _, tc := range []struct{ name, out string }{
		{"stdout mode", ""},
		{"--out mode", filepath.Join(dir, "findings.json")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setConsolidateFlags(t, tc.out)

			stderr := captureReviewerStderr(t, func() {
				if err := runReviewerConsolidate(nil, nil); err != nil {
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
	dir := stubReviewerPasses(t, "security")

	// A disposition that RAISES the verdict above the severity tally: say so.
	runResultPass(t, dir, reviewer.PerspectiveResult{
		Perspective: "security", Verdict: "BLOCK: architectural", Disposition: "request_changes",
	})
	setConsolidateFlags(t, "")
	stderr := captureReviewerStderr(t, func() {
		if err := runReviewerConsolidate(nil, nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stderr, "raised from APPROVE") {
		t.Errorf("an escalating disposition must be named as the cause: %q", stderr)
	}

	// A disposition the floor ABSORBED (it agrees with, or is below, the tally)
	// changed nothing — naming it as the cause of a block sends the reader to the
	// wrong lens.
	runResultPass(t, dir, reviewer.PerspectiveResult{
		Perspective: "security", Verdict: "advisory", Disposition: "comment",
		Findings: []reviewer.Finding{{
			Path: "a.go", Line: 1, Priority: "high", Title: "boom", Body: "b", Perspective: "security",
		}},
	})
	stderr = captureReviewerStderr(t, func() {
		if err := runReviewerConsolidate(nil, nil); err != nil {
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
