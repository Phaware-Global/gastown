//go:build !windows

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/plugin"
)

// testScriptPlugin writes a script plugin whose run.sh is body.
func testScriptPlugin(t *testing.T, body, timeout string) *plugin.Plugin {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(body), 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return &plugin.Plugin{
		Name:         "test-script",
		Path:         dir,
		Location:     plugin.LocationTown,
		HasRunScript: true,
		Execution:    &plugin.Execution{Type: plugin.ExecTypeScript, Timeout: timeout},
	}
}

func TestRunScriptPlugin_RunsRunShInPluginDir(t *testing.T) {
	d := testHandlerDaemon(t, t.TempDir())
	d.ctx = context.Background()
	p := testScriptPlugin(t, "pwd -P > ran\n", "")

	if failure := d.runScriptPlugin(p); failure != "" {
		t.Fatalf("runScriptPlugin reported %q for a script that exits 0", failure)
	}

	got, err := os.ReadFile(filepath.Join(p.Path, "ran"))
	if err != nil {
		t.Fatalf("run.sh did not run: %v", err)
	}
	want, _ := filepath.EvalSymlinks(p.Path)
	if strings.TrimSpace(string(got)) != want {
		t.Errorf("run.sh ran in %q, want %q", strings.TrimSpace(string(got)), want)
	}
}

func TestRunScriptPlugin_TimeoutKillsTheScriptsChildren(t *testing.T) {
	d := testHandlerDaemon(t, t.TempDir())
	d.ctx = context.Background()
	// The child holds the output pipe open; killing only bash would leave
	// CombinedOutput waiting on it for the whole sleep.
	p := testScriptPlugin(t, "sleep 30 &\nwait\n", "1s")

	start := time.Now()
	failure := d.runScriptPlugin(p)
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Errorf("runScriptPlugin returned after %v; the 1s timeout should have stopped the script and its children", elapsed)
	}
	if !strings.HasPrefix(failure, "timed out after 1s") {
		t.Errorf("runScriptPlugin reported %q, want a timeout", failure)
	}
}

func TestRunScriptPlugin_ReportsANonZeroExitWithTheOutputTail(t *testing.T) {
	d := testHandlerDaemon(t, t.TempDir())
	d.ctx = context.Background()
	p := testScriptPlugin(t, "echo build broke\nexit 3\n", "")

	failure := d.runScriptPlugin(p)
	if !strings.HasPrefix(failure, "failed: exit status 3") || !strings.Contains(failure, "build broke") {
		t.Errorf("runScriptPlugin reported %q, want the exit status and the output tail", failure)
	}
}

func TestRunsInDaemon_OnlyTownLevelScriptPlugins(t *testing.T) {
	script := &plugin.Execution{Type: plugin.ExecTypeScript}
	tests := []struct {
		name string
		p    plugin.Plugin
		want bool
	}{
		{"town script", plugin.Plugin{Location: plugin.LocationTown, HasRunScript: true, Execution: script}, true},
		{"rig script", plugin.Plugin{Location: plugin.LocationRig, HasRunScript: true, Execution: script}, false},
		{"town agent", plugin.Plugin{Location: plugin.LocationTown, HasRunScript: true}, false},
	}
	for _, tc := range tests {
		if got := runsInDaemon(&tc.p); got != tc.want {
			t.Errorf("%s: runsInDaemon = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStartScriptPlugin_SkipsAPluginAlreadyRunning(t *testing.T) {
	d := testHandlerDaemon(t, t.TempDir())
	d.ctx = context.Background()
	p := testScriptPlugin(t, "touch ran\n", "")
	d.scriptPluginsRunning.Store(p.Name, struct{}{})

	// A nil recorder is fine: a skipped start returns before recording.
	d.startScriptPlugin(p, nil)
	d.scriptPluginWG.Wait()

	if _, err := os.Stat(filepath.Join(p.Path, "ran")); !os.IsNotExist(err) {
		t.Errorf("run.sh ran while a run of the same plugin was in flight (stat err %v)", err)
	}
}

func TestScriptPluginTimeout(t *testing.T) {
	tests := []struct {
		timeout string
		want    time.Duration
	}{
		{"5m", 5 * time.Minute},
		{"", defaultScriptPluginTimeout},
		{"soon", defaultScriptPluginTimeout},
		{"-1m", defaultScriptPluginTimeout},
	}
	for _, tc := range tests {
		p := &plugin.Plugin{Execution: &plugin.Execution{Timeout: tc.timeout}}
		if got := scriptPluginTimeout(p); got != tc.want {
			t.Errorf("scriptPluginTimeout(%q) = %v, want %v", tc.timeout, got, tc.want)
		}
	}
}
