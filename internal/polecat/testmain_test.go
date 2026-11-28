package polecat

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
	"github.com/steveyegge/gastown/internal/testutil/gitenv"
)

func TestMain(m *testing.M) {
	gitenv.ClearInheritedConfig()
	code := m.Run()
	testutil.TerminateDoltContainer()
	os.Exit(code)
}

func TestHookTestsIgnoreInheritedHooksPath(t *testing.T) {
	gitenv.RequirePassUnderHooksOverride(t, "TestRemoveWithOptions_MissingWorktreeWorkerPushDuringReleaseIsRefused")
}
