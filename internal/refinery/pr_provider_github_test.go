package refinery

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// ghStub installs a fake `gh` on PATH that answers the REST review listing with
// reviewsNDJSON (what `gh api --jq .[]` emits: one object per line) and records
// every invocation. It returns a function reading back the recorded calls.
func ghStub(t *testing.T, reviewsNDJSON string) func() []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub is POSIX-only")
	}
	dir := t.TempDir()
	reviews := filepath.Join(dir, "reviews.ndjson")
	calls := filepath.Join(dir, "calls.log")
	if err := os.WriteFile(reviews, []byte(reviewsNDJSON), 0o600); err != nil {
		t.Fatalf("write reviews: %v", err)
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + calls + "\n" +
		"case \"$*\" in\n" +
		"  *dismissals*) cat > /dev/null; echo '{}' ;;\n" +
		"  *) cat " + reviews + " ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil { //nolint:gosec // test stub must be executable
		t.Fatalf("write gh stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		data, err := os.ReadFile(calls) //nolint:gosec // test-local path
		if err != nil {
			return nil // no calls recorded
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

// dismissCalls returns only the recorded `gh` invocations that dismissed a review.
func dismissCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.Contains(c, "dismissals") {
			out = append(out, c)
		}
	}
	return out
}

// After a fix round the reviewer's prior CHANGES_REQUESTED still blocks the
// merge — GitHub does not dismiss it when the same reviewer approves — so the
// stale one must be dismissed by review ID before the new verdict is posted.
func TestDismissChangesRequestedReviews_DismissesOwnStaleVerdict(t *testing.T) {
	readCalls := ghStub(t, `{"id":11,"state":"CHANGES_REQUESTED","user":{"login":"gastown-reviewer"}}
{"id":12,"state":"COMMENTED","user":{"login":"gastown-reviewer"}}
{"id":13,"state":"CHANGES_REQUESTED","user":{"login":"Gastown-Reviewer"}}
`)
	p := newGitHubPRProvider(git.NewGit(t.TempDir()))

	if err := p.DismissChangesRequestedReviews(7, "gastown-reviewer", "Superseded"); err != nil {
		t.Fatalf("DismissChangesRequestedReviews: %v", err)
	}

	got := dismissCalls(readCalls())
	if len(got) != 2 {
		t.Fatalf("got %d dismissals, want 2 (the login match is case-insensitive):\n%s",
			len(got), strings.Join(got, "\n"))
	}
	for i, want := range []string{"pulls/7/reviews/11/dismissals", "pulls/7/reviews/13/dismissals"} {
		if !strings.Contains(got[i], want) {
			t.Errorf("dismissal[%d] = %q, want it to address %s", i, got[i], want)
		}
		if !strings.Contains(got[i], "PUT") {
			t.Errorf("dismissal[%d] = %q, want a PUT", i, got[i])
		}
	}
}

// A changes-request from anyone else is not ours to clear: dismissing a human
// reviewer's block would silently remove the strongest signal on the PR.
func TestDismissChangesRequestedReviews_SkipsOtherUsers(t *testing.T) {
	readCalls := ghStub(t, `{"id":21,"state":"CHANGES_REQUESTED","user":{"login":"a-human"}}
{"id":22,"state":"APPROVED","user":{"login":"gastown-reviewer"}}
`)
	p := newGitHubPRProvider(git.NewGit(t.TempDir()))

	if err := p.DismissChangesRequestedReviews(7, "gastown-reviewer", "Superseded"); err != nil {
		t.Fatalf("DismissChangesRequestedReviews: %v", err)
	}
	if got := dismissCalls(readCalls()); len(got) != 0 {
		t.Errorf("dismissed a review that was not ours:\n%s", strings.Join(got, "\n"))
	}
}

// The common case — a first-round review, or one that never blocked — must be a
// silent no-op rather than an error.
func TestDismissChangesRequestedReviews_NoOpWhenNoneFound(t *testing.T) {
	readCalls := ghStub(t, "")
	p := newGitHubPRProvider(git.NewGit(t.TempDir()))

	if err := p.DismissChangesRequestedReviews(7, "gastown-reviewer", "Superseded"); err != nil {
		t.Fatalf("a PR with no reviews must be a no-op, got: %v", err)
	}
	if got := dismissCalls(readCalls()); len(got) != 0 {
		t.Errorf("dismissed something on a PR with no reviews:\n%s", strings.Join(got, "\n"))
	}
}

// An empty user means "no configured pr_reviewer", not "everyone" — treating it
// as a wildcard would clear every changes-request on the PR.
func TestDismissChangesRequestedReviews_RequiresAUser(t *testing.T) {
	readCalls := ghStub(t, `{"id":31,"state":"CHANGES_REQUESTED","user":{"login":"a-human"}}
`)
	p := newGitHubPRProvider(git.NewGit(t.TempDir()))

	if err := p.DismissChangesRequestedReviews(7, "  ", "Superseded"); err == nil {
		t.Fatal("an empty user was accepted; it must be rejected, not treated as a wildcard")
	}
	if got := readCalls(); len(got) != 0 {
		t.Errorf("gh was called despite the missing identity:\n%s", strings.Join(got, "\n"))
	}
}

// Bitbucket cannot dismiss reviews; the reviewer's post path tolerates
// ErrUnsupported rather than failing the review it is there to submit.
func TestBitbucketDismissChangesRequestedReviews_Unsupported(t *testing.T) {
	p := &bitbucketPRProvider{}
	if err := p.DismissChangesRequestedReviews(7, "someone", "Superseded"); err != ErrUnsupported {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}
