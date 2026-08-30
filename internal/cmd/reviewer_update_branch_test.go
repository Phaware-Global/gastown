package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// ghHeadSHAStub installs a fake `gh` that answers the headRefOid query with
// `before` for the first (calls-1) invocations and `after` from then on,
// standing in for the delay between `gh pr update-branch` returning 202 and
// GitHub publishing the merge commit.
func ghHeadSHAStub(t *testing.T, before, after string, switchAfter int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub is POSIX-only")
	}
	dir := t.TempDir()
	counter := filepath.Join(dir, "calls")
	script := "#!/bin/sh\n" +
		"n=$(cat " + counter + " 2>/dev/null || echo 0)\n" +
		"n=$((n+1)); echo $n > " + counter + "\n" +
		"if [ \"$n\" -ge " + itoa(switchAfter) + " ]; then\n" +
		"  echo '{\"headRefOid\":\"" + after + "\"}'\n" +
		"else\n" +
		"  echo '{\"headRefOid\":\"" + before + "\"}'\n" +
		"fi\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil { //nolint:gosec // test stub must be executable
		t.Fatalf("write gh stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func shortenHeadMoveWait(t *testing.T, timeout, poll time.Duration) {
	t.Helper()
	origTimeout, origPoll := prHeadMoveTimeout, prHeadMovePollWait
	prHeadMoveTimeout, prHeadMovePollWait = timeout, poll
	t.Cleanup(func() { prHeadMoveTimeout, prHeadMovePollWait = origTimeout, origPoll })
}

// The update-branch REST call returns 202 Accepted — the merge commit is queued,
// not published — so the head must be polled. Accepting the first answer would
// check out the pre-update commit and review exactly the stale SHA the update
// exists to avoid.
func TestWaitForPRHeadToMove_PollsPastTheStaleAnswer(t *testing.T) {
	shortenHeadMoveWait(t, 5*time.Second, time.Millisecond)
	ghHeadSHAStub(t, "oldsha", "newsha", 3)

	if !waitForPRHeadToMove(git.NewGit(t.TempDir()), 1, "oldsha") {
		t.Error("the head moved on the third poll; waitForPRHeadToMove should have seen it")
	}
}

// A head that never moves must time out rather than hang the review: the caller
// warns and proceeds against whatever is published.
func TestWaitForPRHeadToMove_TimesOutWhenTheHeadNeverMoves(t *testing.T) {
	shortenHeadMoveWait(t, 150*time.Millisecond, time.Millisecond)
	ghHeadSHAStub(t, "oldsha", "oldsha", 1)

	start := time.Now()
	if waitForPRHeadToMove(git.NewGit(t.TempDir()), 1, "oldsha") {
		t.Error("an unmoved head must not report as updated")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %s — the poll must be bounded by the timeout", elapsed)
	}
}

// The comparison is case-insensitive: gh and the API both return lowercase hex,
// but a caller passing an upper-cased SHA must not read as "already moved".
func TestWaitForPRHeadToMove_MatchesTheHeadCaseInsensitively(t *testing.T) {
	shortenHeadMoveWait(t, 150*time.Millisecond, time.Millisecond)
	ghHeadSHAStub(t, "abcdef", "abcdef", 1)

	if waitForPRHeadToMove(git.NewGit(t.TempDir()), 1, "ABCDEF") {
		t.Error("the same SHA in a different case is not a moved head")
	}
}
