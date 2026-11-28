package deacon

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/prodguard"
)

func TestMain(m *testing.M) {
	done := prodguard.Install()
	os.Exit(done(m.Run()))
}
