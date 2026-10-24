package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/util"
)

// defaultScriptPluginTimeout bounds a script plugin whose [execution] sets no
// usable timeout.
const defaultScriptPluginTimeout = 10 * time.Minute

// scriptPluginStopGrace is how long a timed-out or canceled script plugin
// gets between SIGTERM and SIGKILL, so git and make can remove their lock
// files. It must stay under shutdown's 30s wait for script plugins.
const scriptPluginStopGrace = 15 * time.Second

// scriptPluginOutputLines is how much of a run's output reaches the daemon
// log and a failure receipt.
const scriptPluginOutputLines = 20

// runsInDaemon reports whether the daemon runs p itself instead of
// dispatching it to a dog. Only town-level plugins qualify: a rig's plugins/
// directory comes from that rig's repo, and the daemon's session holds the
// keychain and the daemon.env secrets.
func runsInDaemon(p *plugin.Plugin) bool {
	return p.IsScript() && p.Location == plugin.LocationTown
}

// startScriptPlugin runs an `[execution] type = "script"` plugin's run.sh in
// the background, from the daemon's own process tree.
//
// That is the point of the type, not just a saving of a dog session. The
// daemon is the town's launchd job, so what it runs is in the login session
// and keeps its keychain and privacy grants. A dog's shell may not: an
// opencode dog's commands run inside the shared opencode service, which is in
// whatever session first started it, and when that was an ssh login
// rebuild-gt's codesign failed with errSecInternalComponent on every run.
func (d *Daemon) startScriptPlugin(p *plugin.Plugin, recorder *plugin.Recorder) {
	if _, running := d.scriptPluginsRunning.LoadOrStore(p.Name, struct{}{}); running {
		return
	}

	// Record the run before it starts, as for a dog dispatch, so the cooldown
	// gate holds even when the script writes no receipt of its own.
	if _, err := recorder.RecordRun(plugin.PluginRunRecord{
		PluginName: p.Name,
		Result:     plugin.ResultSuccess,
		Body:       "Run by the daemon",
	}); err != nil {
		d.logger.Printf("Handler: failed to record run for plugin %s: %v", p.Name, err)
	}

	d.logger.Printf("Handler: running script plugin %s", p.Name)
	d.scriptPluginWG.Add(1)
	go func() {
		defer d.scriptPluginWG.Done()
		defer d.scriptPluginsRunning.Delete(p.Name)
		if failure, stopped := d.runScriptPlugin(p); failure != "" {
			d.reportScriptPluginFailure(p, recorder, failure, !stopped)
		}
	}()
}

// runScriptPlugin runs p's run.sh to completion, timeout or daemon shutdown,
// logs the outcome, and returns a description of the failure ("" on success).
// stopped is true when daemon shutdown ended the run.
func (d *Daemon) runScriptPlugin(p *plugin.Plugin) (failure string, stopped bool) {
	timeout := scriptPluginTimeout(p)
	ctx, cancel := context.WithTimeout(d.ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "run.sh")
	cmd.Dir = p.Path
	// A timeout or shutdown stops make and go too, not just bash, and gives
	// them scriptPluginStopGrace to clean up first.
	util.SetGracefulProcessGroup(cmd, scriptPluginStopGrace)
	cmd.WaitDelay = scriptPluginStopGrace + 5*time.Second
	out, err := cmd.CombinedOutput()

	tail := lastLines(string(out), scriptPluginOutputLines)
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		failure = fmt.Sprintf("timed out after %v", timeout)
	case ctx.Err() != nil:
		failure, stopped = "stopped by daemon shutdown", true
	case err != nil:
		failure = fmt.Sprintf("failed: %v", err)
	}
	if failure == "" {
		d.logger.Printf("Handler: script plugin %s finished:\n%s", p.Name, tail)
		return "", false
	}
	d.logger.Printf("Handler: script plugin %s %s:\n%s", p.Name, failure, tail)
	return failure + "\n\n" + tail, stopped
}

// reportScriptPluginFailure records a failure receipt for p and, when the
// plugin asks for it and escalate is set, escalates. The script may have
// reported the failure itself, but a killed or aborted script cannot, and the
// receipt recorded at start says success. A run cut short by daemon shutdown
// is not escalated: the operator stopped it, and the escalation would race
// the daemon's exit.
func (d *Daemon) reportScriptPluginFailure(p *plugin.Plugin, recorder *plugin.Recorder, failure string, escalate bool) {
	if _, err := recorder.RecordRun(plugin.PluginRunRecord{
		PluginName: p.Name,
		Result:     plugin.ResultFailure,
		Body:       failure,
	}); err != nil {
		d.logger.Printf("Handler: failed to record failure for plugin %s: %v", p.Name, err)
	}

	if !escalate || p.Execution == nil || !p.Execution.NotifyOnFailure {
		return
	}
	severity := p.Execution.Severity
	if severity == "" {
		severity = "medium"
	}
	summary, _, _ := strings.Cut(failure, "\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The fingerprint keeps a failure that repeats every cooldown to one open
	// escalation until someone closes it.
	cmd := exec.CommandContext(ctx, "gt", "escalate", "-s", severity, //nolint:gosec // G204: plugin name and severity come from a town-level plugin.md
		"--source", "plugin:"+p.Name, "--fingerprint", "plugin-failure:"+p.Name,
		fmt.Sprintf("Plugin FAILED: %s", p.Name), "--reason", fmt.Sprintf("Script plugin %s %s (run by the daemon).", p.Name, summary))
	cmd.Dir = d.config.TownRoot
	cmd.Env = append(os.Environ(), "BD_ACTOR=daemon")
	util.SetDetachedProcessGroup(cmd)
	if output, err := cmd.CombinedOutput(); err != nil {
		d.logger.Printf("Handler: escalation for plugin %s failed: %v (%s)", p.Name, err, strings.TrimSpace(string(output)))
	}
}

// scriptPluginTimeout is p's [execution] timeout, or the default when it is
// unset or unparseable.
func scriptPluginTimeout(p *plugin.Plugin) time.Duration {
	if p.Execution != nil {
		if t, err := time.ParseDuration(p.Execution.Timeout); err == nil && t > 0 {
			return t
		}
	}
	return defaultScriptPluginTimeout
}

// lastLines returns at most n trailing lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
