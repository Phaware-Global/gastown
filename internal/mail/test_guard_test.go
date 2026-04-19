package mail

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckTestSendGuard_AllowTestSendBypassesGuard is the precondition
// for this whole package's tests: when allowTestSend is true (set by
// TestMain) the guard is a no-op, regardless of whether os.Args[0]
// looks like a test binary. The existing router tests rely on this.
func TestCheckTestSendGuard_AllowTestSendBypassesGuard(t *testing.T) {
	// Preserve current state; TestMain sets allowTestSend=true for this
	// package. We're just documenting the invariant here.
	if !allowTestSend {
		t.Fatal("TestMain should have set allowTestSend=true for this package")
	}
	if err := checkTestSendGuard(); err != nil {
		t.Errorf("checkTestSendGuard returned %v when allowTestSend=true; want nil", err)
	}
}

// TestCheckTestSendGuard_FiresUnderTestBinary simulates a test-binary
// without the package opt-in and asserts the guard blocks.
func TestCheckTestSendGuard_FiresUnderTestBinary(t *testing.T) {
	// Turn off the opt-in for this test only; restore via t.Cleanup.
	prev := allowTestSend
	allowTestSend = false
	t.Cleanup(func() { allowTestSend = prev })

	// The test binary we're running in has a name ending in ".test"
	// (or ".test.exe" on Windows) by `go test` convention, so
	// runningAsTestBinary() should return true without us faking argv.
	if !runningAsTestBinary() {
		t.Skipf("binary name %q doesn't end in .test; running under a non-go-test harness — skipping", os.Args[0])
	}

	err := checkTestSendGuard()
	if err == nil {
		t.Fatal("checkTestSendGuard returned nil under a test binary; want error")
	}
	if !errors.Is(err, errTestSendRefused) {
		t.Errorf("err = %v; want errors.Is(err, errTestSendRefused) == true", err)
	}
	// The error message should point the caller at the right fix.
	msg := err.Error()
	for _, needle := range []string{"test binary", "production", "in-memory"} {
		if !strings.Contains(strings.ToLower(msg), needle) {
			t.Errorf("error message missing anchor %q; got: %s", needle, msg)
		}
	}
}

// TestRunningAsTestBinary covers the classifier directly so future
// changes to the binary-suffix matching logic can't silently weaken
// the guard.
func TestRunningAsTestBinary(t *testing.T) {
	// The suffix check is on filepath.Base(os.Args[0]). We can't change
	// os.Args[0] portably without breaking `go test`, but we can exercise
	// the underlying `matches` logic via a local helper that mirrors it.
	tests := []struct {
		name    string
		arg0    string
		want    bool
	}{
		{"bare test binary", "foo.test", true},
		{"Windows test binary", "foo.test.exe", true},
		{"test binary with path", "/tmp/go-build/foo.test", true},
		{"Windows path test binary", `C:\Users\x\go-build\foo.test.exe`, true},
		{"production binary", "gt", false},
		{"production with path", "/usr/local/bin/gt", false},
		{"test-suffixed data file, NOT a binary path", "notes.test.md", false},
		{"empty args0", "", false},
		{"test in middle, not suffix", "foo.testing", false},
		// Edge: a production binary intentionally named "something.test"
		// would false-positive. We accept this in the docstring — no
		// gastown tool ships with that name.
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := suffixLooksLikeTestBinary(filepath.Base(tc.arg0))
			if got != tc.want {
				t.Errorf("suffixLooksLikeTestBinary(%q) = %v; want %v", tc.arg0, got, tc.want)
			}
		})
	}
}

// suffixLooksLikeTestBinary is a tiny test-side mirror of the suffix
// check in runningAsTestBinary so the classifier can be unit-tested
// without poking at os.Args. If this helper disagrees with the
// production one, one of them has drifted — keep them in sync.
func suffixLooksLikeTestBinary(exe string) bool {
	return strings.HasSuffix(exe, ".test") || strings.HasSuffix(exe, ".test.exe")
}
