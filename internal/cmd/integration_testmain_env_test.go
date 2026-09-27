//go:build integration

package cmd

import (
	"os"
	"testing"
)

// The integration suite shells out to bd directly in many places (raw
// exec.Command("bd", ...) with no cmd.Env), so those calls inherit the test
// process environment rather than testutil.CleanGTEnv. CI's Integration job
// exports BD_DISABLE_METRICS=1; TestMain must do the same so a local run behaves
// like CI: bd's first-run "anonymous usage metrics" notice does not pollute
// parsed output, and no metrics spooler/flusher writes into the developer's
// real ~/.beads during the run (gt-va4b, gt-bglj).
func TestIntegrationMain_DisablesBDMetrics(t *testing.T) {
	if got := os.Getenv("BD_DISABLE_METRICS"); got != "1" {
		t.Errorf("BD_DISABLE_METRICS = %q in the integration test process, want %q "+
			"(raw exec.Command(\"bd\") calls inherit it; set it in TestMain)", got, "1")
	}
}
