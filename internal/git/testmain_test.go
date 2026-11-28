package git

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/gitenv"
)

func TestMain(m *testing.M) {
	gitenv.ClearInheritedConfig()
	os.Exit(m.Run())
}

func TestHookTestsIgnoreInheritedHooksPath(t *testing.T) {
	gitenv.RequirePassUnderHooksOverride(t, "TestCommitNoVerify_BypassesHook")
}
