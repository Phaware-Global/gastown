package mail

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckTestSendGuard_AllowTestSendBypassesGuard is the precondition
// for this whole package's tests: when allowTestSendFlag is true (set by
// TestMain) the guard is a no-op, regardless of whether os.Args[0]
// looks like a test binary. The existing router tests rely on this.
func TestCheckTestSendGuard_AllowTestSendBypassesGuard(t *testing.T) {
	// TestMain set this for the package — documenting the invariant.
	if !allowTestSendFlag.Load() {
		t.Fatal("TestMain should have set allowTestSendFlag=true for this package")
	}
	if err := checkTestSendGuard(); err != nil {
		t.Errorf("checkTestSendGuard returned %v when allowTestSendFlag=true; want nil", err)
	}
}

// TestCheckTestSendGuard_FiresUnderTestBinary simulates a test-binary
// without the package opt-in and asserts the guard blocks.
//
// NOT t.Parallel-safe: mutates allowTestSendFlag. The rest of this
// package's tests run sequentially by default, so there's no hazard
// today. If a future maintainer adds t.Parallel() elsewhere, the
// flag's atomic.Bool type keeps reads/writes race-free, but ordering
// with other tests' Send calls is still not guaranteed — keep this
// test sequential.
func TestCheckTestSendGuard_FiresUnderTestBinary(t *testing.T) {
	prev := allowTestSendFlag.Load()
	setAllowTestSend(false)
	t.Cleanup(func() { setAllowTestSend(prev) })

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
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{"test binary", "production", "in-memory"} {
		if !strings.Contains(msg, needle) {
			t.Errorf("error message missing anchor %q; got: %s", needle, err.Error())
		}
	}
}

// TestIsTestBinaryName covers the shared classifier so future changes
// to the binary-suffix matching logic can't silently weaken the guard.
// Uses the production helper directly (not a test-side mirror) so the
// test and the guarded path can't drift.
func TestIsTestBinaryName(t *testing.T) {
	tests := []struct {
		name string
		arg0 string
		want bool
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
		// would false-positive. Accepted in the docstring — no gastown
		// tool ships with that name.
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isTestBinaryName(filepath.Base(tc.arg0))
			if got != tc.want {
				t.Errorf("isTestBinaryName(filepath.Base(%q)) = %v; want %v", tc.arg0, got, tc.want)
			}
		})
	}
}

// TestRunningAsTestBinary_DirectViaArgs0 exercises runningAsTestBinary()
// end-to-end by temporarily reassigning os.Args[0]. This complements
// TestIsTestBinaryName (which covers the classifier) by also covering
// the filepath.Base + len(os.Args) guards.
//
// Restores os.Args[0] in t.Cleanup. NOT t.Parallel-safe.
func TestRunningAsTestBinary_DirectViaArgs0(t *testing.T) {
	if len(os.Args) == 0 {
		t.Skip("os.Args is empty — can't test the reassignment path")
	}
	prev := os.Args[0]
	t.Cleanup(func() { os.Args[0] = prev })

	for _, tc := range []struct {
		name string
		arg0 string
		want bool
	}{
		{"real go-test binary", "/tmp/go-build/foo.test", true},
		{"production-ish binary", "/usr/local/bin/gt", false},
		{"empty string", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			os.Args[0] = tc.arg0
			if got := runningAsTestBinary(); got != tc.want {
				t.Errorf("runningAsTestBinary() with os.Args[0]=%q = %v; want %v", tc.arg0, got, tc.want)
			}
		})
	}
}
