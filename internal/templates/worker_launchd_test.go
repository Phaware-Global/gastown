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
		BinaryPath: "/usr/local/bin/gt-worker-client",
		StateDir:   "/Users/op/Library/Application Support/gt-worker",
		Args:       []string{"-listen", "'0.0.0.0:9878'", "-proxy-url", "'https://orch.local:9876'"},
		Path:       "/usr/local/bin:/usr/bin:/bin",
	})
	require.NoError(t, err)
	got := string(out)

	assert.Contains(t, got, "<string>com.gastown.worker</string>")
	assert.Contains(t, got, "exec \"/usr/local/bin/gt-worker-client\" -listen '0.0.0.0:9878'")
	// Secrets reach the worker through a sourced 0600 file, never argv.
	assert.Contains(t, got, `. "/Users/op/Library/Application Support/gt-worker/worker.env"`)
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
func TestRenderWorkerLaunchd_IsValidPlist(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("plutil is macOS-only")
	}
	out, err := RenderWorkerLaunchd(WorkerSupervisorData{
		BinaryPath: "/usr/local/bin/gt-worker-client",
		StateDir:   t.TempDir(),
		Args:       []string{"-listen", "'unix:///tmp/w.sock'"},
		Path:       "/usr/bin:/bin",
	})
	require.NoError(t, err)

	f := filepath.Join(t.TempDir(), "com.gastown.worker.plist")
	require.NoError(t, os.WriteFile(f, out, 0644))
	cmd := exec.Command("plutil", "-lint", f)
	res, err := cmd.CombinedOutput()
	require.NoError(t, err, "plutil rejected the rendered plist: %s", strings.TrimSpace(string(res)))
}

func TestWorkerLaunchdPlistPath(t *testing.T) {
	p, err := WorkerLaunchdPlistPath()
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(p, "Library/LaunchAgents/com.gastown.worker.plist"), p)
}
