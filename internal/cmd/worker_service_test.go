package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/workerclient"
)

// fakeWorkerClient puts an executable gt-worker-client on PATH so the real
// binary lookup resolves in tests (the lookup itself is what we want covered:
// a plan that points at a missing binary installs a job that never runs).
func fakeWorkerClient(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "gt-worker-client")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0755))
	// The agent's gt/bd are this binary under those names, so the planner
	// requires it too.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gt-proxy-client"), []byte("#!/bin/sh\nexit 0\n"), 0755))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return bin
}

// enrolledTLSDir fakes the material `gt-worker-client enroll` writes.
func enrolledTLSDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{workerclient.MachineCertFile, workerclient.MachineKeyFile, workerclient.ClientCAFile} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte("x"), 0600))
	}
	return dir
}

// TestPlanWorkerService_Preflight pins that misconfiguration fails BEFORE the
// job is installed. A launchd job with missing TLS material loads happily and
// then refuses every connection — which the operator discovers from a provision
// error on a different machine, hours later.
func TestPlanWorkerService_Preflight(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd job planning is macOS-only")
	}

	fakeWorkerClient(t)

	t.Run("proxy url required", func(t *testing.T) {
		_, err := planWorkerService(workerServiceOpts{Listen: "0.0.0.0:9878"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--proxy-url")
	})

	t.Run("tcp requires tls material", func(t *testing.T) {
		_, err := planWorkerService(workerServiceOpts{
			Listen: "0.0.0.0:9878", ProxyURL: "https://orch.local:9876", StateDir: t.TempDir()})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "requires --tls-dir")
	})

	t.Run("unenrolled tls dir", func(t *testing.T) {
		_, err := planWorkerService(workerServiceOpts{
			Listen: "0.0.0.0:9878", ProxyURL: "https://orch.local:9876",
			StateDir: t.TempDir(), TLSDir: t.TempDir()})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "enroll this machine first")
	})

	t.Run("agent env file must exist", func(t *testing.T) {
		_, err := planWorkerService(workerServiceOpts{
			Listen: "unix:///tmp/gtw.sock", ProxyURL: "https://orch.local:9876",
			StateDir: t.TempDir(), AgentEnvFile: filepath.Join(t.TempDir(), "nope.env")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "agent-env-file")
	})
}

// TestPlanWorkerService_TCPJob checks the argv a real TCP worker gets.
func TestPlanWorkerService_TCPJob(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd job planning is macOS-only")
	}
	fakeWorkerClient(t)
	tlsDir := enrolledTLSDir(t)
	envFile := filepath.Join(t.TempDir(), "agent.env")
	require.NoError(t, os.WriteFile(envFile, []byte("ANTHROPIC_API_KEY=sk-x\n"), 0600))
	stateDir := t.TempDir()

	plan, err := planWorkerService(workerServiceOpts{
		Listen: "0.0.0.0:9878", ProxyURL: "https://orch.local:9876",
		StateDir: stateDir, WorkerName: "mac-mini-1", AgentEnvFile: envFile,
		TLSDir: tlsDir, ExecModes: "native,container", MaxSessions: 2,
	})
	require.NoError(t, err)

	argv := strings.Join(plan.Args, " ")
	assert.Contains(t, argv, "-worker-id mac-mini-1")
	assert.Contains(t, argv, "-tls-cert "+filepath.Join(tlsDir, workerclient.MachineCertFile))
	assert.Contains(t, argv, "-tls-client-ca "+filepath.Join(tlsDir, workerclient.ClientCAFile))
	assert.Contains(t, argv, "-agent-env-file "+envFile)
	assert.Contains(t, argv, "-max-sessions 2")
	// container in exec-modes implies a usable docker daemon.
	assert.Contains(t, argv, "-docker")
	// The token is NEVER an argument — it comes from worker.env via the job.
	assert.NotContains(t, argv, "-token")
	assert.Contains(t, string(plan.Plist), "worker.env")

	// A native-only worker must not claim docker.
	nativeOnly, err := planWorkerService(workerServiceOpts{
		Listen: "0.0.0.0:9878", ProxyURL: "https://orch.local:9876",
		StateDir: stateDir, TLSDir: tlsDir, ExecModes: "native"})
	require.NoError(t, err)
	assert.NotContains(t, strings.Join(nativeOnly.Args, " "), "-docker")
}

// shellLineFromPlist returns the /bin/sh -c line launchd will actually run,
// decoded by plutil — so these tests assert on the real thing rather than on
// escaped bytes.
func shellLineFromPlist(t *testing.T, plist []byte) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "com.gastown.worker.plist")
	require.NoError(t, os.WriteFile(f, plist, 0644))
	out, err := exec.Command("plutil", "-extract", "ProgramArguments.2", "raw", "-o", "-", f).CombinedOutput()
	require.NoError(t, err, "plutil could not read the job: %s", strings.TrimSpace(string(out)))
	return strings.TrimSpace(string(out))
}

// TestPlanWorkerService_QuotesPathsWithSpaces pins quoting on the path that
// matters most: the DEFAULT state dir is "~/Library/Application Support/
// gt-worker", and unquoted the worker would be handed "-state-dir …/Application".
func TestPlanWorkerService_QuotesPathsWithSpaces(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd job planning is macOS-only")
	}
	fakeWorkerClient(t)
	spaced := filepath.Join(t.TempDir(), "Application Support", "gt-worker")
	require.NoError(t, os.MkdirAll(spaced, 0700))

	plan, err := planWorkerService(workerServiceOpts{
		Listen: "unix:///tmp/gtw.sock", ProxyURL: "http://127.0.0.1:9876", StateDir: spaced})
	require.NoError(t, err)

	line := shellLineFromPlist(t, plan.Plist)
	assert.Contains(t, line, "-state-dir '"+spaced+"'", "the path must survive as ONE shell word")
}

// TestPlanWorkerService_ShellLineIsInjectionSafe pins that operator-supplied
// paths cannot execute at job start. Every part of the line is single-quoted, so
// a $(…) in a path is inert text — and the line still parses as valid shell.
func TestPlanWorkerService_ShellLineIsInjectionSafe(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd job planning is macOS-only")
	}
	fakeWorkerClient(t)
	marker := filepath.Join(t.TempDir(), "pwned")
	stateDir := filepath.Join(t.TempDir(), `we"ird $(touch `+marker+`)`)
	require.NoError(t, os.MkdirAll(stateDir, 0700))

	plan, err := planWorkerService(workerServiceOpts{
		Listen: "unix:///tmp/gtw.sock", ProxyURL: "http://127.0.0.1:9876", StateDir: stateDir})
	require.NoError(t, err)

	line := shellLineFromPlist(t, plan.Plist)
	// Quoted, therefore inert — not absent: a path is allowed to contain these
	// characters, it just must not be interpreted.
	assert.Contains(t, line, "'"+stateDir+"'")

	// Valid shell, and running it does NOT fire the embedded command. The exec
	// target is a stub that exits 0, so the line runs to completion.
	syntax, err := exec.Command("/bin/sh", "-n", "-c", line).CombinedOutput()
	require.NoError(t, err, "the rendered line is not valid shell: %s", strings.TrimSpace(string(syntax)))
	_ = exec.Command("/bin/sh", "-c", line).Run()
	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr), "the path's $(…) must never execute")
}

// TestPlanWorkerService_UnixJobNeedsNoTLS pins that the unix path is usable
// without enrollment: fs permissions plus the token are its trust model (§3.3).
func TestPlanWorkerService_UnixJobNeedsNoTLS(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd job planning is macOS-only")
	}
	fakeWorkerClient(t)
	plan, err := planWorkerService(workerServiceOpts{
		Listen: "unix:///tmp/gtw.sock", ProxyURL: "http://127.0.0.1:9876", StateDir: t.TempDir()})
	require.NoError(t, err)
	assert.NotContains(t, strings.Join(plan.Args, " "), "-tls-cert")
}

// TestPlanWorkerService_StateDirIsAbsolute pins that a relative --state-dir is
// resolved: launchd runs the job from a different working directory, so a
// relative path would put session state somewhere unpredictable.
func TestPlanWorkerService_StateDirIsAbsolute(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd job planning is macOS-only")
	}
	fakeWorkerClient(t)
	plan, err := planWorkerService(workerServiceOpts{
		Listen: "unix:///tmp/gtw.sock", ProxyURL: "http://127.0.0.1:9876", StateDir: "relative/state"})
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(plan.StateDir), plan.StateDir)
}

// TestInstallBinDir_PointsGtAndBdAtTheProxyClient pins how a remote agent
// reaches the control plane: `gt` and `bd` are gt-proxy-client under those
// names, in the WORKER's own bin dir — not the gt install dir, where on a
// single-box setup the real gt lives and must stay.
func TestInstallBinDir_PointsGtAndBdAtTheProxyClient(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd job planning is macOS-only")
	}
	fakeWorkerClient(t)
	stateDir := t.TempDir()

	plan, err := planWorkerService(workerServiceOpts{
		Listen: "unix:///tmp/gtw.sock", ProxyURL: "http://127.0.0.1:9876", StateDir: stateDir})
	require.NoError(t, err)
	require.NoError(t, installBinDir(plan))

	for _, name := range []string{"gt", "bd"} {
		link := filepath.Join(stateDir, "bin", name)
		target, err := os.Readlink(link)
		require.NoError(t, err, "%s must be a symlink", name)
		// RELATIVE, into the same dir: the shims follow a pushed
		// gt-proxy-client with nothing else to update, and the directory can
		// move.
		assert.Equal(t, "gt-proxy-client", target)
		resolved, err := filepath.EvalSymlinks(link)
		require.NoError(t, err, "%s must resolve to a real file", name)
		// macOS /var is itself a symlink to /private/var; compare resolved.
		wantTarget, err := filepath.EvalSymlinks(plan.ProxyClient)
		require.NoError(t, err)
		assert.Equal(t, wantTarget, resolved)
	}

	// The binaries are COPIES in the worker's dir: push_binaries replaces these
	// files, and a symlink into the install dir would make a pushed binary
	// inert (and would need write access the worker should not have).
	for _, name := range []string{"gt-worker-client", "gt-proxy-client"} {
		fi, err := os.Lstat(filepath.Join(stateDir, "bin", name))
		require.NoError(t, err)
		assert.Zero(t, fi.Mode()&os.ModeSymlink, "%s must be a copy, not a link", name)
		assert.Equal(t, os.FileMode(0755), fi.Mode().Perm())
	}

	// And the job must RUN the copy, or a pushed upgrade would never take.
	assert.Equal(t, filepath.Join(stateDir, "bin", "gt-worker-client"), plan.Binary)

	// The shim dir must come FIRST, or a single-box worker's agent would get the
	// real gt instead of the proxy shim.
	assert.True(t, strings.HasPrefix(plan.Path, filepath.Join(stateDir, "bin")+":"), plan.Path)
	assert.Contains(t, string(plan.Plist), filepath.Join(stateDir, "bin")+":")
}

// TestInstallBinDir_ReplacesStaleLinks pins that re-installing repoints existing
// shims: a shim left pointing at an old path fails at agent runtime, which is
// worse than no shim at all.
func TestInstallBinDir_ReplacesStaleLinks(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd job planning is macOS-only")
	}
	fakeWorkerClient(t)
	stateDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(stateDir, "bin"), 0755))
	require.NoError(t, os.Symlink("/nonexistent/old-proxy-client", filepath.Join(stateDir, "bin", "gt")))

	plan, err := planWorkerService(workerServiceOpts{
		Listen: "unix:///tmp/gtw.sock", ProxyURL: "http://127.0.0.1:9876", StateDir: stateDir})
	require.NoError(t, err)
	require.NoError(t, installBinDir(plan))

	target, err := os.Readlink(filepath.Join(stateDir, "bin", "gt"))
	require.NoError(t, err)
	assert.Equal(t, "gt-proxy-client", target)
}

// TestPlanWorkerService_RequiresProxyClient pins that a worker missing
// gt-proxy-client fails at install: the agent would start and then be unable to
// call gt done at all.
func TestPlanWorkerService_RequiresProxyClient(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd job planning is macOS-only")
	}
	// A PATH with gt-worker-client but NO gt-proxy-client.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gt-worker-client"), []byte("#!/bin/sh\n"), 0755))
	t.Setenv("PATH", dir)

	_, err := planWorkerService(workerServiceOpts{
		Listen: "unix:///tmp/gtw.sock", ProxyURL: "http://127.0.0.1:9876", StateDir: t.TempDir()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gt-proxy-client not found")
}

func TestWorkerServicePath(t *testing.T) {
	got := workerServicePath("/opt/gt/bin/gt-worker-client")
	parts := strings.Split(got, ":")
	assert.Equal(t, "/opt/gt/bin", parts[0], "the gt install dir must come first")
	assert.Contains(t, parts, "/usr/bin", "the worker execs git and the agent binary")
	seen := map[string]bool{}
	for _, p := range parts {
		require.False(t, seen[p], "duplicate PATH entry %q", p)
		seen[p] = true
	}
}

func TestDefaultWorkerStateDir(t *testing.T) {
	d, err := defaultWorkerStateDir()
	require.NoError(t, err)
	// The binary's /var/lib default is a Linux path a user-level launchd job
	// cannot create.
	assert.NotEqual(t, "/var/lib/gt-worker", d)
	assert.True(t, strings.HasSuffix(d, filepath.Join("Library", "Application Support", "gt-worker")), d)
}
