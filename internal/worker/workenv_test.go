package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDocker writes a docker stand-in that appends each invocation (one line,
// tab-joined argv) to logFile and consults failFile: if failFile lists a
// subcommand (e.g. "exec"), that invocation exits 1.
func fakeDocker(t *testing.T) (docker, logFile, failFile string) {
	t.Helper()
	dir := t.TempDir()
	docker = filepath.Join(dir, "docker")
	logFile = filepath.Join(dir, "docker.log")
	failFile = filepath.Join(dir, "docker.fail")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logFile + `"
if [ -f "` + failFile + `" ] && grep -q "^$1$" "` + failFile + `"; then
  echo "forced failure for $1" >&2
  exit 1
fi
if [ "$1" = "run" ]; then echo "deadbeefcafe"; fi
exit 0
`
	require.NoError(t, os.WriteFile(docker, []byte(script), 0755))
	return docker, logFile, failFile
}

func dockerCalls(t *testing.T, logFile string) []string {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func testWorkEnvConfig(t *testing.T, docker string) WorkEnvConfig {
	t.Helper()
	return WorkEnvConfig{
		Rig:       "MyRig",
		Polecat:   "furiosa",
		Session:   "gt-MyRig-furiosa",
		Image:     "registry.example.com/dev:latest",
		Worktree:  t.TempDir(),
		GTDir:     t.TempDir(),
		RelayPort: 9899,
		Docker:    docker,
	}
}

func TestNewWorkEnv_Validation(t *testing.T) {
	base := testWorkEnvConfig(t, "docker")

	t.Run("valid defaults to bridge", func(t *testing.T) {
		w, err := NewWorkEnv(base)
		require.NoError(t, err)
		assert.Equal(t, "gt-work-MyRig-furiosa", w.ContainerName())
		assert.Equal(t, "http://host.docker.internal:9899", w.ProxyURL())
	})

	t.Run("host networking proxies via loopback", func(t *testing.T) {
		cfg := base
		cfg.ContainerNetwork = "host"
		w, err := NewWorkEnv(cfg)
		require.NoError(t, err)
		assert.Equal(t, "http://127.0.0.1:9899", w.ProxyURL())
	})

	t.Run("sandboxed plus host networking is refused (§6.1.1)", func(t *testing.T) {
		cfg := base
		cfg.ContainerNetwork = "host"
		cfg.Sandboxed = true
		_, err := NewWorkEnv(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bridge")
	})

	t.Run("missing image / worktree / relay port are refused", func(t *testing.T) {
		for _, mutate := range []func(*WorkEnvConfig){
			func(c *WorkEnvConfig) { c.Image = "" },
			func(c *WorkEnvConfig) { c.Worktree = "" },
			func(c *WorkEnvConfig) { c.GTDir = "" },
			func(c *WorkEnvConfig) { c.RelayPort = 0 },
			func(c *WorkEnvConfig) { c.ContainerNetwork = "overlay" },
		} {
			cfg := base
			mutate(&cfg)
			_, err := NewWorkEnv(cfg)
			assert.Error(t, err)
		}
	})
}

func TestWorkEnvPrepare_RunsIdleContainerWithContract(t *testing.T) {
	docker, logFile, _ := fakeDocker(t)
	cfg := testWorkEnvConfig(t, docker)
	w, err := NewWorkEnv(cfg)
	require.NoError(t, err)

	require.NoError(t, w.Prepare(context.Background()))

	// Injected idle entrypoint exists and is executable.
	fi, err := os.Stat(filepath.Join(cfg.GTDir, "gt-idle.sh"))
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&0111, "idle entrypoint must be executable")

	calls := dockerCalls(t, logFile)
	require.Len(t, calls, 3, "rm -f (stale), run, preflight exec: %v", calls)
	assert.Equal(t, "rm -f gt-work-MyRig-furiosa", calls[0])

	run := calls[1]
	for _, want := range []string{
		"run -d --name gt-work-MyRig-furiosa",
		"--label gt.rig=MyRig",
		"--label gt.polecat=furiosa",
		"--label gt.session=gt-MyRig-furiosa",
		"-v " + cfg.GTDir + ":/opt/gt:ro",
		"-v " + cfg.Worktree + ":/work",
		"-w /work",
		"-e GT_PROXY_URL=http://host.docker.internal:9899",
		"--add-host host.docker.internal:host-gateway",
		cfg.Image + " " + idleEntrypoint,
	} {
		assert.Contains(t, run, want)
	}
	// Bridge mode must NOT use host networking; socket not mounted unless asked.
	assert.NotContains(t, run, "--network host")
	assert.NotContains(t, run, "docker.sock")

	assert.Contains(t, calls[2], "exec gt-work-MyRig-furiosa /bin/sh -c true")
}

func TestWorkEnvPrepare_HostNetworkAndDockerSocket(t *testing.T) {
	docker, logFile, _ := fakeDocker(t)
	cfg := testWorkEnvConfig(t, docker)
	cfg.ContainerNetwork = "host"
	cfg.MountDockerSocket = true
	w, err := NewWorkEnv(cfg)
	require.NoError(t, err)
	require.NoError(t, w.Prepare(context.Background()))

	run := dockerCalls(t, logFile)[1]
	assert.Contains(t, run, "--network host")
	assert.NotContains(t, run, "--add-host")
	assert.Contains(t, run, "-v /var/run/docker.sock:/var/run/docker.sock")
	assert.Contains(t, run, "-e GT_PROXY_URL=http://127.0.0.1:9899")
}

func TestWorkEnvPrepare_AgentPreflight(t *testing.T) {
	docker, logFile, failFile := fakeDocker(t)
	cfg := testWorkEnvConfig(t, docker)
	cfg.AgentBinary = "claude"
	w, err := NewWorkEnv(cfg)
	require.NoError(t, err)

	t.Run("agent on PATH passes and is probed quoted", func(t *testing.T) {
		require.NoError(t, w.Prepare(context.Background()))
		calls := dockerCalls(t, logFile)
		last := calls[len(calls)-1]
		assert.Contains(t, last, "command -v 'claude'")
	})

	t.Run("preflight failure stops before agent launch and removes the container", func(t *testing.T) {
		require.NoError(t, os.WriteFile(failFile, []byte("exec\n"), 0644))
		t.Cleanup(func() { os.Remove(failFile) })
		err := w.Prepare(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "/bin/sh not usable")
		// Last call must be the teardown rm of the failed container.
		calls := dockerCalls(t, logFile)
		assert.Equal(t, "rm -f gt-work-MyRig-furiosa", calls[len(calls)-1])
	})
}

func TestWorkEnvStopAndTeardown(t *testing.T) {
	docker, logFile, _ := fakeDocker(t)
	w, err := NewWorkEnv(testWorkEnvConfig(t, docker))
	require.NoError(t, err)

	require.NoError(t, w.StopWork(context.Background()))
	require.NoError(t, w.Teardown(context.Background()))
	calls := dockerCalls(t, logFile)
	require.Len(t, calls, 2)
	assert.Equal(t, "stop -t 30 gt-work-MyRig-furiosa", calls[0])
	assert.Equal(t, "rm -f gt-work-MyRig-furiosa", calls[1])
}

func TestNewWorkEnv_SandboxedRefusesDockerSocket(t *testing.T) {
	cfg := testWorkEnvConfig(t, "docker")
	cfg.Sandboxed = true
	cfg.MountDockerSocket = true
	_, err := NewWorkEnv(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "docker socket")
}

func TestWorkEnvPrepare_EndOfOptionsBeforeImage(t *testing.T) {
	docker, logFile, _ := fakeDocker(t)
	cfg := testWorkEnvConfig(t, docker)
	w, err := NewWorkEnv(cfg)
	require.NoError(t, err)
	require.NoError(t, w.Prepare(context.Background()))
	run := dockerCalls(t, logFile)[1]
	assert.Contains(t, run, "-- "+cfg.Image+" "+idleEntrypoint,
		"image must sit behind an end-of-options separator")
}

func TestWorkEnvPrepare_IdleEntrypointForwardsTERM(t *testing.T) {
	docker, _, _ := fakeDocker(t)
	cfg := testWorkEnvConfig(t, docker)
	w, err := NewWorkEnv(cfg)
	require.NoError(t, err)
	require.NoError(t, w.Prepare(context.Background()))
	script, err := os.ReadFile(filepath.Join(cfg.GTDir, "gt-idle.sh"))
	require.NoError(t, err)
	// PID 1 must forward TERM to the namespace and drain, not exit
	// instantly (an instant exit SIGKILLs the exec'd agent with no grace).
	assert.Contains(t, string(script), "kill -TERM -1")
	assert.Contains(t, string(script), "trap term TERM INT")
	assert.NotContains(t, string(script), "trap 'exit 0'")
}

func TestWorkEnvDocker_CancellationAttribution(t *testing.T) {
	dir := t.TempDir()
	slow := filepath.Join(dir, "docker")
	// A docker stand-in that hangs until killed.
	require.NoError(t, os.WriteFile(slow, []byte("#!/bin/sh\nsleep 60\n"), 0755))
	cfg := testWorkEnvConfig(t, slow)
	w, err := NewWorkEnv(cfg)
	require.NoError(t, err)

	t.Run("kill under canceled ctx wraps context.Canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(100 * time.Millisecond); cancel() }()
		_, err := w.docker(ctx, "run", "x")
		require.Error(t, err)
		assert.True(t, errors.Is(err, context.Canceled), "signal-kill under canceled ctx must classify as cancellation, got: %v", err)
	})

	t.Run("genuine nonzero exit keeps its own error even with ctx canceled", func(t *testing.T) {
		fail := filepath.Join(dir, "docker-fail")
		require.NoError(t, os.WriteFile(fail, []byte("#!/bin/sh\necho boom >&2\nexit 3\n"), 0755))
		cfgF := testWorkEnvConfig(t, fail)
		wf, err := NewWorkEnv(cfgF)
		require.NoError(t, err)
		// A genuine nonzero exit keeps its own error and stderr. (The exact
		// exited-nonzero-while-ctx-cancels race can't be sequenced
		// deterministically — CommandContext's kill is untrappable — but the
		// Exited() guard in docker() covers it; this pins the normal-fault
		// path staying un-swallowed.)
		_, err = wf.docker(context.Background(), "run", "x")
		require.Error(t, err)
		assert.False(t, errors.Is(err, context.Canceled))
		assert.Contains(t, err.Error(), "boom")
	})
}

func TestRemoveWorkContainer(t *testing.T) {
	docker, logFile, _ := fakeDocker(t)
	err := RemoveWorkContainer(context.Background(), docker, "MyRig", "furiosa")
	require.NoError(t, err)
	calls := dockerCalls(t, logFile)
	require.Len(t, calls, 1)
	assert.Equal(t, "rm -f gt-work-MyRig-furiosa", calls[0])
	assert.Equal(t, "gt-work-MyRig-furiosa", WorkContainerName("MyRig", "furiosa"))
}
