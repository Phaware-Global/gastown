package daemon

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/util"
)

// defaultScriptPluginTimeout bounds a script plugin whose [execution] sets no
// usable timeout.
const defaultScriptPluginTimeout = 10 * time.Minute

// scriptPluginOutputLines is how much of a run's output reaches the daemon
// log. The script's own receipts and escalations carry the details.
const scriptPluginOutputLines = 20

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
		d.runScriptPlugin(p)
	}()
}

// runScriptPlugin runs p's run.sh to completion or timeout and logs the
// outcome. Escalating a failure is the script's job, as it is when a dog runs
// it: only the script knows which failures matter.
func (d *Daemon) runScriptPlugin(p *plugin.Plugin) {
	ctx, cancel := context.WithTimeout(d.ctx, scriptPluginTimeout(p))
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "run.sh")
	cmd.Dir = p.Path
	util.SetProcessGroup(cmd) // a timeout kills make and go too, not just bash
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()

	tail := lastLines(string(out), scriptPluginOutputLines)
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		d.logger.Printf("Handler: script plugin %s timed out after %v:\n%s", p.Name, scriptPluginTimeout(p), tail)
	case err != nil:
		d.logger.Printf("Handler: script plugin %s failed: %v:\n%s", p.Name, err, tail)
	default:
		d.logger.Printf("Handler: script plugin %s finished:\n%s", p.Name, tail)
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
