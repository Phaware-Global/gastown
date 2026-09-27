package testutil

import (
	"os"
	"strings"
	"testing"
)

func TestCleanGTEnv_PreservesDoltPort(t *testing.T) {
	t.Setenv("GT_DOLT_PORT", "13307")
	t.Setenv("GT_TOWN_ROOT", "/some/town")
	t.Setenv("BD_ACTOR", "polecat/test")

	env := CleanGTEnv()

	var hasDoltPort, hasTownRoot, hasBDActor bool
	for _, e := range env {
		switch {
		case strings.HasPrefix(e, "GT_DOLT_PORT="):
			hasDoltPort = true
		case strings.HasPrefix(e, "GT_TOWN_ROOT="):
			hasTownRoot = true
		case strings.HasPrefix(e, "BD_ACTOR="):
			hasBDActor = true
		}
	}

	if !hasDoltPort {
		t.Error("CleanGTEnv stripped GT_DOLT_PORT — must preserve it")
	}
	if hasTownRoot {
		t.Error("CleanGTEnv preserved GT_TOWN_ROOT — must strip it")
	}
	if hasBDActor {
		t.Error("CleanGTEnv preserved BD_ACTOR — must strip it")
	}
}

func TestCleanGTEnv_PreservesBeadsDoltPort(t *testing.T) {
	t.Setenv("BEADS_DOLT_PORT", "13307")
	t.Setenv("BD_DEBUG", "1")

	env := CleanGTEnv()

	var hasBeadsPort, hasBDDebug bool
	for _, e := range env {
		switch {
		case strings.HasPrefix(e, "BEADS_DOLT_PORT="):
			hasBeadsPort = true
		case strings.HasPrefix(e, "BD_DEBUG="):
			hasBDDebug = true
		}
	}

	if !hasBeadsPort {
		t.Error("CleanGTEnv stripped BEADS_DOLT_PORT — must preserve it")
	}
	if hasBDDebug {
		t.Error("CleanGTEnv preserved BD_DEBUG — must strip it")
	}
}

func TestCleanGTEnv_ExtraEnv(t *testing.T) {
	env := CleanGTEnv("HOME=/tmp/test", "FOO=bar")

	var hasHome, hasFoo bool
	for _, e := range env {
		switch {
		case e == "HOME=/tmp/test":
			hasHome = true
		case e == "FOO=bar":
			hasFoo = true
		}
	}

	if !hasHome {
		t.Error("CleanGTEnv did not include extra HOME override")
	}
	if !hasFoo {
		t.Error("CleanGTEnv did not include extra FOO=bar")
	}
}

func TestNewBDCommand_InheritsEnv(t *testing.T) {
	t.Setenv("GT_DOLT_PORT", "13307")

	cmd := NewBDCommand("version")
	// cmd.Env should be nil (inherits process env)
	if cmd.Env != nil {
		t.Error("NewBDCommand should not set cmd.Env (nil inherits process env)")
	}
	if cmd.Path == "" {
		t.Error("NewBDCommand returned empty command path")
	}
}

func TestNewIsolatedBDCommand_SetEnv(t *testing.T) {
	t.Setenv("GT_DOLT_PORT", "13307")
	t.Setenv("GT_TOWN_ROOT", "/some/town")

	cmd := NewIsolatedBDCommand("version")
	if cmd.Env == nil {
		t.Fatal("NewIsolatedBDCommand should set cmd.Env")
	}

	var hasDoltPort, hasTownRoot bool
	for _, e := range cmd.Env {
		switch {
		case strings.HasPrefix(e, "GT_DOLT_PORT="):
			hasDoltPort = true
		case strings.HasPrefix(e, "GT_TOWN_ROOT="):
			hasTownRoot = true
		}
	}

	if !hasDoltPort {
		t.Error("NewIsolatedBDCommand stripped GT_DOLT_PORT")
	}
	if hasTownRoot {
		t.Error("NewIsolatedBDCommand preserved GT_TOWN_ROOT")
	}
}

func TestNewIsolatedGTCommand_SetEnv(t *testing.T) {
	t.Setenv("GT_DOLT_PORT", "13307")

	cmd := NewIsolatedGTCommand("version")
	if cmd.Env == nil {
		t.Fatal("NewIsolatedGTCommand should set cmd.Env")
	}

	var hasDoltPort bool
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "GT_DOLT_PORT=") {
			hasDoltPort = true
		}
	}

	if !hasDoltPort {
		t.Error("NewIsolatedGTCommand stripped GT_DOLT_PORT")
	}
}

// bd's metrics spooler writes under $HOME/.beads/eventsData and spawns a
// detached `bd send-metrics` flusher that can create eventkit.lock there after
// the test returns. Tests set HOME to a t.TempDir(), so the late write races
// t.TempDir's RemoveAll ("directory not empty", gt-bglj). CleanGTEnv strips
// BD_*, so it must re-assert BD_DISABLE_METRICS itself — including when the
// caller's environment (e.g. a developer shell) never set it.
func TestCleanGTEnv_DisablesBDMetrics(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"unset":     func(t *testing.T) { t.Setenv("BD_DISABLE_METRICS", ""); os.Unsetenv("BD_DISABLE_METRICS") },
		"set to 0":  func(t *testing.T) { t.Setenv("BD_DISABLE_METRICS", "0") },
		"set to 1":  func(t *testing.T) { t.Setenv("BD_DISABLE_METRICS", "1") },
		"set empty": func(t *testing.T) { t.Setenv("BD_DISABLE_METRICS", "") },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			setup(t)

			var got []string
			for _, e := range CleanGTEnv("HOME=/tmp/test") {
				if strings.HasPrefix(e, "BD_DISABLE_METRICS=") {
					got = append(got, e)
				}
			}
			if len(got) != 1 || got[0] != "BD_DISABLE_METRICS=1" {
				t.Errorf("CleanGTEnv BD_DISABLE_METRICS entries = %q, want exactly [BD_DISABLE_METRICS=1]", got)
			}
		})
	}
}
