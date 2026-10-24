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

	if failure, _ := d.runScriptPlugin(p); failure != "" {
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
	failure, stopped := d.runScriptPlugin(p)
	if stopped {
		t.Error("runScriptPlugin reported a timeout as a shutdown stop")
	}
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

	failure, _ := d.runScriptPlugin(p)
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

func TestRunScriptPlugin_ShutdownIsReportedAsStopped(t *testing.T) {
	d := testHandlerDaemon(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	d.ctx = ctx
	p := testScriptPlugin(t, "sleep 30 &\nwait\n", "")

	time.AfterFunc(300*time.Millisecond, cancel)
	failure, stopped := d.runScriptPlugin(p)
	if !stopped || !strings.HasPrefix(failure, "stopped by daemon shutdown") {
		t.Errorf("runScriptPlugin = (%q, %v), want a shutdown stop", failure, stopped)
	}
}

// stubBdAndGt puts bd and gt stubs first on PATH. gt appends its argv to the
// returned file, one argument per line, with a blank line after each call.
func stubBdAndGt(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	calls := filepath.Join(binDir, "gt-calls")
	stubs := map[string]string{
		"bd": "#!/bin/sh\necho '[]'\n",
		"gt": "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done >> \"" + calls + "\"\necho >> \"" + calls + "\"\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func TestReportScriptPluginFailure_EscalatesOnceKeyedByPlugin(t *testing.T) {
	calls := stubBdAndGt(t)
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)
	p := testScriptPlugin(t, "", "")
	p.Execution.NotifyOnFailure = true
	p.Execution.Severity = "high"

	d.reportScriptPluginFailure(p, plugin.NewRecorder(townRoot), "timed out after 5m\n\noutput", true)

	got, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("gt escalate was not called: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(got)), "\n")
	want := []string{"escalate", "-s", "high", "--source", "plugin:test-script", "--fingerprint", "plugin-failure:test-script", "Plugin FAILED: test-script", "--reason", "Script plugin test-script timed out after 5m (run by the daemon)."}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Errorf("gt argv = %q\nwant      %q", args, want)
	}
}

func TestReportScriptPluginFailure_NoEscalationWhenStoppedOrNotAsked(t *testing.T) {
	tests := []struct {
		name     string
		notify   bool
		escalate bool
	}{
		{"stopped by shutdown", true, false},
		{"notify_on_failure unset", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := stubBdAndGt(t)
			townRoot := t.TempDir()
			d := testHandlerDaemon(t, townRoot)
			p := testScriptPlugin(t, "", "")
			p.Execution.NotifyOnFailure = tc.notify

			d.reportScriptPluginFailure(p, plugin.NewRecorder(townRoot), "failed", tc.escalate)

			if _, err := os.Stat(calls); !os.IsNotExist(err) {
				t.Errorf("gt was called (stat err %v); want no escalation", err)
			}
		})
	}
}

// TestStartScriptPlugin_EscalatesATimeoutButNotAShutdown drives the whole
// path, run through report: a timeout escalates, a daemon shutdown does not.
func TestStartScriptPlugin_EscalatesATimeoutButNotAShutdown(t *testing.T) {
	tests := []struct {
		name         string
		timeout      string
		shutdown     bool
		wantEscalate bool
	}{
		{"timeout", "1s", false, true},
		{"shutdown", "", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := stubBdAndGt(t)
			townRoot := t.TempDir()
			d := testHandlerDaemon(t, townRoot)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d.ctx = ctx
			p := testScriptPlugin(t, "sleep 30 &\nwait\n", tc.timeout)
			p.Execution.NotifyOnFailure = true

			d.startScriptPlugin(p, plugin.NewRecorder(townRoot))
			if tc.shutdown {
				time.AfterFunc(300*time.Millisecond, cancel)
			}
			d.scriptPluginWG.Wait()

			_, err := os.Stat(calls)
			if escalated := err == nil; escalated != tc.wantEscalate {
				t.Errorf("escalated = %v, want %v", escalated, tc.wantEscalate)
			}
		})
	}
}
