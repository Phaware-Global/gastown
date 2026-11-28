//go:build !windows

package reviewer

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// townVars lead to the town's tmux server or the production Dolt server.
var townVars = []string{
	"TMUX", "TMUX_PANE", "GT_TOWN_SOCKET", "GT_TMUX_SOCKET", "GT_TOWN_ROOT", "GT_ROOT",
	"GT_DOLT_HOST", "BEADS_DOLT_HOST", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_DATABASE",
	"BEADS_DIR", "BEADS_DB", "BD_DB", "BEADS_SHARED_SERVER_DIR", "OLDPWD",
}

// forcedVars are set to the dead port.
var forcedVars = []string{"GT_DOLT_PORT", "BEADS_DOLT_PORT", "BEADS_DOLT_SERVER_PORT", "GT_DOLT_FORCE_PORT"}

// envSeenByPRCommand returns the environment a command run by RunPRCommand starts
// with. The town variables are set to a marker beforehand.
func envSeenByPRCommand(t *testing.T) map[string]string {
	t.Helper()
	for _, name := range slices.Concat([]string{"TMUX_TMPDIR", "BEADS_DOLT_AUTO_START"}, forcedVars, townVars) {
		t.Setenv(name, "inherited")
	}
	t.Setenv("PR_CODE_NEEDS_THIS", "kept")
	var out bytes.Buffer
	script := `for n in TMUX_TMPDIR BEADS_DOLT_AUTO_START PR_CODE_NEEDS_THIS GT_PR_CHECKOUT PWD ` +
		strings.Join(slices.Concat(forcedVars, townVars), " ") + `; do eval "if [ -n \"\${$n+x}\" ]; then echo $n=\$$n; fi"; done
		test -d "$TMUX_TMPDIR" && echo DIR_EXISTS=yes`
	code, err := RunPRCommand([]string{"sh", "-c", script}, nil, &out, &out)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v output=%s", code, err, out.String())
	}
	env := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		name, value, _ := strings.Cut(line, "=")
		env[name] = value
	}
	return env
}

func TestRunPRCommandKeepsTheTownOutOfReach(t *testing.T) {
	env := envSeenByPRCommand(t)
	for _, name := range townVars {
		if _, set := env[name]; set {
			t.Errorf("%s reaches the command", name)
		}
	}
	for _, name := range forcedVars {
		if env[name] != deadDoltPort {
			t.Errorf("%s = %q, want the dead port", name, env[name])
		}
	}
	if env["BEADS_DOLT_AUTO_START"] != "0" {
		t.Errorf("BEADS_DOLT_AUTO_START = %q, want 0", env["BEADS_DOLT_AUTO_START"])
	}
	if env["PR_CODE_NEEDS_THIS"] != "kept" {
		t.Error("an unrelated variable was removed")
	}
}

func TestRunPRCommandUsesAPrivateTmuxDirAndRemovesIt(t *testing.T) {
	env := envSeenByPRCommand(t)
	dir := env["TMUX_TMPDIR"]
	if filepath.Dir(dir) != "/tmp" || len(dir) > 32 || env["DIR_EXISTS"] != "yes" {
		t.Fatalf("TMUX_TMPDIR = %q, want a fresh short directory under /tmp", dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("%s is left behind", dir)
	}
	if other := envSeenByPRCommand(t)["TMUX_TMPDIR"]; other == dir {
		t.Error("two commands shared one directory")
	}
}

func TestRunPRCommandReportsTheExitCode(t *testing.T) {
	code, err := RunPRCommand([]string{"sh", "-c", "exit 7"}, nil, nil, nil)
	if err != nil || code != 7 {
		t.Errorf("code=%d err=%v, want 7", code, err)
	}
	if _, err := RunPRCommand([]string{"gt-no-such-command"}, nil, nil, nil); err == nil {
		t.Error("a command that cannot start reported no error")
	}
}

// A command starts outside the checkout, so nothing found from the working
// directory leads to the town, and is told where the checkout is.
func TestRunPRCommandStartsOutsideTheCheckout(t *testing.T) {
	checkout, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	env := envSeenByPRCommand(t)
	if env["GT_PR_CHECKOUT"] != checkout {
		t.Errorf("GT_PR_CHECKOUT = %q, want %q", env["GT_PR_CHECKOUT"], checkout)
	}
	if env["PWD"] != env["TMUX_TMPDIR"] {
		t.Errorf("PWD = %q, want the private directory %q", env["PWD"], env["TMUX_TMPDIR"])
	}
	var out bytes.Buffer
	code, err := RunPRCommand([]string{"pwd", "-P"}, nil, &out, &out)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v output=%s", code, err, out.String())
	}
	if dir := strings.TrimSpace(out.String()); !strings.Contains(dir, "/tmp/gt-pr-") {
		t.Errorf("the command ran in %q, want a private temporary directory", dir)
	}
}
