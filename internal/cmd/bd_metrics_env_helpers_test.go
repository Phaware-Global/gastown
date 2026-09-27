package cmd

import (
	"os"
	"strings"
	"testing"
)

// bdMetricsEnvSetups covers the caller-environment states a test env builder
// must be immune to: a developer shell that never set BD_DISABLE_METRICS, and
// ones that set it to a value that would leave metrics on.
var bdMetricsEnvSetups = map[string]func(t *testing.T){
	"unset":     func(t *testing.T) { t.Setenv("BD_DISABLE_METRICS", ""); os.Unsetenv("BD_DISABLE_METRICS") },
	"set to 0":  func(t *testing.T) { t.Setenv("BD_DISABLE_METRICS", "0") },
	"set to 1":  func(t *testing.T) { t.Setenv("BD_DISABLE_METRICS", "1") },
	"set empty": func(t *testing.T) { t.Setenv("BD_DISABLE_METRICS", "") },
}

// assertBDMetricsDisabled fails unless env carries exactly one
// BD_DISABLE_METRICS entry and it is "1".
//
// Test envs point HOME at a t.TempDir() and run real gt/bd subprocesses. bd's
// metrics spooler writes $HOME/.beads/eventsData and spawns a detached
// `bd send-metrics` flusher that can create eventkit.lock there after the test
// returns, failing t.TempDir's RemoveAll ("directory not empty", gt-bglj,
// gt-6kbn). Helpers that strip BD_* themselves must re-assert the variable.
func assertBDMetricsDisabled(t *testing.T, helper string, env []string) {
	t.Helper()
	var got []string
	for _, e := range env {
		if strings.HasPrefix(e, "BD_DISABLE_METRICS=") {
			got = append(got, e)
		}
	}
	if len(got) != 1 || got[0] != "BD_DISABLE_METRICS=1" {
		t.Errorf("%s BD_DISABLE_METRICS entries = %q, want exactly [BD_DISABLE_METRICS=1]", helper, got)
	}
}
