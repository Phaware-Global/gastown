package mail

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// allowTestSendFlag short-circuits the test-binary guard in Router.Send.
// It is ONLY flipped on by this package's own TestMain so the mail
// package's routing tests can exercise the real Send path. It is
// unexported so other packages cannot opt in.
//
// sync/atomic.Bool so any future test that uses t.Parallel() can't
// race with other tests reading the flag inside checkTestSendGuard.
// Production code must never touch this variable.
var allowTestSendFlag atomic.Bool

// setAllowTestSend flips the opt-in for this package's own tests.
// Unexported — callers outside the package cannot bypass the guard.
func setAllowTestSend(v bool) { allowTestSendFlag.Store(v) }

// isTestBinaryName reports whether `name` (expected to be
// filepath.Base(os.Args[0])) looks like a Go test binary. Shared
// between the production `runningAsTestBinary` classifier and the
// tests that cover it, so the two can't drift.
//
// Go's `go test` produces binaries whose names end in `.test` on
// Unix or `.test.exe` on Windows. We rely on that convention rather
// than env vars or build tags — see runningAsTestBinary's docstring
// for the trade-off analysis.
func isTestBinaryName(name string) bool {
	return strings.HasSuffix(name, ".test") ||
		strings.HasSuffix(name, ".test.exe")
}

// runningAsTestBinary reports whether the current process was launched
// by `go test`. Go's test framework compiles each package into a binary
// whose name ends in `.test` (or `.test.exe` on Windows) and launches
// it with that name as os.Args[0]. We use that as the sole signal —
// env vars like GT_TEST_MODE require test-author cooperation (TestMain),
// which is easy to forget; a binary-name check is automatic and fails
// loud for every test binary regardless of setup.
//
// Accepted false-positive risk: a production binary whose basename
// happens to end in `.test` / `.test.exe`. Only the basename matters
// here (filepath.Base is applied before the suffix check), so a
// directory like `/opt/foo.test/bin/gt` does NOT trigger — the base
// is `gt`. Nobody ships production binaries with those exact
// basenames; if that ever changes, switching to a `go test`-only
// build-tag gate is the next step.
func runningAsTestBinary() bool {
	if len(os.Args) == 0 {
		return false
	}
	return isTestBinaryName(filepath.Base(os.Args[0]))
}

// errTestSendRefused is returned (wrapped) by Router.Send when a test
// binary tries to send mail through the production backend.
//
// Unexported — in-package tests can match on it via errors.Is, but
// external callers cannot silence the guard by checking for it.
var errTestSendRefused = fmt.Errorf("mail.Router.Send refused under test binary")

// checkTestSendGuard returns a non-nil error when the current process
// is a `go test` binary AND the mail package's own tests have not
// opted in. The error message points the caller at the in-memory test
// fake they should be using instead.
//
// Split out from Router.Send so it can be unit-tested without spinning
// up a real Router.
func checkTestSendGuard() error {
	if allowTestSendFlag.Load() || !runningAsTestBinary() {
		return nil
	}
	return fmt.Errorf("%w (binary=%q): code under test called mail.Router.Send against the production Dolt-backed mail queue. "+
		"Tests that exercise mail-sending code must construct an in-memory test router "+
		"(e.g. mail.NewMemoryRouter in a future PR) and inject it. Real mail from tests pollutes the mayor's inbox and slows the whole engine via Dolt write amplification.",
		errTestSendRefused, os.Args[0])
}
