package socket

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/execution"
	"github.com/steveyegge/gastown/internal/proxy"
	"github.com/steveyegge/gastown/internal/worker"
)

// startProxyAdmin runs a real proxy with its plaintext admin server and returns
// the admin address.
func startProxyAdmin(t *testing.T) string {
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
	go func() { _ = srv.Start(ctx) }()

	var adminAddr string
	require.Eventually(t, func() bool {
		if a := srv.AdminAddr(); a != nil {
			adminAddr = a.String()
		}
		return adminAddr != ""
	}, 5*time.Second, 10*time.Millisecond)
	return adminAddr
}

// csrFor builds a real CSR the way a worker does.
func csrFor(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

// TestAdminSigner_SignsAgainstRealProxy exercises the PRODUCTION signing path
// end to end: the CSR a worker would send, signed through the proxy's admin
// API, yielding a cert bound to the polecat's CN plus a real expiry.
func TestAdminSigner_SignsAgainstRealProxy(t *testing.T) {
	adminAddr := startProxyAdmin(t)

	s, err := newAdminSigner("http://" + adminAddr)
	require.NoError(t, err)

	certPEM, caPEM, notAfter, err := s.SignSessionCSR(context.Background(),
		csrFor(t, worker.PolecatCN("demo", "furiosa")), "demo", "furiosa")
	require.NoError(t, err)
	assert.NotEmpty(t, caPEM, "the worker needs the proxy CA to verify the relay")

	leaf, err := parseLeaf(certPEM)
	require.NoError(t, err)
	assert.Equal(t, worker.PolecatCN("demo", "furiosa"), leaf.Subject.CommonName)
	assert.WithinDuration(t, leaf.NotAfter, notAfter, time.Second,
		"notAfter must come from the issued cert, not be invented")
	assert.True(t, notAfter.After(time.Now()), "a signed cert must not be born expired")
}

// TestAdminSigner_CSRIdentityIsBoundServerSide pins the §4.2 binding: the
// identity comes from the session the backend opened, so a worker that asks for
// a DIFFERENT polecat's CN is refused rather than issued a cert for it.
func TestAdminSigner_CSRIdentityIsBoundServerSide(t *testing.T) {
	adminAddr := startProxyAdmin(t)
	s, err := newAdminSigner("http://" + adminAddr)
	require.NoError(t, err)

	_, _, _, err = s.SignSessionCSR(context.Background(),
		csrFor(t, worker.PolecatCN("demo", "someone-else")), "demo", "furiosa")
	require.Error(t, err, "a CSR for another identity must not be signed")
	assert.Contains(t, err.Error(), "does not match expected identity")
}

// TestNewAdminSigner_RefusesNonLoopback pins the guard on an UNAUTHENTICATED
// API: a remote admin URL would ship every session CSR to an unverified signer
// and then install whatever CA it returned as the session's trust root.
func TestNewAdminSigner_RefusesNonLoopback(t *testing.T) {
	for _, u := range []string{
		"http://10.0.0.5:9877",
		"http://signer.example:9877",
		"https://192.168.1.10:9877",
	} {
		_, err := newAdminSigner(u)
		require.Error(t, err, u)
		assert.Contains(t, err.Error(), "not loopback")
	}
	// Hostnames are case-insensitive: refusing "Localhost" would be a false
	// negative that just confuses an operator.
	for _, u := range []string{"", "http://127.0.0.1:9877", "http://localhost:9877",
		"http://Localhost:9877", "http://LOCALHOST:9877", "http://[::1]:9877", "http://127.0.0.5:9877"} {
		_, err := newAdminSigner(u)
		assert.NoError(t, err, u)
	}
	// Near-misses that must still be refused.
	for _, u := range []string{"http://127.0.0.1.evil.example:9877", "http://0.0.0.0:9877",
		"http://localhost.evil.example:9877", "http://user@10.1.2.3:9877"} {
		_, err := newAdminSigner(u)
		assert.Error(t, err, u)
	}
}

// TestAdminSigner_RefusesWrongCN pins that this side VERIFIES rather than
// trusts. The cert it returns becomes the session's control-plane identity, so a
// signer that answered with another polecat's CN would silently hand the session
// that polecat's authz. Driven with a stand-in admin server returning a
// wrong-CN cert, since the real proxy would never produce one.
func TestAdminSigner_RefusesWrongCN(t *testing.T) {
	wrong := selfSigned(t, worker.PolecatCN("demo", "someone-else"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"cn":   worker.PolecatCN("demo", "furiosa"), // claims the right one
			"cert": string(wrong),                       // but issues another
			"ca":   string(wrong),
		})
	}))
	t.Cleanup(srv.Close)

	s, err := newAdminSigner(srv.URL)
	require.NoError(t, err)
	_, _, _, err = s.SignSessionCSR(context.Background(),
		csrFor(t, worker.PolecatCN("demo", "furiosa")), "demo", "furiosa")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to install it as this session's identity")
}

// TestAdminSigner_RefusesCAThatDidNotSignTheLeaf pins that the pair is checked:
// the returned CA becomes the worker's trust root for the relay, so a
// substituted or mixed-up root must be refused here rather than surface as an
// opaque mTLS failure once the session is running.
func TestAdminSigner_RefusesCAThatDidNotSignTheLeaf(t *testing.T) {
	adminAddr := startProxyAdmin(t)
	real, err := newAdminSigner("http://" + adminAddr)
	require.NoError(t, err)
	certPEM, _, _, err := real.SignSessionCSR(context.Background(),
		csrFor(t, worker.PolecatCN("demo", "furiosa")), "demo", "furiosa")
	require.NoError(t, err)

	// A legitimate leaf, paired with an unrelated CA.
	otherCA := selfSignedCA(t, "some-other-root")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"cert": string(certPEM), "ca": string(otherCA),
		})
	}))
	t.Cleanup(srv.Close)

	s, err := newAdminSigner(srv.URL)
	require.NoError(t, err)
	_, _, _, err = s.SignSessionCSR(context.Background(),
		csrFor(t, worker.PolecatCN("demo", "furiosa")), "demo", "furiosa")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not issued by the returned CA")
}

// selfSignedCA builds a throwaway CA certificate.
func selfSignedCA(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestAdminSigner_RefusesRedirectOffLoopback pins the gap a URL check alone
// leaves: validating admin_url proves where the FIRST request goes, but a
// redirect would carry the CSR exchange off-box and let the far end choose the
// CA that becomes the session's trust root.
func TestAdminSigner_RefusesRedirectOffLoopback(t *testing.T) {
	var elsewhere *httptest.Server
	elsewhere = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"cert": "x", "ca": "x"})
	}))
	t.Cleanup(elsewhere.Close)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/v1/admin/sign-csr", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	s, err := newAdminSigner(redirector.URL)
	require.NoError(t, err)
	_, _, _, err = s.SignSessionCSR(context.Background(),
		csrFor(t, worker.PolecatCN("demo", "furiosa")), "demo", "furiosa")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redirect")
}

func TestParseLeaf_RejectsGarbage(t *testing.T) {
	_, err := parseLeaf([]byte("not a pem"))
	assert.Error(t, err)
	_, err = parseLeaf(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")}))
	assert.Error(t, err)
}

// selfSigned builds a throwaway leaf for cn.
func selfSigned(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestForConfig_InstallsSigner is the regression test for the wiring gap: a
// socket backend resolved the way the polecat spawn path resolves it MUST come
// with a signer, or Provision fails on the CSR for every session — the exact
// failure that made socket rigs unusable from `gt polecat start` while every
// test set the signer by hand.
func TestForConfig_InstallsSigner(t *testing.T) {
	raw, err := json.Marshal(Settings{
		Address: "worker.example:9899",
		TLS:     TLSConfig{Mode: "auto", WorkerName: "mac-mini-1"},
	})
	require.NoError(t, err)
	cfg := &config.ExecutionConfig{
		Backend:  BackendName,
		ExecMode: config.ExecModeContainer,
		Image:    "dev:latest",
		Extensions: map[string]json.RawMessage{
			BackendName: raw,
		},
	}

	b, err := execution.ForConfig(cfg)
	require.NoError(t, err)
	sb, ok := b.(*SocketBackend)
	require.True(t, ok)
	assert.NotNil(t, sb.Signer, "a registry-resolved socket backend must be able to sign session CSRs")
	assert.NotEmpty(t, sb.OrchestratorID, "the handshake must identify this orchestrator")
}

// TestNew_RejectsBadAdminURL pins that a misconfigured admin_url fails at
// backend construction — polecat start reports it — rather than at the first
// CSR, mid-bringup, with a worker session already open.
func TestNew_RejectsBadAdminURL(t *testing.T) {
	raw, err := json.Marshal(Settings{
		Address:  "worker.example:9899",
		TLS:      TLSConfig{Mode: "auto", WorkerName: "mac-mini-1"},
		AdminURL: "http://evil.example:9877",
	})
	require.NoError(t, err)
	_, err = New(&config.ExecutionConfig{
		Backend:    BackendName,
		ExecMode:   config.ExecModeContainer,
		Image:      "dev:latest",
		Extensions: map[string]json.RawMessage{BackendName: raw},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not loopback")
}
