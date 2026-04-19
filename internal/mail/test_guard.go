package mail

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// allowTestSend short-circuits the test-binary guard in Router.Send.
// It is ONLY set by this package's own TestMain so the mail package's
// routing tests can exercise the real Send path. It is unexported so
// other packages cannot opt in.
//
// Production code must never touch this variable.
var allowTestSend bool

// runningAsTestBinary reports whether the current process was launched
// by `go test`. Go's test framework compiles each package into a binary
// whose name ends in `.test` (or `.test.exe` on Windows) and launches
// it with that name as os.Args[0]. We use that as the sole signal —
// env vars like GT_TEST_MODE require test-author cooperation (TestMain),
// which is easy to forget; a binary-name check is automatic and fails
// loud for every test binary regardless of setup.
//
// Accepted false-positive risk: a production binary intentionally named
// `*.test` or stored under a path fragment that ends that way. Nobody
// ships production binaries with those names. If that ever changes,
// switching to a `go test`-only build-tag gate is the next step.
func runningAsTestBinary() bool {
	if len(os.Args) == 0 {
		return false
	}
	exe := filepath.Base(os.Args[0])
	return strings.HasSuffix(exe, ".test") || strings.HasSuffix(exe, ".test.exe")
}

// errTestSendRefused is returned by Router.Send when a test binary
// tries to send mail through the production backend.
//
// Exported as a package sentinel so tests that want to VERIFY the guard
// fires can match on `errors.Is(err, errTestSendRefused)`. Unexported
// so external callers can't silence the guard by checking for it.
var errTestSendRefused = fmt.Errorf("mail.Router.Send refused under test binary")

// checkTestSendGuard returns a non-nil error when the current process
// is a `go test` binary AND the mail package's own tests have not
// opted in. The error message points the caller at the in-memory test
// fake they should be using instead.
//
// Split out from Router.Send so it can be unit-tested without spinning
// up a real Router.
func checkTestSendGuard() error {
	if allowTestSend || !runningAsTestBinary() {
		return nil
	}
	return fmt.Errorf("%w (binary=%q): code under test called mail.Router.Send against the production Dolt-backed mail queue. "+
		"Tests that exercise mail-sending code must construct an in-memory test router "+
		"(e.g. mail.NewMemoryRouter in a future PR) and inject it. Real mail from tests pollutes the mayor's inbox and slows the whole engine via Dolt write amplification.",
		errTestSendRefused, os.Args[0])
}
