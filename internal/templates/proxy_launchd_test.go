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

// proxyPlistValue extracts one key from a rendered proxy plist with plutil.
func proxyPlistValue(t *testing.T, plist []byte, keyPath string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "com.gastown.proxy.plist")
	require.NoError(t, os.WriteFile(f, plist, 0644))
	out, err := exec.Command("plutil", "-extract", keyPath, "raw", "-o", "-", f).CombinedOutput()
	require.NoError(t, err, "plutil could not read the job: %s", strings.TrimSpace(string(out)))
	return strings.TrimSpace(string(out))
}

func TestRenderProxyLaunchd(t *testing.T) {
	out, err := RenderProxyLaunchd(ProxySupervisorData{
		Args:     []string{"/usr/local/bin/gt-proxy-server", "-town-root", "/Users/op/gt", "-listen", "0.0.0.0:9876"},
		TownRoot: "/Users/op/gt",
		LogPath:  "/Users/op/gt/.runtime/proxy/proxy-server.log",
		Path:     "/usr/local/bin:/usr/bin:/bin",
	})
	require.NoError(t, err)
	got := string(out)

	assert.Contains(t, got, "<string>com.gastown.proxy</string>")
	// A remote polecat has no control plane while the proxy is down.
	assert.Contains(t, got, "<key>KeepAlive</key>\n    <true/>")
	assert.Contains(t, got, "<key>RunAtLoad</key>\n    <true/>")
	assert.Contains(t, got, "proxy-server.log")
	assert.Contains(t, got, "<string>Interactive</string>")

	if runtime.GOOS == "darwin" {
		// launchd runs the argv directly: each element is its own string.
		assert.Equal(t, "/usr/local/bin/gt-proxy-server", proxyPlistValue(t, out, "ProgramArguments.0"))
		assert.Equal(t, "0.0.0.0:9876", proxyPlistValue(t, out, "ProgramArguments.4"))
		assert.Equal(t, "/Users/op/gt", proxyPlistValue(t, out, "WorkingDirectory"))
		assert.Equal(t, "/usr/local/bin:/usr/bin:/bin", proxyPlistValue(t, out, "EnvironmentVariables.PATH"))
	}
}

// TestRenderProxyLaunchd_EscapesXML pins that operator-supplied paths cannot
// corrupt the document, and that an argument with shell metacharacters reaches
// the server as one literal argument: there is no shell in this job.
func TestRenderProxyLaunchd_EscapesXML(t *testing.T) {
	out, err := RenderProxyLaunchd(ProxySupervisorData{
		Args:     []string{"/opt/a&b/gt-proxy-server", "-town-root", "/Users/op/a&b<c>/gt; touch /tmp/x $(id)"},
		TownRoot: "/Users/op/a&b<c>/gt",
		LogPath:  "/Users/op/a&b<c>/gt/proxy-server.log",
		Path:     "/opt/a&b:/usr/bin",
	})
	require.NoError(t, err)
	got := string(out)

	assert.NotContains(t, got, "a&b", "a raw ampersand makes the plist invalid XML")
	assert.Contains(t, got, "a&amp;b")
	assert.Contains(t, got, "&lt;c&gt;")

	if runtime.GOOS == "darwin" {
		f := filepath.Join(t.TempDir(), "com.gastown.proxy.plist")
		require.NoError(t, os.WriteFile(f, out, 0644))
		res, err := exec.Command("plutil", "-lint", f).CombinedOutput()
		require.NoError(t, err, "plutil rejected the escaped plist: %s", strings.TrimSpace(string(res)))

		assert.Equal(t, "/Users/op/a&b<c>/gt", proxyPlistValue(t, out, "WorkingDirectory"),
			"escaping must round-trip: launchd has to see the real path")
		assert.Equal(t, "/Users/op/a&b<c>/gt; touch /tmp/x $(id)", proxyPlistValue(t, out, "ProgramArguments.2"))
		assert.Equal(t, "3", proxyPlistValue(t, out, "ProgramArguments"), "one argument stays one argument")
	}
}

func TestProxyLaunchdPlistPath(t *testing.T) {
	p, err := ProxyLaunchdPlistPath()
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(p, "Library/LaunchAgents/com.gastown.proxy.plist"), p)
}

func TestProxyServicePath_IncludesTheBinaryDir(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	assert.Equal(t, "/opt/gt/bin:/usr/bin:/bin", ProxyServicePath("/opt/gt/bin/gt-proxy-server"))
}
