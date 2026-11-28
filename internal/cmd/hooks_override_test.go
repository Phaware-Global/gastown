package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/gitenv"
)

func TestHookTestsIgnoreInheritedHooksPath(t *testing.T) {
	gitenv.RequirePassUnderHooksOverride(t, "TestLocalDonePushStillRunsHooks")
}
