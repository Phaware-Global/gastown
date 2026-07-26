package workerca

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCSR(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func TestLoadOrCreate_IdempotentAndSeparateFromProxyCA(t *testing.T) {
	dir := t.TempDir()
	ca1, err := LoadOrCreate(dir)
	require.NoError(t, err)
	assert.True(t, ca1.Cert.IsCA)
	assert.Equal(t, "GasTown Worker CA", ca1.Cert.Subject.CommonName)

	// Reload returns the SAME CA (a new one would invalidate every enrolled
	// machine cert).
	ca2, err := LoadOrCreate(dir)
	require.NoError(t, err)
	assert.Equal(t, ca1.Cert.SerialNumber, ca2.Cert.SerialNumber)

	// The orchestrator client cert exists and chains to this CA.
	certPEM, keyPEM, caPEM, err := ca2.OrchestratorMaterial()
	require.NoError(t, err)
	assert.NotEmpty(t, keyPEM)
	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block)
	orch, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(caPEM))
	_, err = orch.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	assert.NoError(t, err, "orchestrator client cert must chain to the worker CA")
}

func TestSignMachineCSR_BindsCNServerSide(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	require.NoError(t, err)

	// The CSR asks for a DIFFERENT CN than the enrolled name: the CA must bind
	// the operator-chosen name, ignoring the request.
	certPEM, notAfter, err := ca.SignMachineCSR(testCSR(t, "impostor"), "gpu-box-1")
	require.NoError(t, err)
	assert.True(t, notAfter.After(time.Now()))

	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.Equal(t, "gpu-box-1", cert.Subject.CommonName)
	assert.Equal(t, []string{"gpu-box-1"}, cert.DNSNames, "name must be a DNS SAN for ServerName pinning")
	assert.False(t, cert.IsCA, "a machine cert must never be a CA")

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(ca.CertPEM))
	_, err = cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "gpu-box-1",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	assert.NoError(t, err)
}

func TestSignMachineCSR_RejectsBadInput(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	require.NoError(t, err)

	t.Run("invalid names", func(t *testing.T) {
		for _, name := range []string{"", "..", ".", "has space", "a/b", "-leading", string(make([]byte, 64))} {
			_, _, err := ca.SignMachineCSR(testCSR(t, "x"), name)
			assert.Error(t, err, "name %q must be rejected", name)
		}
	})

	t.Run("malformed CSR", func(t *testing.T) {
		_, _, err := ca.SignMachineCSR([]byte("not a pem"), "gpu-box-1")
		assert.Error(t, err)
	})

	t.Run("weak RSA key rejected", func(t *testing.T) {
		key, err := rsaKey(t, 1024)
		require.NoError(t, err)
		der, err := x509.CreateCertificateRequest(rand.Reader,
			&x509.CertificateRequest{Subject: pkix.Name{CommonName: "x"}}, key)
		require.NoError(t, err)
		csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
		_, _, err = ca.SignMachineCSR(csrPEM, "gpu-box-1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "rsa key too small")
	})
}

func TestRegistry_RecordListRevoke(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, ca.Record(Worker{Name: "b-box", Address: "10.0.0.2:9878", Serial: "b2", EnrolledAt: time.Now()}))
	require.NoError(t, ca.Record(Worker{Name: "a-box", Address: "10.0.0.1:9878", Serial: "a1", EnrolledAt: time.Now()}))

	reg, err := ca.LoadRegistry()
	require.NoError(t, err)
	require.Len(t, reg.Workers, 2)
	assert.Equal(t, "a-box", reg.Workers[0].Name, "registry is sorted by name")

	// Re-enrollment replaces in place (rotation), not duplicates.
	require.NoError(t, ca.Record(Worker{Name: "a-box", Address: "10.0.0.9:9878", Serial: "a2", EnrolledAt: time.Now()}))
	reg, err = ca.LoadRegistry()
	require.NoError(t, err)
	require.Len(t, reg.Workers, 2)
	w, err := ca.Lookup("a-box")
	require.NoError(t, err)
	assert.Equal(t, "a2", w.Serial)
	assert.Equal(t, "10.0.0.9:9878", w.Address)

	// Revoke marks, second revoke errors, unknown name errors.
	require.NoError(t, ca.Revoke("a-box"))
	w, err = ca.Lookup("a-box")
	require.NoError(t, err)
	assert.True(t, w.Revoked)
	assert.Error(t, ca.Revoke("a-box"))
	assert.Error(t, ca.Revoke("nope"))

	// LoadRegistryFrom sees the same state without touching the CA.
	reg2, err := LoadRegistryFrom(ca.Dir)
	require.NoError(t, err)
	require.Len(t, reg2.Workers, 2)
}

func TestNewJoinToken_Unique(t *testing.T) {
	a, err := NewJoinToken()
	require.NoError(t, err)
	b, err := NewJoinToken()
	require.NoError(t, err)
	assert.Len(t, a, 64) // 32 bytes hex
	assert.NotEqual(t, a, b)
}

func TestValidWorkerName(t *testing.T) {
	for _, ok := range []string{"gpu-box-1", "a", "A.b_c-1", "worker99"} {
		assert.True(t, ValidWorkerName(ok), "%q should be valid", ok)
	}
	for _, bad := range []string{"", ".", "..", "-lead", "_lead", ".lead", "has space", "a/b", "a\nb"} {
		assert.False(t, ValidWorkerName(bad), "%q should be invalid", bad)
	}
}

func TestEnrollWorker_RejectsBadArgs(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	require.NoError(t, err)
	_, err = ca.EnrollWorker(context.Background(), "bad name", "127.0.0.1:1", "tok")
	assert.Error(t, err)
	_, err = ca.EnrollWorker(context.Background(), "ok-name", "127.0.0.1:1", "")
	assert.Error(t, err)
}

// rsaKey generates an RSA key of the given size for weak-key testing.
func rsaKey(t *testing.T, bits int) (*rsa.PrivateKey, error) {
	t.Helper()
	return rsa.GenerateKey(rand.Reader, bits)
}

// TestMachineCertCannotActAsClient pins the round-1 HIGH: a machine cert must
// NOT satisfy a client-auth requirement, or one stolen worker key would
// authenticate as the orchestrator to every other worker.
func TestMachineCertCannotActAsClient(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	require.NoError(t, err)
	certPEM, _, err := ca.SignMachineCSR(testCSR(t, "x"), "gpu-box-1")
	require.NoError(t, err)
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)

	assert.NotContains(t, cert.ExtKeyUsage, x509.ExtKeyUsageClientAuth,
		"machine certs must be ServerAuth-only")

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(ca.CertPEM))
	_, err = cert.Verify(x509.VerifyOptions{Roots: pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	assert.Error(t, err, "a machine cert must fail client-auth verification")
}

func TestCertHygiene_BasicConstraints(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	require.NoError(t, err)
	assert.True(t, ca.Cert.MaxPathLenZero, "CA must forbid intermediates")

	certPEM, _, err := ca.SignMachineCSR(testCSR(t, "x"), "gpu-box-1")
	require.NoError(t, err)
	block, _ := pem.Decode(certPEM)
	machine, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.True(t, machine.BasicConstraintsValid, "leaf must assert CA:FALSE explicitly")
	assert.False(t, machine.IsCA)

	orchPEM, _, _, err := ca.OrchestratorMaterial()
	require.NoError(t, err)
	oblock, _ := pem.Decode(orchPEM)
	orch, err := x509.ParseCertificate(oblock.Bytes)
	require.NoError(t, err)
	assert.True(t, orch.BasicConstraintsValid)
	assert.False(t, orch.IsCA)
	assert.Equal(t, OrchestratorCN, orch.Subject.CommonName)
}

func TestConcurrentRecord_NoLostUpdates(t *testing.T) {
	// Concurrent enroll-style registry writes must not lose entries (a lost
	// worker can never be revoked even though its cert stays valid).
	ca, err := LoadOrCreate(t.TempDir())
	require.NoError(t, err)
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = ca.Record(Worker{
				Name: fmt.Sprintf("box-%d", i), Address: "10.0.0.1:9878",
				Serial: fmt.Sprintf("s%d", i), EnrolledAt: time.Now(),
			})
		}(i)
	}
	wg.Wait()
	reg, err := ca.LoadRegistry()
	require.NoError(t, err)
	assert.Len(t, reg.Workers, n, "concurrent Record must not lose entries")
}

func TestEnsureOrchestratorCert_RemintsWhenCARotates(t *testing.T) {
	dir := t.TempDir()
	ca1, err := LoadOrCreate(dir)
	require.NoError(t, err)
	orig, _, _, err := ca1.OrchestratorMaterial()
	require.NoError(t, err)

	// Replace the CA (as a clobbering concurrent create would) and reload:
	// the stale orchestrator cert no longer chains, so it must be re-minted
	// rather than left unusable.
	require.NoError(t, os.Remove(filepath.Join(dir, WorkerCACertFile)))
	require.NoError(t, os.Remove(filepath.Join(dir, caKeyFile)))
	ca2, err := LoadOrCreate(dir)
	require.NoError(t, err)
	fresh, _, caPEM, err := ca2.OrchestratorMaterial()
	require.NoError(t, err)
	assert.NotEqual(t, string(orig), string(fresh), "stale orchestrator cert must be re-minted")

	block, _ := pem.Decode(fresh)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(caPEM))
	_, err = cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	assert.NoError(t, err, "re-minted cert must chain to the current CA")
}
