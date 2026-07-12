package proxy

import (
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// DefaultRemoteCertTTL is the leaf TTL for polecat certs issued via the
// CSR-signing path (docs/design/remote-polecat-execution.md §7.2).
// Deliberately much shorter than the 720h keypair-issuance default: remote
// workers are ephemeral, and a short TTL bounds exposure if a worker is
// compromised. The authoritative default lives here, not inherited from
// /v1/admin/issue-cert.
const DefaultRemoteCertTTL = 24 * time.Hour

// SignPolecatCSR signs a PEM-encoded PKCS#10 CSR as a polecat client cert.
//
// This is the remote-worker cert path (design §7.2): the private key is
// generated on the worker and never seen by the CA — only the public key and
// subject cross, and only the signed (public) cert returns. Do not use the
// keypair-issuing paths (IssuePolecat, /v1/admin/issue-cert) for remote
// workers; they transport the key.
//
// The CSR's CN must equal expectedCN, which must itself be a valid
// gt-<rig>-<name> identity — the caller binds expectedCN to the worker it is
// talking to over its authenticated provider channel. Everything else the CSR
// requests (SANs, extensions, key usages) is deliberately ignored: the issued
// cert is built fresh so a malicious CSR cannot smuggle CA:TRUE, a server
// EKU, or extra names past the CA.
//
// ttl <= 0 uses DefaultRemoteCertTTL.
func (ca *CA) SignPolecatCSR(csrPEM []byte, expectedCN string, ttl time.Duration) (certPEM []byte, err error) {
	if cnToIdentity(expectedCN) == "" {
		return nil, fmt.Errorf("invalid expected CN %q: must be gt-<rig>-<name> with non-empty rig and name", expectedCN)
	}

	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("csr: expected a CERTIFICATE REQUEST PEM block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("csr: parse: %w", err)
	}
	// Proof-of-possession: the CSR must be self-signed by the key it names.
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("csr: signature check: %w", err)
	}
	if csr.Subject.CommonName != expectedCN {
		return nil, fmt.Errorf("csr: CN %q does not match expected identity %q", csr.Subject.CommonName, expectedCN)
	}

	if ttl <= 0 {
		ttl = DefaultRemoteCertTTL
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: expectedCN},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(ttl),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("sign csr: %w", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), nil
}
