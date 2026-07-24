// Package worker implements the provider-neutral core of gt-worker-agent —
// the worker-side gastown supervisor for remote polecat execution
// (docs/design/remote-polecat-execution.md §3, §6.1). This package carries
// the pieces every provider shares: the polecat's mTLS identity bootstrap
// (CSR flow, §7.2) and the local plaintext relay that terminates mTLS to the
// host proxy on the worker (§6.1). Provider-specific delivery channels sit
// behind the Signer interface.
package worker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// Signer obtains a signed polecat client cert for a worker-generated CSR.
// It abstracts the provider channel (design §7.2): an EC2 backend delivers
// the CSR over its cloud command channel, a socket provider over its session
// connection, and tests/local dev over the proxy admin API. Implementations
// return the signed leaf cert and the proxy CA cert, both PEM.
type Signer interface {
	SignCSR(ctx context.Context, csrPEM []byte) (certPEM, caPEM []byte, err error)
}

// Identity is the polecat's control-plane identity on the worker: a private
// key that never left the machine, plus the CA-signed leaf cert for it.
type Identity struct {
	CN       string
	CertFile string // PEM leaf cert path
	KeyFile  string // PEM private key path
	CAFile   string // PEM proxy CA cert path
}

// renewBefore is how much remaining validity triggers re-enrollment instead
// of reuse. Half the 24h default TTL gives a wide margin over relay uptime.
const renewBefore = 12 * time.Hour

// EnsureIdentity loads or establishes the polecat identity for cn in dir.
//
// If dir already holds a cert for cn with at least renewBefore validity left
// (and its key), it is reused — this is what makes worker-agent restarts
// cheap. Otherwise a fresh ECDSA P-256 key is generated in dir (0600, never
// transmitted), a CSR for cn is signed via the provider channel, and the
// resulting cert + CA are written alongside it.
//
// dir should live on worker tmpfs per the design (§7.2); the caller chooses.
func EnsureIdentity(ctx context.Context, dir, cn string, signer Signer) (*Identity, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create identity dir: %w", err)
	}
	// Serialize concurrent enrollments on the same dir so interleaved writes
	// can never pair one run's key with another run's cert.
	unlock, err := lock.FlockAcquire(filepath.Join(dir, ".enroll.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock identity dir: %w", err)
	}
	defer unlock()

	id := &Identity{
		CN:       cn,
		CertFile: filepath.Join(dir, "client.crt"),
		KeyFile:  filepath.Join(dir, "client.key"),
		CAFile:   filepath.Join(dir, "ca.crt"),
	}

	if reusable(id, cn) {
		return id, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}, key)
	if err != nil {
		return nil, fmt.Errorf("create csr: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	certPEM, caPEM, err := signer.SignCSR(ctx, csrPEM)
	if err != nil {
		return nil, fmt.Errorf("sign csr: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	// Temp+rename per file so a crash never leaves a truncated file; the
	// flock above keeps whole enrollments from interleaving across runs.
	// Key first (0600), then cert + CA; a crash mid-way just re-enrolls.
	if err := writeFileAtomic(id.KeyFile, keyPEM, 0600); err != nil {
		return nil, fmt.Errorf("write key: %w", err)
	}
	if err := writeFileAtomic(id.CertFile, certPEM, 0644); err != nil {
		return nil, fmt.Errorf("write cert: %w", err)
	}
	if err := writeFileAtomic(id.CAFile, caPEM, 0644); err != nil {
		return nil, fmt.Errorf("write ca: %w", err)
	}
	return id, nil
}

// writeFileAtomic writes data to a temp sibling and renames it into place.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// reusable reports whether id's on-disk cert matches cn, still has
// renewBefore validity left, chains to the on-disk CA, and pairs with its
// on-disk key.
func reusable(id *Identity, cn string) bool {
	certPEM, err := os.ReadFile(id.CertFile)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	if leaf.Subject.CommonName != cn || time.Until(leaf.NotAfter) < renewBefore {
		return false
	}
	// The leaf must chain to the CURRENT on-disk CA. If the proxy's CA rotated
	// (e.g. its ca dir was regenerated after a host rebuild), a cached leaf
	// passes every other local check yet fails every mTLS handshake — a CA
	// mismatch must force re-enrollment, not silent reuse.
	caPEM, err := os.ReadFile(id.CAFile)
	if err != nil {
		return false
	}
	caBlock, _ := pem.Decode(caPEM)
	if caBlock == nil || caBlock.Type != "CERTIFICATE" {
		return false
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return false
	}
	if err := leaf.CheckSignatureFrom(caCert); err != nil {
		return false
	}
	// Confirm cert and key agree (also validates the key parses).
	if _, err := tls.LoadX509KeyPair(id.CertFile, id.KeyFile); err != nil {
		return false
	}
	return true
}

// ClientTLS builds the mTLS client config for dialing the host proxy with
// this identity.
func (id *Identity) ClientTLS() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(id.CertFile, id.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load client keypair: %w", err)
	}
	caPEM, err := os.ReadFile(id.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("ca file %s contains no valid certificates", id.CAFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}
