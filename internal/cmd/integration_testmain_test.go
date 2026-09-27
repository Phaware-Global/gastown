//go:build integration

package cmd

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

func TestMain(m *testing.M) {
	// Force sequential test execution to avoid bd file locks on Windows.
	_ = flag.Set("test.parallel", "1")
	flag.Parse()

	// Match CI's Integration job env (ci.yml sets BD_DISABLE_METRICS=1) for the
	// whole process. Many tests call exec.Command("bd", ...) without a cmd.Env,
	// so they inherit this; without it a local run spools bd metrics into the
	// developer's real ~/.beads and prints the first-run metrics notice into
	// output the tests parse (gt-va4b).
	if err := os.Setenv("BD_DISABLE_METRICS", "1"); err != nil {
		fmt.Fprintf(os.Stderr, "integration TestMain: set BD_DISABLE_METRICS: %v\n", err)
		os.Exit(1)
	}

	// Start an ephemeral Dolt container for this package's integration tests.
	// Tests like TestAgentWorktreesStayClean and TestBeadsRoutingFromTownRoot
	// spawn gt/bd subprocesses that create databases (e.g., "tr", "hq").
	// By routing to an isolated container (via GT_DOLT_PORT), those databases
	// are destroyed when the container is terminated at cleanup —
	// preventing orphan accumulation in the shared production Dolt data dir.
	if err := testutil.EnsureDoltContainerForTestMain(); err != nil {
		fmt.Fprintf(os.Stderr, "integration TestMain: dolt setup: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	// Clean up the shared Dolt container.
	testutil.TerminateDoltContainer()
	os.Exit(code)
}
