package polecat

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

func TestMain(m *testing.M) {
	// No test in this package may start the real indexer; see ensureCodegraphIndex.
	ensureCodegraphIndex = func(_, _, _ string) error { return nil }
	code := m.Run()
	testutil.TerminateDoltContainer()
	os.Exit(code)
}
