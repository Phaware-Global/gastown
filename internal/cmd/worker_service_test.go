package cmd

import (
	"os"
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

// TestPlanWorkerService_QuotesPathsWithSpaces pins quoting on the path that
// matters: the plist embeds the argv inside a `/bin/sh -c` string, and the
// DEFAULT state dir is "~/Library/Application Support/gt-worker" — unquoted,
// the worker would be handed "-state-dir …/Application" and fail at startup.
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

	// The rendered job must carry the path as ONE shell word.
	assert.Contains(t, string(plan.Plist), "-state-dir '"+spaced+"'")
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
