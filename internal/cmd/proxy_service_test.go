package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProxyServer puts an executable gt-proxy-server on PATH so the real
// binary lookup resolves in tests.
func fakeProxyServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "gt-proxy-server")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0755))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return bin
}

// argAfter returns the argument following flag in args.
func argAfter(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("%s not in %v", flag, args)
	return ""
}

// TestPlanProxyService_Job pins the job an operator gets from the defaults:
// the installed binary run in place, from the town root, with the town root
// and config path spelled out rather than left to the server's own defaults.
func TestPlanProxyService_Job(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd job planning is macOS-only")
	}
	bin := fakeProxyServer(t)
	town := t.TempDir()

	plan, err := planProxyService(proxyServiceOpts{TownRoot: town, Listen: "0.0.0.0:9876"})
	require.NoError(t, err)

	assert.Equal(t, bin, plan.Binary)
	assert.Equal(t, bin, plan.Args[0], "the job runs the installed binary where it is")
	assert.Equal(t, town, argAfter(t, plan.Args, "-town-root"))
	assert.Equal(t, "0.0.0.0:9876", argAfter(t, plan.Args, "-listen"))
	assert.Equal(t, filepath.Join(town, ".runtime", "proxy", "config.json"), argAfter(t, plan.Args, "-config"))
	assert.Equal(t, filepath.Join(town, ".runtime", "proxy", "proxy-server.log"), plan.LogPath)
	assert.True(t, strings.HasSuffix(plan.PlistPath, "Library/LaunchAgents/com.gastown.proxy.plist"), plan.PlistPath)

	got := string(plan.Plist)
	assert.Contains(t, got, "<string>com.gastown.proxy</string>")
	assert.Contains(t, got, "<key>WorkingDirectory</key>\n    <string>"+town+"</string>")
	assert.Contains(t, strings.Split(plan.Path, ":"), filepath.Dir(bin),
		"the proxy execs gt by name, so the install dir must be on its PATH")
}

// TestPlanProxyService_ListenIsOptional: without --listen the server's own
// precedence (config file, then its default) decides, so the flag is absent.
func TestPlanProxyService_ListenIsOptional(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd job planning is macOS-only")
	}
	fakeProxyServer(t)

	plan, err := planProxyService(proxyServiceOpts{TownRoot: t.TempDir()})
	require.NoError(t, err)
	assert.NotContains(t, plan.Args, "-listen")
}

// TestPlanProxyService_Preflight pins that misconfiguration fails before the
// job is installed. A job whose server exits at startup is restarted by
// launchd forever, and the operator finds out from a worker.
func TestPlanProxyService_Preflight(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd job planning is macOS-only")
	}
	fakeProxyServer(t)
	town := t.TempDir()

	t.Run("town root must exist", func(t *testing.T) {
		_, err := planProxyService(proxyServiceOpts{TownRoot: filepath.Join(town, "missing")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--town-root")
	})

	t.Run("listen must be host:port", func(t *testing.T) {
		_, err := planProxyService(proxyServiceOpts{TownRoot: town, Listen: "9876"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--listen")
	})

	t.Run("an explicit config must exist", func(t *testing.T) {
		_, err := planProxyService(proxyServiceOpts{TownRoot: town, Config: filepath.Join(town, "nope.json")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--config")
	})

	t.Run("a config that does not parse is refused", func(t *testing.T) {
		bad := filepath.Join(town, "bad.json")
		require.NoError(t, os.WriteFile(bad, []byte(`{"extra_san_hosts": [`), 0600))
		_, err := planProxyService(proxyServiceOpts{TownRoot: town, Config: bad})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not valid JSON")
	})

	t.Run("the default config is validated when present", func(t *testing.T) {
		dir := filepath.Join(town, ".runtime", "proxy")
		require.NoError(t, os.MkdirAll(dir, 0700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`not json`), 0600))
		_, err := planProxyService(proxyServiceOpts{TownRoot: town})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not valid JSON")
	})
}

// A config can be valid JSON and still be one the server exits on (a string
// where a list belongs, an out-of-range exec_timeout). The server is the only
// authority on that, so the preflight asks it, and a refusal stops the install.
func TestPlanProxyService_AsksTheServerToCheckTheConfig(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd job planning is macOS-only")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "gt-proxy-server")
	// Stands in for the server's -check-config: refuse, and record the call.
	script := "#!/bin/sh\necho \"$@\" > " + filepath.Join(dir, "args") + "\necho 'cannot unmarshal string into Go struct field' >&2\nexit 1\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0755))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	town := t.TempDir()
	cfg := filepath.Join(town, "config.json")
	require.NoError(t, os.WriteFile(cfg, []byte(`{"extra_san_hosts": "orchestrator.local"}`), 0600))

	_, err := planProxyService(proxyServiceOpts{TownRoot: town, Config: cfg})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--config")
	assert.Contains(t, err.Error(), "cannot unmarshal string")

	args, readErr := os.ReadFile(filepath.Join(dir, "args"))
	require.NoError(t, readErr)
	assert.Equal(t, "-config "+cfg+" -check-config", strings.TrimSpace(string(args)))
}

// launchctl accepting the job says nothing about the server staying up: one
// that cannot bind its port exits at once and is restarted forever.
func TestProxyServiceRunning(t *testing.T) {
	running := "gui/501/com.gastown.proxy = {\n\tstate = running\n\tpid = 4242\n\tlast exit code = (never exited)\n}"
	assert.NoError(t, proxyServiceRunning(running))

	for name, out := range map[string]string{
		"exited and waiting to respawn": "gui/501/com.gastown.proxy = {\n\tstate = spawn scheduled\n\tlast exit code = 1\n}",
		"not running":                   "gui/501/com.gastown.proxy = {\n\tstate = not running\n\tlast exit code = 1\n}",
		"no state at all":               "",
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, proxyServiceRunning(out))
		})
	}
}

// The server takes a while to reach its listener (it runs gt to discover the
// allowed subcommands first), so one early sample can read "running" for a
// job that is about to fail and be respawned forever. The check has to watch
// the job across a launchd respawn interval.
func TestProxyServiceStayedUp(t *testing.T) {
	running := func(pid string) string {
		return "\tstate = running\n\tpid = " + pid + "\n"
	}
	samples := func(outs ...string) func() (string, error) {
		i := 0
		return func() (string, error) {
			out := outs[len(outs)-1]
			if i < len(outs) {
				out = outs[i]
			}
			i++
			return out, nil
		}
	}
	noSleep := func(time.Duration) {}

	if err := proxyServiceStayedUp(samples(running("41")), 5, noSleep); err != nil {
		t.Fatalf("a server that stays up: %v", err)
	}
	// launchd reports a job it has spawned but not yet exec'd as xpcproxy,
	// with the pid the server will run as. That is starting, not failed.
	starting := "\tstate = xpcproxy\n\tpid = 41\n"
	if err := proxyServiceStayedUp(samples(starting, running("41")), 5, noSleep); err != nil {
		t.Fatalf("a server seen while it was still starting: %v", err)
	}
	if err := proxyServiceStayedUp(samples(starting), 5, noSleep); err == nil {
		t.Fatal("a job that never gets past starting must be reported")
	}
	if err := proxyServiceStayedUp(samples(starting, running("57")), 5, noSleep); err == nil {
		t.Fatal("a pid change between starting and running must be reported")
	}
	crashed := "\tstate = spawn scheduled\n\tlast exit code = 1\n"
	if err := proxyServiceStayedUp(samples(running("41"), running("41"), crashed), 5, noSleep); err == nil {
		t.Fatal("a server that exits after the first samples must be reported")
	} else if !strings.Contains(err.Error(), "last exit code 1") {
		t.Fatalf("error should carry the exit code, got: %v", err)
	}
	if err := proxyServiceStayedUp(samples(running("41"), running("57")), 5, noSleep); err == nil {
		t.Fatal("a respawn between two running samples (the pid changed) must be reported")
	}
	gone := func() (string, error) { return "Could not find service", fmt.Errorf("exit status 113") }
	if err := proxyServiceStayedUp(gone, 5, noSleep); err == nil {
		t.Fatal("a job launchd no longer reports must be an error")
	}
}

// TestPlanProxyService_RequiresServerBinary: a job pointing at a binary that
// is not there loads and never runs.
func TestPlanProxyService_RequiresServerBinary(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd job planning is macOS-only")
	}
	if self, err := os.Executable(); err == nil {
		if _, err := os.Stat(filepath.Join(filepath.Dir(self), "gt-proxy-server")); err == nil {
			t.Skip("a gt-proxy-server sits next to the test binary")
		}
	}
	t.Setenv("PATH", t.TempDir())

	_, err := planProxyService(proxyServiceOpts{TownRoot: t.TempDir()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gt-proxy-server not found")
}

// TestPlanProxyService_NoCredentialsInJob: proxied commands are started with
// HOME and PATH only, so the job carries PATH and nothing else.
func TestPlanProxyService_NoCredentialsInJob(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd job planning is macOS-only")
	}
	fakeProxyServer(t)
	t.Setenv("GH_TOKEN", "must-not-appear")
	t.Setenv("GT_REVIEWER_GITHUB_TOKEN", "must-not-appear")

	plan, err := planProxyService(proxyServiceOpts{TownRoot: t.TempDir()})
	require.NoError(t, err)
	got := string(plan.Plist)
	assert.NotContains(t, got, "must-not-appear")
	assert.Equal(t, 1, strings.Count(got, "<key>PATH</key>"))
	env := got[strings.Index(got, "<key>EnvironmentVariables</key>"):strings.Index(got, "<key>ProcessType</key>")]
	assert.Equal(t, 2, strings.Count(env, "<key>"), "EnvironmentVariables holds PATH only: %s", env)
}
