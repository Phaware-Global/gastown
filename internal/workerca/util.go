package workerca

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// workerNameRe bounds enrolled worker names: they become a cert CN, a DNS SAN,
// the TLS ServerName pin, and a registry key, so keep them strictly safe.
var workerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidWorkerName reports whether name is a safe enrolled-machine name.
func ValidWorkerName(name string) bool {
	if name == "." || name == ".." {
		return false
	}
	// Reserve the orchestrator identity: a machine cert with CN
	// gt-orchestrator would satisfy the worker's CN pin, coupling the two
	// impersonation barriers that are meant to be independent (the pin would
	// stop protecting anything if machine certs ever regained ClientAuth).
	if name == OrchestratorCN {
		return false
	}
	return workerNameRe.MatchString(name)
}

// newSerial returns a random 128-bit certificate serial.
func newSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("workerca: generate serial: %w", err)
	}
	return serial, nil
}

// marshalKey PEM-encodes an ECDSA private key.
func marshalKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("workerca: marshal key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// writeFileAtomic writes via a temp sibling + rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	// A PID-unique temp sibling: a fixed ".tmp" would be shared by two
	// concurrent writers, risking a corrupt intermediate.
	tmp := fmt.Sprintf("%s.tmp%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return fmt.Errorf("workerca: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("workerca: rename %s: %w", path, err)
	}
	return nil
}

// checkKeyStrength rejects weak or unknown CSR public keys before the CA
// endorses them — CheckSignature proves possession, not strength. Mirrors the
// proxy CA's floor: RSA >= 2048, ECDSA P-256/384/521, Ed25519.
func checkKeyStrength(pub any) error {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256(), elliptic.P384(), elliptic.P521():
			return nil
		default:
			return fmt.Errorf("ecdsa curve %s not allowed (want P-256/P-384/P-521)", k.Curve.Params().Name)
		}
	case ed25519.PublicKey:
		return nil
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return fmt.Errorf("rsa key too small: %d bits (min 2048)", k.N.BitLen())
		}
		return nil
	default:
		return fmt.Errorf("unsupported public key type %T", pub)
	}
}

// certSerialHex returns the hex serial of a PEM certificate.
func certSerialHex(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("workerca: not a CERTIFICATE PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("workerca: parse cert: %w", err)
	}
	return cert.SerialNumber.Text(16), nil
}

// lockDir takes an exclusive advisory lock on the material dir, serializing
// read-modify-write sequences (registry updates, first CA creation) across
// concurrent gt invocations.
func lockDir(dir string) (func(), error) {
	unlock, err := lock.FlockAcquire(filepath.Join(dir, ".workerca.lock"))
	if err != nil {
		return nil, fmt.Errorf("workerca: lock material dir: %w", err)
	}
	return unlock, nil
}

// orchestratorCertValid reports whether an existing orchestrator cert both
// parses and still chains to the CURRENT CA — if the CA was replaced, the old
// client cert is useless and must be re-minted.
func (ca *CA) orchestratorCertValid(certPath string) bool {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	if err := cert.CheckSignatureFrom(ca.Cert); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(ca.Dir, OrchestratorKeyFile)); err != nil {
		return false
	}
	return time.Now().Before(cert.NotAfter)
}
