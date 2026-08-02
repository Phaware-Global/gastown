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
	"path/filepath"
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
