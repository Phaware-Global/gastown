package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProxyHTTPClient_RelayMode pins the shape a remote worker actually uses:
// the agent holds NO client cert (the worker's local relay holds the session
// identity and terminates mTLS upstream), so requiring cert/key/ca is what made
// `gt` unusable on a worker — it fell through to gt.real instead of proxying.
func TestProxyHTTPClient_RelayMode(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.1:54321",
		"http://localhost:54321",
		"http://Localhost:54321",
		"http://[::1]:54321",
	} {
		c, err := proxyHTTPClient(u, "", "", "")
		require.NoError(t, err, u)
		assert.NotNil(t, c)
		assert.Nil(t, c.Transport, "relay mode is plaintext to loopback: no TLS config needed")
	}
}

// TestProxyHTTPClient_RelayRefusesNonLoopback pins the guard on relay mode:
// plaintext to anything but loopback would send control-plane commands — `gt
// done`, bead updates — to a host this process cannot verify.
func TestProxyHTTPClient_RelayRefusesNonLoopback(t *testing.T) {
	for _, u := range []string{
		"http://10.0.0.7:9876",
		"https://proxy.example:9876",
		"http://127.0.0.1.evil.example:9876",
		"http://0.0.0.0:9876",
	} {
		_, err := proxyHTTPClient(u, "", "", "")
		require.Error(t, err, u)
		assert.Contains(t, err.Error(), "not loopback")
	}
}

// TestProxyHTTPClient_ExplicitRelayAllowsBridgeGateway pins the container shape:
// its relay binds the docker bridge gateway, not loopback, so relay mode there
// must be stated by the worker rather than inferred from the address.
func TestProxyHTTPClient_ExplicitRelayAllowsBridgeGateway(t *testing.T) {
	t.Setenv("GT_PROXY_RELAY", "1")
	c, err := proxyHTTPClient("http://172.17.0.1:9899", "", "", "")
	require.NoError(t, err)
	assert.Nil(t, c.Transport)

	// Anything other than an explicit "1" is not an opt-in.
	t.Setenv("GT_PROXY_RELAY", "true")
	_, err = proxyHTTPClient("http://172.17.0.1:9899", "", "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not loopback")
}

// TestProxyHTTPClient_PartialMaterialIsAnError pins that a half-configured mTLS
// setup fails loudly. Silently falling back to plaintext would drop the mTLS the
// operator asked for.
func TestProxyHTTPClient_PartialMaterialIsAnError(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "c.pem")
	require.NoError(t, os.WriteFile(cert, []byte("x"), 0600))

	_, err := proxyHTTPClient("https://proxy.example:9876", cert, "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be set together")
}

// TestProxyHTTPClient_DirectMTLSStillWorks pins the container-direct shape:
// full material means mTLS, to any host, exactly as before.
func TestProxyHTTPClient_DirectMTLSStillWorks(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, caFile := writeTestKeypair(t, dir)

	c, err := proxyHTTPClient("https://proxy.example:9876", certFile, keyFile, caFile)
	require.NoError(t, err)
	require.NotNil(t, c.Transport)
}

// writeTestKeypair writes a self-signed cert usable as both client cert and CA.
func writeTestKeypair(t *testing.T, dir string) (certFile, keyFile, caFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "gt-demo-furiosa"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	certFile = filepath.Join(dir, "client.crt")
	keyFile = filepath.Join(dir, "client.key")
	caFile = filepath.Join(dir, "ca.crt")
	require.NoError(t, os.WriteFile(certFile, certPEM, 0600))
	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0600))
	require.NoError(t, os.WriteFile(caFile, certPEM, 0600))
	return certFile, keyFile, caFile
}

func TestToolNameFromArg0(t *testing.T) {
	assert.Equal(t, "gt", toolNameFromArg0("/opt/gt/bin/gt"))
	assert.Equal(t, "bd", toolNameFromArg0("bd"))
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// hostHasRef reports whether ref exists in the host repo.
func hostHasRef(bare, ref string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = bare
	return cmd.Run() == nil
}

// polecatClone returns a session's clone on a polecat branch with one unpushed
// commit, and the bare repo it was cloned from.
func polecatClone(t *testing.T, branch string) (clone, bare string) {
	t.Helper()
	root := t.TempDir()
	bare = filepath.Join(root, "repo.git")
	gitT(t, root, "init", "--bare", "-b", "main", bare)
	clone = filepath.Join(root, "wt")
	gitT(t, root, "clone", bare, clone)
	gitT(t, clone, "commit", "--allow-empty", "-m", "initial")
	gitT(t, clone, "push", "origin", "HEAD:refs/heads/main")
	gitT(t, clone, "checkout", "-b", branch)
	// As the worker sets a session's clone up: the branch is pushed to its
	// work ref on the host, which has a remote-tracking ref for the lease.
	gitT(t, clone, "config", "remote.origin.push", "refs/heads/"+branch+":refs/remote-work/"+branch)
	gitT(t, clone, "config", "--add", "remote.origin.fetch", "+refs/remote-work/*:refs/remotes/origin-work/*")
	gitT(t, clone, "commit", "--allow-empty", "-m", "work")
	return clone, bare
}

// gt done runs on the host, so the remote agent's commits must be on the host
// before the call is forwarded.
func TestPushWorkBeforeDone_PushesPolecatBranch(t *testing.T) {
	const branch = "polecat/furiosa/gt-abc12@mv3cxv4t"
	clone, bare := polecatClone(t, branch)

	if _, err := pushWorkBeforeDone("gt", []string{"done"}, clone, ""); err != nil {
		t.Fatalf("pushWorkBeforeDone: %v", err)
	}
	if got, want := gitT(t, bare, "rev-parse", "refs/remote-work/"+branch), gitT(t, clone, "rev-parse", "HEAD"); got != want {
		t.Fatalf("host work ref = %s, want the worker's HEAD %s", got, want)
	}
	// The host's own branch namespace is not written.
	if hostHasRef(bare, "refs/heads/"+branch) {
		t.Fatalf("refs/heads/%s must not be created on the host", branch)
	}
}

// gt done on the host reads commits, so tracked changes left uncommitted would
// be dropped from the work without a word. Untracked files are not work.
func TestPushWorkBeforeDone_UncommittedTrackedChangesAreRefused(t *testing.T) {
	const branch = "polecat/furiosa/gt-abc12@mv3cxv4t"
	clone, bare := polecatClone(t, branch)
	if err := os.WriteFile(filepath.Join(clone, "tracked.txt"), []byte("v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitT(t, clone, "add", "tracked.txt")
	gitT(t, clone, "commit", "-m", "add file")
	if err := os.WriteFile(filepath.Join(clone, "scratch.txt"), []byte("notes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := pushWorkBeforeDone("gt", []string{"done"}, clone, branch); err != nil {
		t.Fatalf("an untracked file must not block gt done: %v", err)
	}

	if err := os.WriteFile(filepath.Join(clone, "tracked.txt"), []byte("v2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	pushed := gitT(t, bare, "rev-parse", "refs/remote-work/"+branch)
	_, err := pushWorkBeforeDone("gt", []string{"done"}, clone, branch)
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("expected a refusal naming the uncommitted changes, got: %v", err)
	}
	if got := gitT(t, bare, "rev-parse", "refs/remote-work/"+branch); got != pushed {
		t.Fatalf("a refused gt done must not push: work ref moved %s -> %s", pushed, got)
	}
}

// A commit amended or rebased after an earlier push replaces the session's own
// work ref: what ships is what the agent has now, not what it once pushed.
func TestPushWorkBeforeDone_RewrittenHistoryReplacesTheWorkRef(t *testing.T) {
	const branch = "polecat/furiosa/gt-abc12@mv3cxv4t"
	clone, bare := polecatClone(t, branch)
	if _, err := pushWorkBeforeDone("gt", []string{"done"}, clone, branch); err != nil {
		t.Fatalf("first push: %v", err)
	}
	gitT(t, clone, "commit", "--amend", "--allow-empty", "-m", "work, reworded")
	if _, err := pushWorkBeforeDone("gt", []string{"done"}, clone, branch); err != nil {
		t.Fatalf("push after an amend: %v", err)
	}
	if got, want := gitT(t, bare, "rev-parse", "refs/remote-work/"+branch), gitT(t, clone, "rev-parse", "HEAD"); got != want {
		t.Fatalf("host work ref = %s, want the amended HEAD %s", got, want)
	}
}

func TestPushWorkBeforeDone_OnlyForGtDoneOnAPolecatBranch(t *testing.T) {
	const branch = "polecat/furiosa/gt-abc12@mv3cxv4t"
	clone, bare := polecatClone(t, branch)
	for _, tc := range []struct {
		tool string
		args []string
	}{
		{"gt", []string{"prime"}},
		{"bd", []string{"done"}},
		{"gt", nil},
	} {
		if _, err := pushWorkBeforeDone(tc.tool, tc.args, clone, ""); err != nil {
			t.Fatalf("%s %v: %v", tc.tool, tc.args, err)
		}
	}
	gitT(t, clone, "checkout", "main")
	if _, err := pushWorkBeforeDone("gt", []string{"done"}, clone, ""); err != nil {
		t.Fatalf("non-polecat branch: %v", err)
	}
	if hostHasRef(bare, "refs/remote-work/"+branch) || hostHasRef(bare, "refs/heads/"+branch) {
		t.Fatal("nothing should have been pushed")
	}
	// Outside a git worktree there is nothing to push, and that is not an error.
	if _, err := pushWorkBeforeDone("gt", []string{"done"}, t.TempDir(), ""); err != nil {
		t.Fatalf("outside a worktree: %v", err)
	}
}

// A push that fails must stop gt done: forwarding it would complete the bead
// against a host branch that does not have the work.
func TestPushWorkBeforeDone_FailedPushIsAnError(t *testing.T) {
	clone, bare := polecatClone(t, "polecat/furiosa/gt-abc12@mv3cxv4t")
	if err := os.RemoveAll(bare); err != nil {
		t.Fatal(err)
	}
	if _, err := pushWorkBeforeDone("gt", []string{"done"}, clone, ""); err == nil {
		t.Fatal("expected an error when the push fails")
	}
}

// ESCALATED and DEFERRED do not complete the bead: they tell the host the
// polecat is stuck or parking. A polecat that cannot push, or has not
// committed, must still be able to say so, so the push is best effort there.
func TestPushWorkBeforeDone_NonCompletedStatusIsBestEffort(t *testing.T) {
	const branch = "polecat/furiosa/gt-abc12@mv3cxv4t"
	for _, args := range [][]string{
		{"done", "--status", "ESCALATED"},
		{"done", "--status=deferred"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			clone, bare := polecatClone(t, branch)
			if err := os.WriteFile(filepath.Join(clone, "tracked.txt"), []byte("v1\n"), 0600); err != nil {
				t.Fatal(err)
			}
			gitT(t, clone, "add", "tracked.txt")
			gitT(t, clone, "commit", "-m", "add file")
			if err := os.WriteFile(filepath.Join(clone, "tracked.txt"), []byte("v2\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := pushWorkBeforeDone("gt", args, clone, branch); err != nil {
				t.Fatalf("uncommitted changes must not block %v: %v", args, err)
			}
			// What is committed is still sent, so the host has it to recover.
			if got, want := gitT(t, bare, "rev-parse", "refs/remote-work/"+branch), gitT(t, clone, "rev-parse", "HEAD"); got != want {
				t.Fatalf("host work ref = %s, want the committed HEAD %s", got, want)
			}

			if err := os.RemoveAll(bare); err != nil {
				t.Fatal(err)
			}
			if _, err := pushWorkBeforeDone("gt", args, clone, branch); err != nil {
				t.Fatalf("a failed push must not block %v: %v", args, err)
			}
		})
	}
	// COMPLETED, named explicitly, keeps the hard refusal.
	clone, _ := polecatClone(t, branch)
	if err := os.WriteFile(filepath.Join(clone, "new.txt"), []byte("v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitT(t, clone, "add", "new.txt")
	if _, err := pushWorkBeforeDone("gt", []string{"done", "--status", "COMPLETED"}, clone, branch); err == nil {
		t.Fatal("an explicit COMPLETED must still refuse uncommitted tracked changes")
	}
}

// When the session names its work branch (GT_BRANCH), gt done must not be
// forwarded from anywhere else: the host would complete the bead against a
// copy of the branch that never received the work.
func TestPushWorkBeforeDone_SessionBranchIsAuthoritative(t *testing.T) {
	const branch = "polecat/furiosa/gt-abc12@mv3cxv4t"

	t.Run("detached HEAD", func(t *testing.T) {
		clone, _ := polecatClone(t, branch)
		gitT(t, clone, "checkout", "--detach")
		if _, err := pushWorkBeforeDone("gt", []string{"done"}, clone, branch); err == nil {
			t.Fatal("expected an error on a detached HEAD")
		}
	})
	t.Run("a different local branch", func(t *testing.T) {
		clone, _ := polecatClone(t, branch)
		gitT(t, clone, "checkout", "-b", "scratch")
		if _, err := pushWorkBeforeDone("gt", []string{"done"}, clone, branch); err == nil {
			t.Fatal("expected an error on a branch other than the session's")
		}
	})
	t.Run("outside a worktree", func(t *testing.T) {
		if _, err := pushWorkBeforeDone("gt", []string{"done"}, t.TempDir(), branch); err == nil {
			t.Fatal("expected an error outside a git worktree")
		}
	})
	t.Run("on the session branch", func(t *testing.T) {
		clone, bare := polecatClone(t, branch)
		if _, err := pushWorkBeforeDone("gt", []string{"done"}, clone, branch); err != nil {
			t.Fatalf("pushWorkBeforeDone: %v", err)
		}
		if got, want := gitT(t, bare, "rev-parse", "refs/remote-work/"+branch), gitT(t, clone, "rev-parse", "HEAD"); got != want {
			t.Fatalf("host work ref = %s, want %s", got, want)
		}
	})
	t.Run("a detached host worktree reports HEAD, which names no branch", func(t *testing.T) {
		clone, _ := polecatClone(t, branch)
		if _, err := pushWorkBeforeDone("gt", []string{"done"}, clone, "HEAD"); err != nil {
			t.Fatalf("pushWorkBeforeDone: %v", err)
		}
	})
}

// A best-effort exit that left work behind must tell the host so: the host
// reads its own worktree, which never holds the session's work, and would
// otherwise record the polecat as clean and reusable while the only copy of
// the work sits in a clone the next session replaces.
func TestPushWorkBeforeDone_UnsavedWorkIsReportedAsCleanupStatus(t *testing.T) {
	const branch = "polecat/furiosa/gt-abc12@mv3cxv4t"
	deferred := []string{"done", "--status", "DEFERRED"}

	t.Run("pushed and committed reports nothing", func(t *testing.T) {
		clone, _ := polecatClone(t, branch)
		status, err := pushWorkBeforeDone("gt", deferred, clone, branch)
		if err != nil || status != "" {
			t.Fatalf("status = %q, err = %v; want no status", status, err)
		}
	})
	t.Run("uncommitted tracked changes", func(t *testing.T) {
		clone, _ := polecatClone(t, branch)
		if err := os.WriteFile(filepath.Join(clone, "new.txt"), []byte("v1\n"), 0600); err != nil {
			t.Fatal(err)
		}
		gitT(t, clone, "add", "new.txt")
		status, err := pushWorkBeforeDone("gt", deferred, clone, branch)
		if err != nil || status != "uncommitted" {
			t.Fatalf("status = %q, err = %v; want uncommitted", status, err)
		}
	})
	t.Run("failed push", func(t *testing.T) {
		clone, bare := polecatClone(t, branch)
		if err := os.RemoveAll(bare); err != nil {
			t.Fatal(err)
		}
		status, err := pushWorkBeforeDone("gt", deferred, clone, branch)
		if err != nil || status != "unpushed" {
			t.Fatalf("status = %q, err = %v; want unpushed", status, err)
		}
	})
	t.Run("not on the session branch", func(t *testing.T) {
		clone, _ := polecatClone(t, branch)
		gitT(t, clone, "checkout", "--detach")
		status, err := pushWorkBeforeDone("gt", deferred, clone, branch)
		if err != nil || status != "unknown" {
			t.Fatalf("status = %q, err = %v; want unknown", status, err)
		}
	})
}

func TestWithCleanupStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		argv   []string
		status string
		want   []string
	}{
		{"nothing to report", []string{"gt", "done", "--status", "DEFERRED"}, "", []string{"gt", "done", "--status", "DEFERRED"}},
		{"appended", []string{"gt", "done", "--status", "DEFERRED"}, "unpushed", []string{"gt", "done", "--status", "DEFERRED", "--cleanup-status=unpushed"}},
		{"caller's own flag wins", []string{"gt", "done", "--cleanup-status", "stash"}, "unpushed", []string{"gt", "done", "--cleanup-status", "stash"}},
		{"caller's own flag wins (= form)", []string{"gt", "done", "--cleanup-status=clean"}, "uncommitted", []string{"gt", "done", "--cleanup-status=clean"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := withCleanupStatus(tc.argv, tc.status)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
