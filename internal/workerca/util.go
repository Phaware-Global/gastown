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
	"regexp"
)

// workerNameRe bounds enrolled worker names: they become a cert CN, a DNS SAN,
// the TLS ServerName pin, and a registry key, so keep them strictly safe.
var workerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidWorkerName reports whether name is a safe enrolled-machine name.
func ValidWorkerName(name string) bool {
	if name == "." || name == ".." {
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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return fmt.Errorf("workerca: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
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
