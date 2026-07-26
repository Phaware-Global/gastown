package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
case "$*" in
  *"command -v gt"*) echo "/opt/gt/gt"; echo "/opt/gt/bd"; exit 0 ;;
esac
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
	require.Len(t, calls, 4, "rm -f (stale), run, /bin/sh probe, gt/bd verification: %v", calls)
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
		// The gt/bd verification now runs after this probe, so look for it
		// among the calls rather than assuming it is last.
		assert.Contains(t, strings.Join(dockerCalls(t, logFile), "\n"), "command -v 'claude'")
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

// TestWorkEnvPrepare_InjectsGtAndBd pins how a containerized agent reaches the
// control plane: gt-proxy-client from the CONTAINER's platform, mounted at
// /opt/gt as `gt` and `bd`, then linked onto PATH inside the container. Without
// it the agent runs but cannot call `gt done`, take mail, or update a bead —
// a session that looks alive and accomplishes nothing.
func TestWorkEnvPrepare_InjectsGtAndBd(t *testing.T) {
	docker, logFile, _ := fakeDocker(t)
	cfg := testWorkEnvConfig(t, docker)

	// A stand-in for the Linux gt-proxy-client the orchestrator pushed.
	client := filepath.Join(t.TempDir(), "gt-proxy-client")
	require.NoError(t, os.WriteFile(client, []byte("linux-proxy-client"), 0755))
	cfg.ProxyClient = client

	w, err := NewWorkEnv(cfg)
	require.NoError(t, err)
	require.NoError(t, w.Prepare(context.Background()))

	// The binary is copied (not linked) into the mounted dir, since the
	// container cannot follow a host symlink out of the mount.
	got, err := os.ReadFile(filepath.Join(cfg.GTDir, "gt-proxy-client"))
	require.NoError(t, err)
	assert.Equal(t, []byte("linux-proxy-client"), got)
	fi, err := os.Stat(filepath.Join(cfg.GTDir, "gt-proxy-client"))
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&0111, "the container must be able to execute it")

	for _, name := range []string{"gt", "bd"} {
		target, err := os.Readlink(filepath.Join(cfg.GTDir, name))
		require.NoError(t, err, "%s must be a symlink", name)
		// Relative: the link is resolved INSIDE the container, where the host
		// path does not exist.
		assert.Equal(t, "gt-proxy-client", target)
	}

	calls := dockerCalls(t, logFile)
	joined := strings.Join(calls, "\n")
	// The relay a container reaches is the bridge gateway, so relay mode has to
	// be stated — gt-proxy-client refuses plaintext to a non-loopback host.
	assert.Contains(t, joined, "-e GT_PROXY_RELAY=1")
	// Linked as root: the image's user may not own /usr/local/bin, and
	// /opt/gt is read-only so it cannot be done from inside the mount.
	assert.Contains(t, joined, "exec -u 0 gt-work-MyRig-furiosa /bin/sh -c mkdir -p /usr/local/bin && ln -sf /opt/gt/gt /usr/local/bin/gt")
	assert.Contains(t, joined, AgentPathPrefix+"command -v gt && command -v bd",
		"the probe must use the PATH the agent will run with, or it proves nothing")
}

// TestWorkEnvPrepare_NoInjectionWithoutAClient pins that a worker which has not
// yet been pushed container binaries still brings the container up — the next
// provision pushes them, and failing here would strand the session instead.
func TestWorkEnvPrepare_NoInjectionWithoutAClient(t *testing.T) {
	docker, logFile, _ := fakeDocker(t)
	cfg := testWorkEnvConfig(t, docker) // ProxyClient empty
	w, err := NewWorkEnv(cfg)
	require.NoError(t, err)
	require.NoError(t, w.Prepare(context.Background()))

	_, err = os.Stat(filepath.Join(cfg.GTDir, "gt"))
	assert.True(t, os.IsNotExist(err))
	joined := strings.Join(dockerCalls(t, logFile), "\n")
	assert.NotContains(t, joined, "/usr/local/bin/gt", "nothing to link")
	// But verification still runs: "can this agent reach the control plane" is
	// the question, and an image that ships its own gt/bd answers it.
	assert.Contains(t, joined, "command -v gt")
}

// fakeDockerMatching is a docker stand-in that fails only the invocations whose
// joined argv matches pattern — finer than fakeDocker's per-subcommand switch,
// which cannot express "the link exec fails but the verify exec succeeds".
func fakeDockerMatching(t *testing.T, pattern string) (docker, logFile string) {
	t.Helper()
	dir := t.TempDir()
	docker = filepath.Join(dir, "docker")
	logFile = filepath.Join(dir, "docker.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logFile + `"
case "$*" in
  *"` + pattern + `"*) echo "forced failure" >&2; exit 1 ;;
  *"command -v gt"*) echo "/opt/gt/gt"; echo "/opt/gt/bd"; exit 0 ;;
esac
if [ "$1" = "run" ]; then echo "deadbeefcafe"; fi
exit 0
`
	require.NoError(t, os.WriteFile(docker, []byte(script), 0755))
	return docker, logFile
}

// withProxyClient adds an injectable client to a config.
func withProxyClient(t *testing.T, cfg WorkEnvConfig) WorkEnvConfig {
	t.Helper()
	client := filepath.Join(t.TempDir(), "gt-proxy-client")
	require.NoError(t, os.WriteFile(client, []byte("client"), 0755))
	cfg.ProxyClient = client
	return cfg
}

// TestWorkEnvPrepare_CreatesUsrLocalBin pins the minimal-image case: busybox has
// no /usr/local/bin, and `ln -sf` into a missing directory fails. Before
// injection existed such an image came up degraded; it must not now fail to come
// up at all for want of one mkdir.
func TestWorkEnvPrepare_CreatesUsrLocalBin(t *testing.T) {
	docker, logFile := fakeDockerMatching(t, "NOTHING-FAILS")
	cfg := withProxyClient(t, testWorkEnvConfig(t, docker))
	w, err := NewWorkEnv(cfg)
	require.NoError(t, err)
	require.NoError(t, w.Prepare(context.Background()))

	assert.Contains(t, strings.Join(dockerCalls(t, logFile), "\n"), "mkdir -p /usr/local/bin && ln -sf")
}

// TestWorkEnvPrepare_LinkFailureSurvivesWhenGtResolves pins that VERIFICATION is
// the gate, not the linking: an image that already ships gt/bd (or has a
// read-only /usr) must still come up, because the agent can reach the control
// plane regardless.
func TestWorkEnvPrepare_LinkFailureSurvivesWhenGtResolves(t *testing.T) {
	docker, logFile := fakeDockerMatching(t, "ln -sf")
	cfg := withProxyClient(t, testWorkEnvConfig(t, docker))
	w, err := NewWorkEnv(cfg)
	require.NoError(t, err)

	require.NoError(t, w.Prepare(context.Background()),
		"a failed link must not fail the session when gt is on PATH anyway")
	assert.Contains(t, strings.Join(dockerCalls(t, logFile), "\n"), "command -v gt")
}

// TestWorkEnvPrepare_RefusesAShadowedInjection pins that an image cannot
// substitute its OWN control-plane CLI for the injected one. `command -v gt`
// merely proves something named gt is on PATH; the image is a registry tag —
// someone else's supply chain — and that binary would run with the session's
// env, worktree and (when mounted) docker socket.
func TestWorkEnvPrepare_RefusesAShadowedInjection(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	logFile := filepath.Join(dir, "docker.log")
	// Resolves gt to an image-supplied path rather than the injected one.
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logFile + `"
case "$*" in
  *"command -v gt"*) echo "/usr/bin/gt"; echo "/opt/gt/bd"; exit 0 ;;
esac
if [ "$1" = "run" ]; then echo "deadbeefcafe"; fi
exit 0
`
	require.NoError(t, os.WriteFile(docker, []byte(script), 0755))

	cfg := withProxyClient(t, testWorkEnvConfig(t, docker))
	w, err := NewWorkEnv(cfg)
	require.NoError(t, err)

	err = w.Prepare(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/usr/bin/gt")
	assert.Contains(t, err.Error(), "image-supplied")
}

// TestWorkEnvPrepare_RefusesAShadowedBd pins the half the first version of this
// check threw away: `bd` is the same injected binary under another name — the
// beads CLI — and only `gt` was ever compared, so an image shipping its own `bd`
// went completely unexamined.
func TestWorkEnvPrepare_RefusesAShadowedBd(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	logFile := filepath.Join(dir, "docker.log")
	// gt resolves to ours; bd does not.
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logFile + `"
case "$*" in
  *"command -v gt"*) echo "/opt/gt/gt"; echo "/usr/bin/bd"; exit 0 ;;
esac
if [ "$1" = "run" ]; then echo "deadbeefcafe"; fi
exit 0
`
	require.NoError(t, os.WriteFile(docker, []byte(script), 0755))

	cfg := withProxyClient(t, testWorkEnvConfig(t, docker))
	w, err := NewWorkEnv(cfg)
	require.NoError(t, err)

	err = w.Prepare(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bd")
	assert.Contains(t, err.Error(), "/usr/bin/bd")
}

// TestWorkEnvPrepare_LinkFailureStillRequiresTheMountedClient pins the hole the
// earlier guard had: it accepted /usr/local/bin/gt precisely when the link had
// failed, i.e. when that path could NOT be ours. With the PATH prefix the
// expectation is the mount either way.
func TestWorkEnvPrepare_LinkFailureStillRequiresTheMountedClient(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	logFile := filepath.Join(dir, "docker.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logFile + `"
case "$*" in
  *"ln -sf"*) echo "forced failure" >&2; exit 1 ;;
  *"command -v gt"*) echo "/usr/local/bin/gt"; echo "/usr/local/bin/bd"; exit 0 ;;
esac
if [ "$1" = "run" ]; then echo "deadbeefcafe"; fi
exit 0
`
	require.NoError(t, os.WriteFile(docker, []byte(script), 0755))

	cfg := withProxyClient(t, testWorkEnvConfig(t, docker))
	w, err := NewWorkEnv(cfg)
	require.NoError(t, err)

	err = w.Prepare(context.Background())
	require.Error(t, err, "a link that failed cannot have produced /usr/local/bin/gt")
	assert.Contains(t, err.Error(), "/usr/local/bin/gt")
}

// TestWorkEnvPrepare_AcceptsAnImageThatShipsItsOwn pins the supported config the
// hard error would otherwise have killed: no injectable client, but the image
// carries gt/bd. Verification passes, so the session comes up.
func TestWorkEnvPrepare_AcceptsAnImageThatShipsItsOwn(t *testing.T) {
	docker, _ := fakeDockerMatching(t, "NOTHING-FAILS")
	cfg := testWorkEnvConfig(t, docker) // no ProxyClient
	w, err := NewWorkEnv(cfg)
	require.NoError(t, err)
	require.NoError(t, w.Prepare(context.Background()),
		"an image that supplies its own gt/bd is a supported configuration")
}

// TestWorkEnvPrepare_FailsWhenGtIsNotOnPath pins the case that must stay fatal:
// an agent that cannot run `gt` cannot call `gt done`, so the session would look
// alive and accomplish nothing.
func TestWorkEnvPrepare_FailsWhenGtIsNotOnPath(t *testing.T) {
	docker, _ := fakeDockerMatching(t, "command -v gt")
	cfg := withProxyClient(t, testWorkEnvConfig(t, docker))
	w, err := NewWorkEnv(cfg)
	require.NoError(t, err)

	err = w.Prepare(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "control plane")
}

// TestWorkEnvPrepare_ConcurrentSessionsShareGTDirSafely pins the swap the
// injection does in a WORKER-SHARED directory: two sessions preparing at once
// must not collide on the gt/bd links (remove-then-symlink raced into EEXIST and
// failed a session), and the swap must never leave a live container's `gt`
// missing.
func TestWorkEnvPrepare_ConcurrentSessionsShareGTDirSafely(t *testing.T) {
	docker, _ := fakeDockerMatching(t, "NOTHING-FAILS")
	shared := t.TempDir()

	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		cfg := withProxyClient(t, testWorkEnvConfig(t, docker))
		cfg.GTDir = shared
		cfg.Polecat = fmt.Sprintf("polecat%d", i)
		cfg.Session = fmt.Sprintf("gt-MyRig-polecat%d", i)
		w, err := NewWorkEnv(cfg)
		require.NoError(t, err)
		wg.Add(1)
		go func(i int, w *WorkEnv) {
			defer wg.Done()
			errs[i] = w.Prepare(context.Background())
		}(i, w)
	}
	wg.Wait()

	for i, err := range errs {
		assert.NoError(t, err, "session %d must not lose a race on the shared gt/bd links", i)
	}
	target, err := os.Readlink(filepath.Join(shared, "gt"))
	require.NoError(t, err)
	assert.Equal(t, "gt-proxy-client", target)
}
