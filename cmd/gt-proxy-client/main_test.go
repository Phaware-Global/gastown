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

// polecatClone returns a clone on a polecat branch with one unpushed commit,
// and the bare repo it was cloned from.
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
	gitT(t, clone, "commit", "--allow-empty", "-m", "work")
	return clone, bare
}

// gt done runs on the host, so the remote agent's commits must be on the host
// before the call is forwarded.
func TestPushWorkBeforeDone_PushesPolecatBranch(t *testing.T) {
	const branch = "polecat/furiosa/gt-abc12@mv3cxv4t"
	clone, bare := polecatClone(t, branch)

	if err := pushWorkBeforeDone("gt", []string{"done"}, clone); err != nil {
		t.Fatalf("pushWorkBeforeDone: %v", err)
	}
	if got, want := gitT(t, bare, "rev-parse", "refs/heads/"+branch), gitT(t, clone, "rev-parse", "HEAD"); got != want {
		t.Fatalf("host branch = %s, want the worker's HEAD %s", got, want)
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
		if err := pushWorkBeforeDone(tc.tool, tc.args, clone); err != nil {
			t.Fatalf("%s %v: %v", tc.tool, tc.args, err)
		}
	}
	gitT(t, clone, "checkout", "main")
	if err := pushWorkBeforeDone("gt", []string{"done"}, clone); err != nil {
		t.Fatalf("non-polecat branch: %v", err)
	}
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = bare
	if cmd.Run() == nil {
		t.Fatal("nothing should have been pushed")
	}
	// Outside a git worktree there is nothing to push, and that is not an error.
	if err := pushWorkBeforeDone("gt", []string{"done"}, t.TempDir()); err != nil {
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
	if err := pushWorkBeforeDone("gt", []string{"done"}, clone); err == nil {
		t.Fatal("expected an error when the push fails")
	}
}
