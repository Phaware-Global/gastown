package workerclient

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/execution"
	"github.com/steveyegge/gastown/internal/proxy"
	"github.com/steveyegge/gastown/internal/socket"
	"github.com/steveyegge/gastown/internal/worker"
)

// git runs git in dir, failing the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// startProxy runs a real proxy server with a rig repo, returning
// (proxyURL, ca, townRoot).
func startProxy(t *testing.T) (string, *proxy.CA, string) {
	t.Helper()
	townRoot := t.TempDir()
	// Host-side rig repo the worker clones through the relay.
	rigRepo := filepath.Join(townRoot, "demo", ".repo.git")
	require.NoError(t, os.MkdirAll(filepath.Dir(rigRepo), 0755))
	git(t, townRoot, "init", "--bare", rigRepo)
	seed := t.TempDir()
	git(t, seed, "clone", rigRepo, filepath.Join(seed, "wt"))
	wt := filepath.Join(seed, "wt")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "main.go"), []byte("v1\n"), 0644))
	git(t, wt, "add", "main.go")
	git(t, wt, "commit", "-m", "initial")
	git(t, wt, "push", "origin", "HEAD")

	ca, err := proxy.GenerateCA(t.TempDir())
	require.NoError(t, err)
	srv, err := proxy.New(proxy.Config{
		ListenAddr:      "127.0.0.1:0",
		AllowedCommands: []string{"echo"},
		TownRoot:        townRoot,
	}, ca)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { srv.Start(ctx) }() //nolint:errcheck

	var addr string
	require.Eventually(t, func() bool {
		if a := srv.Addr(); a != nil {
			addr = a.String()
			return true
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)
	return "https://" + addr, ca, townRoot
}

// caSigner implements socket.Signer with the real proxy CA (what the daemon
// does in production, via SignPolecatCSR).
type caSigner struct{ ca *proxy.CA }

func (s *caSigner) SignSessionCSR(_ context.Context, csrPEM []byte, cn string) ([]byte, []byte, time.Time, error) {
	certPEM, err := s.ca.SignPolecatCSR(csrPEM, cn, 0)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	return certPEM, s.ca.CertPEM, time.Now().Add(proxy.DefaultRemoteCertTTL), nil
}

// startService runs the worker service on a short unix socket.
func startService(t *testing.T, proxyURL string, cfgMut ...func(*Config)) (addr string, svc *Service) {
	t.Helper()
	dir, err := os.MkdirTemp("", "gtwc")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "w.sock")

	cfg := Config{
		WorkerID:    "test-worker",
		Token:       "t0k",
		StateDir:    filepath.Join(dir, "state"),
		ProxyURL:    proxyURL,
		ExecModes:   []string{"native"},
		MaxSessions: 2,
	}
	for _, mut := range cfgMut {
		mut(&cfg)
	}
	svc, err = New(cfg)
	require.NoError(t, err)

	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = svc.Serve(ctx, ln) }()
	return "unix://" + sock, svc
}

// newBackend builds a real SocketBackend against the service.
func newBackend(t *testing.T, addr string, ca *proxy.CA) *socket.SocketBackend {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"address": addr,
		"tls":     map[string]string{"mode": "none"},
		"token":   "t0k",
	})
	require.NoError(t, err)
	cfg := &config.ExecutionConfig{
		Backend:  socket.BackendName,
		ExecMode: config.ExecModeNative,
		// Fast enough for the test to observe a checkpoint, slow enough that a
		// loopback mTLS push completes within the derived op timeout and the
		// dead-man window (4× interval) doesn't fire before the first push.
		CheckpointStr: "2s",
		Extensions:    map[string]json.RawMessage{socket.BackendName: raw},
	}
	b, err := socket.New(cfg)
	require.NoError(t, err)
	b.Signer = &caSigner{ca: ca}
	b.OrchestratorID = "town-test"
	b.GTVersion = "test"
	return b
}

func polecatSpec() execution.PolecatSpec {
	return execution.PolecatSpec{Rig: "demo", Polecat: "furiosa", Session: "gt-demo-furiosa"}
}

// TestFullSessionLifecycle drives the REAL SocketBackend against the REAL
// service and REAL proxy: Provision (CSR signed by the real CA, relay up,
// worktree cloned through the relay), checkpointing through the relay to the
// host repo, Discover, and Teardown leaving the machine clean.
func TestFullSessionLifecycle(t *testing.T) {
	proxyURL, ca, townRoot := startProxy(t)
	addr, svc := startService(t, proxyURL)
	b := newBackend(t, addr, ca)

	// ---- Provision ----
	ep, err := b.Provision(context.Background(), polecatSpec())
	require.NoError(t, err)
	assert.Equal(t, "gt-demo-furiosa", ep.ID)

	// The worktree was cloned THROUGH the relay from the host rig repo.
	worktree := filepath.Join(svc.cfg.StateDir, "worktrees", "demo", "furiosa")
	data, err := os.ReadFile(filepath.Join(worktree, "main.go"))
	require.NoError(t, err)
	assert.Equal(t, "v1\n", string(data))

	// ---- Checkpointing: change the worktree, the session's checkpoint loop
	// pushes it through the relay to the host .repo.git ----
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "main.go"), []byte("v2-checkpointed\n"), 0644))
	rigRepo := filepath.Join(townRoot, "demo", ".repo.git")
	require.Eventually(t, func() bool {
		cmd := exec.Command("git", "show", "refs/checkpoints/polecat/furiosa:main.go")
		cmd.Dir = rigRepo
		out, err := cmd.Output()
		return err == nil && string(out) == "v2-checkpointed\n"
	}, 20*time.Second, 200*time.Millisecond, "checkpoint must land in the host repo through the relay")

	// ---- Reattach: a second Provision reuses the live session ----
	ep2, err := b.Provision(context.Background(), polecatSpec())
	require.NoError(t, err)
	assert.Equal(t, ep.ID, ep2.ID)

	// ---- Discover ----
	eps, err := b.Discover(context.Background(), execution.IdentityTags{Rig: "demo"})
	require.NoError(t, err)
	require.Len(t, eps, 1)
	assert.Equal(t, "furiosa", eps[0].Identity.Polecat)

	// ---- Teardown: session gone, worktree removed, identity shredded ----
	require.NoError(t, b.Teardown(context.Background(), ep))
	eps, err = b.Discover(context.Background(), execution.IdentityTags{})
	require.NoError(t, err)
	assert.Empty(t, eps)
	_, err = os.Stat(worktree)
	assert.True(t, os.IsNotExist(err), "worktree must be removed on teardown")
	_, err = os.Stat(filepath.Join(svc.cfg.StateDir, "identity", "gt-demo-furiosa"))
	assert.True(t, os.IsNotExist(err), "session identity must be shredded on teardown")
}

func TestAuth_BadTokenRefused(t *testing.T) {
	proxyURL, ca, _ := startProxy(t)
	addr, _ := startService(t, proxyURL)

	raw, err := json.Marshal(map[string]any{
		"address": addr, "tls": map[string]string{"mode": "none"}, "token": "WRONG",
	})
	require.NoError(t, err)
	cfg := &config.ExecutionConfig{
		Backend:    socket.BackendName,
		ExecMode:   config.ExecModeNative,
		Extensions: map[string]json.RawMessage{socket.BackendName: raw},
	}
	b, err := socket.New(cfg)
	require.NoError(t, err)
	b.Signer = &caSigner{ca: ca}

	_, err = b.Provision(context.Background(), polecatSpec())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth")
}

func TestSessionOpen_UnsupportedExecMode(t *testing.T) {
	proxyURL, ca, _ := startProxy(t)
	addr, _ := startService(t, proxyURL) // native only

	raw, err := json.Marshal(map[string]any{
		"address": addr, "tls": map[string]string{"mode": "none"}, "token": "t0k",
	})
	require.NoError(t, err)
	cfg := &config.ExecutionConfig{
		Backend:    socket.BackendName,
		ExecMode:   config.ExecModeContainer,
		Image:      "dev:latest",
		Extensions: map[string]json.RawMessage{socket.BackendName: raw},
	}
	b, err := socket.New(cfg)
	require.NoError(t, err)
	b.Signer = &caSigner{ca: ca}

	_, err = b.Provision(context.Background(), polecatSpec())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exec_mode")
}

func TestRestart_PersistedSessionsReportOrphaned(t *testing.T) {
	proxyURL, ca, _ := startProxy(t)
	addr, svc := startService(t, proxyURL)
	b := newBackend(t, addr, ca)

	_, err := b.Provision(context.Background(), polecatSpec())
	require.NoError(t, err)

	// "Restart": a fresh Service over the same state dir. The persisted
	// session must surface as orphaned (its supervisor/relay are gone), and
	// Provision must NOT reattach to it — it reaps via fresh session_open.
	dir, err := os.MkdirTemp("", "gtwc2")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock2 := filepath.Join(dir, "w.sock")
	svc2, err := New(Config{
		WorkerID: "test-worker", Token: "t0k",
		StateDir: svc.cfg.StateDir, ProxyURL: proxyURL,
		ExecModes: []string{"native"}, MaxSessions: 2,
	})
	require.NoError(t, err)
	ln2, err := net.Listen("unix", sock2)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = svc2.Serve(ctx, ln2) }()

	b2 := newBackend(t, "unix://"+sock2, ca)
	eps, err := b2.Discover(context.Background(), execution.IdentityTags{})
	require.NoError(t, err)
	require.Len(t, eps, 1, "persisted session must be reported")

	// The orphan is replaced by a fresh session on re-provision.
	ep, err := b2.Provision(context.Background(), polecatSpec())
	require.NoError(t, err)
	assert.Equal(t, "gt-demo-furiosa", ep.ID)
}

func TestShutdownFlushesFinalCheckpoint(t *testing.T) {
	proxyURL, ca, townRoot := startProxy(t)
	addr, svc := startService(t, proxyURL)
	b := newBackend(t, addr, ca)

	ep, err := b.Provision(context.Background(), polecatSpec())
	require.NoError(t, err)

	// A change made right before teardown: the graceful shutdown's final
	// flush must capture it (§9.3 via Teardown's shutdown-then-teardown).
	worktree := filepath.Join(svc.cfg.StateDir, "worktrees", "demo", "furiosa")
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "main.go"), []byte("final-words\n"), 0644))

	require.NoError(t, b.Teardown(context.Background(), ep))

	rigRepo := filepath.Join(townRoot, "demo", ".repo.git")
	out := git(t, rigRepo, "show", "refs/checkpoints/polecat/furiosa:main.go")
	assert.Equal(t, "final-words", out)
}

// silence unused import when worker types are only referenced indirectly.
var _ = worker.CheckpointRefForPolecat
