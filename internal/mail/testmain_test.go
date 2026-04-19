package mail

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

func TestMain(m *testing.M) {
	// The mail package's own tests legitimately exercise the real
	// Router.Send path — they verify routing behavior, address parsing,
	// fan-out, etc. Opt this package's test binary out of the
	// test-binary guard in Router.Send (see test_guard.go). Other
	// packages that test mail-using code must use an in-memory fake;
	// they do NOT get this opt-in.
	setAllowTestSend(true)

	code := m.Run()
	testutil.TerminateDoltContainer()
	os.Exit(code)
}
