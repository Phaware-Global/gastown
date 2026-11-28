package refinery

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
	"github.com/steveyegge/gastown/internal/testutil/prodguard"
)

func TestMain(m *testing.M) {
	done := prodguard.Install()

	code := m.Run()
	testutil.TerminateDoltContainer()
	os.Exit(done(code))
}
