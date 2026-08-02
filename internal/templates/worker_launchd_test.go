package templates

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderWorkerLaunchd(t *testing.T) {
	out, err := RenderWorkerLaunchd(WorkerSupervisorData{
		ShellCommand: `set -a; [ -r '/state/worker.env' ] && . '/state/worker.env'; set +a; exec '/usr/local/bin/gt-worker-client' -listen '0.0.0.0:9878'`,
		StateDir:     "/Users/op/Library/Application Support/gt-worker",
		LogPath:      "/Users/op/Library/Application Support/gt-worker/worker.log",
		Path:         "/usr/local/bin:/usr/bin:/bin",
	})
	require.NoError(t, err)
	got := string(out)

	assert.Contains(t, got, "<string>com.gastown.worker</string>")
	// The shell line is XML-escaped in the document; what matters is that it
	// decodes back to exactly what the caller assembled.
	if runtime.GOOS == "darwin" {
		assert.Equal(t,
			`set -a; [ -r '/state/worker.env' ] && . '/state/worker.env'; set +a; exec '/usr/local/bin/gt-worker-client' -listen '0.0.0.0:9878'`,
			shellLine(t, out))
	}
	// Secrets reach the worker through a sourced 0600 file, never argv.
	assert.NotContains(t, got, "GT_WORKER_TOKEN=")
	// A worker that stays down gets its sessions reaped, so it must come back.
	assert.Contains(t, got, "<key>KeepAlive</key>\n    <true/>")
	assert.Contains(t, got, "worker.log")
	// Agents are interactive; Background would throttle them.
	assert.Contains(t, got, "<string>Interactive</string>")
}

// TestRenderWorkerLaunchd_IsValidPlist runs the rendered job through plutil,
// because launchd rejects a malformed plist silently from the operator's point
// of view — the job simply never starts.
// shellLine decodes ProgramArguments[2] — the line launchd hands /bin/sh.
func shellLine(t *testing.T, plist []byte) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "com.gastown.worker.plist")
	require.NoError(t, os.WriteFile(f, plist, 0644))
	out, err := exec.Command("plutil", "-extract", "ProgramArguments.2", "raw", "-o", "-", f).CombinedOutput()
	require.NoError(t, err, "plutil could not read the job: %s", strings.TrimSpace(string(out)))
	return strings.TrimSpace(string(out))
}

func TestRenderWorkerLaunchd_IsValidPlist(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("plutil is macOS-only")
	}
	out, err := RenderWorkerLaunchd(WorkerSupervisorData{
		ShellCommand: `exec '/usr/local/bin/gt-worker-client' -listen 'unix:///tmp/w.sock'`,
		StateDir:     t.TempDir(),
		LogPath:      "/tmp/worker.log",
		Path:         "/usr/bin:/bin",
	})
	require.NoError(t, err)

	f := filepath.Join(t.TempDir(), "com.gastown.worker.plist")
	require.NoError(t, os.WriteFile(f, out, 0644))
	cmd := exec.Command("plutil", "-lint", f)
	res, err := cmd.CombinedOutput()
	require.NoError(t, err, "plutil rejected the rendered plist: %s", strings.TrimSpace(string(res)))
}

// TestRenderWorkerLaunchd_EscapesXML pins that operator-supplied paths cannot
// corrupt the document. An & in a path produced a plist launchd silently refuses
// to load — the job just never starts, with nothing to read.
func TestRenderWorkerLaunchd_EscapesXML(t *testing.T) {
	out, err := RenderWorkerLaunchd(WorkerSupervisorData{
		ShellCommand: `exec '/opt/a&b/gt-worker-client' -proxy-url 'https://o/?x=1&y=2'`,
		StateDir:     "/Users/op/a&b<c>/gt-worker",
		LogPath:      "/Users/op/a&b<c>/gt-worker/worker.log",
		Path:         "/opt/a&b:/usr/bin",
	})
	require.NoError(t, err)
	got := string(out)

	assert.NotContains(t, got, "a&b", "a raw ampersand makes the plist invalid XML")
	assert.Contains(t, got, "a&amp;b")
	assert.Contains(t, got, "&lt;c&gt;")

	// And it must still parse as a plist with the ORIGINAL values intact.
	if runtime.GOOS == "darwin" {
		f := filepath.Join(t.TempDir(), "com.gastown.worker.plist")
		require.NoError(t, os.WriteFile(f, out, 0644))
		res, err := exec.Command("plutil", "-lint", f).CombinedOutput()
		require.NoError(t, err, "plutil rejected the escaped plist: %s", strings.TrimSpace(string(res)))

		res, err = exec.Command("plutil", "-extract", "WorkingDirectory", "raw", "-o", "-", f).CombinedOutput()
		require.NoError(t, err)
		assert.Equal(t, "/Users/op/a&b<c>/gt-worker", strings.TrimSpace(string(res)),
			"escaping must round-trip: launchd has to see the real path")
	}
}

func TestWorkerLaunchdPlistPath(t *testing.T) {
	p, err := WorkerLaunchdPlistPath()
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(p, "Library/LaunchAgents/com.gastown.worker.plist"), p)
}
