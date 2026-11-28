//go:build !integration

package cmd

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/gitenv"
)

// The integration build has its own TestMain.
func TestMain(m *testing.M) {
	gitenv.ClearInheritedConfig()
	os.Exit(m.Run())
}
