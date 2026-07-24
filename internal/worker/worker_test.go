package worker

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/proxy"
)

// startProxy runs a real proxy.Server (mTLS main + plaintext admin) and
// returns its main and admin addresses.
func startProxy(t *testing.T) (mainAddr, adminAddr string) {
	t.Helper()
	ca, err := proxy.GenerateCA(t.TempDir())
	require.NoError(t, err)

	srv, err := proxy.New(proxy.Config{
		ListenAddr:      "127.0.0.1:0",
		AdminListenAddr: "127.0.0.1:0",
		AllowedCommands: []string{"echo"},
		TownRoot:        t.TempDir(),
	}, ca)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { srv.Start(ctx) }() //nolint:errcheck

	require.Eventually(t, func() bool {
		if a := srv.Addr(); a != nil {
			mainAddr = a.String()
		}
		if a := srv.AdminAddr(); a != nil {
			adminAddr = a.String()
		}
		return mainAddr != "" && adminAddr != ""
	}, 5*time.Second, 10*time.Millisecond)
	return mainAddr, adminAddr
}

func TestEnsureIdentity_AdminSignerRoundTrip(t *testing.T) {
	_, adminAddr := startProxy(t)
	dir := t.TempDir()

	signer := &AdminSigner{AdminURL: "http://" + adminAddr, Rig: "MyRig", Name: "furiosa"}
	id, err := EnsureIdentity(context.Background(), dir, "gt-MyRig-furiosa", signer)
	require.NoError(t, err)

	// Key stays local with owner-only permissions.
	fi, err := os.Stat(id.KeyFile)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), fi.Mode().Perm())

	// Cert is bound to the expected CN with the default remote TTL.
	certPEM, err := os.ReadFile(id.CertFile)
	require.NoError(t, err)
	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block)
	leaf, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.Equal(t, "gt-MyRig-furiosa", leaf.Subject.CommonName)
	assert.WithinDuration(t, time.Now().Add(proxy.DefaultRemoteCertTTL), leaf.NotAfter, 5*time.Minute)

	// A second call reuses the identity instead of re-enrolling.
	id2, err := EnsureIdentity(context.Background(), dir, "gt-MyRig-furiosa", signer)
	require.NoError(t, err)
	certPEM2, err := os.ReadFile(id2.CertFile)
	require.NoError(t, err)
	assert.Equal(t, certPEM, certPEM2, "identity should be reused, not re-enrolled")

	// A different CN in the same dir re-enrolls.
	signer.Name = "nux"
	id3, err := EnsureIdentity(context.Background(), dir, "gt-MyRig-nux", signer)
	require.NoError(t, err)
	certPEM3, err := os.ReadFile(id3.CertFile)
	require.NoError(t, err)
	assert.NotEqual(t, certPEM, certPEM3)
}

func TestEnsureIdentity_ReEnrollsNearExpiry(t *testing.T) {
	_, adminAddr := startProxy(t)
	dir := t.TempDir()

	// First enrollment with a TTL below the renewal threshold.
	signer := &AdminSigner{AdminURL: "http://" + adminAddr, Rig: "MyRig", Name: "furiosa", TTL: "1h"}
	id, err := EnsureIdentity(context.Background(), dir, "gt-MyRig-furiosa", signer)
	require.NoError(t, err)
	certPEM, err := os.ReadFile(id.CertFile)
	require.NoError(t, err)

	// 1h remaining < renewBefore (12h) → must re-enroll.
	signer.TTL = ""
	id2, err := EnsureIdentity(context.Background(), dir, "gt-MyRig-furiosa", signer)
	require.NoError(t, err)
	certPEM2, err := os.ReadFile(id2.CertFile)
	require.NoError(t, err)
	assert.NotEqual(t, certPEM, certPEM2, "near-expiry identity should re-enroll")
}

func TestRelay_EndToEndExecThroughProxy(t *testing.T) {
	mainAddr, adminAddr := startProxy(t)

	signer := &AdminSigner{AdminURL: "http://" + adminAddr, Rig: "MyRig", Name: "furiosa"}
	id, err := EnsureIdentity(context.Background(), t.TempDir(), "gt-MyRig-furiosa", signer)
	require.NoError(t, err)

	relay, err := NewRelay("https://"+mainAddr, id)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- relay.Serve("127.0.0.1:0") }()
	t.Cleanup(func() {
		_ = relay.Close()
		select {
		case err := <-done:
			assert.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("relay did not shut down")
		}
	})

	var relayAddr net.Addr
	require.Eventually(t, func() bool {
		relayAddr = relay.Addr()
		return relayAddr != nil
	}, 5*time.Second, 10*time.Millisecond)

	// A plaintext client (the agent's side of the world) execs through the
	// relay; the relay authenticates to the proxy as gt-MyRig-furiosa.
	resp, err := http.Post(
		"http://"+relayAddr.String()+"/v1/exec",
		"application/json",
		strings.NewReader(`{"argv":["echo","hello-through-relay"]}`),
	)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	assert.Equal(t, 0, result.ExitCode)
	assert.Contains(t, result.Stdout, "hello-through-relay")
}

func TestNewRelay_RejectsNonHTTPSUpstream(t *testing.T) {
	dir := t.TempDir()
	// Identity files are not touched before the scheme check fails.
	id := &Identity{CertFile: filepath.Join(dir, "c"), KeyFile: filepath.Join(dir, "k"), CAFile: filepath.Join(dir, "a")}
	_, err := NewRelay("http://host:9876", id)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https")
}

func TestAdminSigner_ErrorSurfacesServerMessage(t *testing.T) {
	_, adminAddr := startProxy(t)
	// A CSR the server will reject: CN mismatch (signer says rig=Other).
	signer := &AdminSigner{AdminURL: "http://" + adminAddr, Rig: "Other", Name: "name"}
	_, err := EnsureIdentity(context.Background(), t.TempDir(), "gt-MyRig-furiosa", signer)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match")
}
