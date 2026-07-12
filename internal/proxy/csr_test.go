package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"
	"time"
)

func testCSR(t *testing.T, cn string, extra func(*x509.CertificateRequest)) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}
	if extra != nil {
		extra(tmpl)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func TestSignPolecatCSR_HappyPath(t *testing.T) {
	ca, err := GenerateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := testCSR(t, "gt-gastown-furiosa", nil)

	certPEM, err := ca.SignPolecatCSR(csrPEM, "gt-gastown-furiosa", 0)
	if err != nil {
		t.Fatalf("SignPolecatCSR: %v", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("did not get a CERTIFICATE PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	if cert.Subject.CommonName != "gt-gastown-furiosa" {
		t.Errorf("CN = %q", cert.Subject.CommonName)
	}
	if cert.IsCA {
		t.Error("issued cert must not be a CA")
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("EKU = %v, want client auth only", cert.ExtKeyUsage)
	}
	// Default TTL ≈ 24h (design §7.2), not the 720h keypair default.
	ttl := time.Until(cert.NotAfter)
	if ttl > 25*time.Hour || ttl < 23*time.Hour {
		t.Errorf("default TTL = %v, want ≈%v", ttl, DefaultRemoteCertTTL)
	}

	// Chains to the CA.
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("cert does not verify against CA: %v", err)
	}
}

func TestSignPolecatCSR_CNMismatch(t *testing.T) {
	ca, err := GenerateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := testCSR(t, "gt-gastown-impostor", nil)
	if _, err := ca.SignPolecatCSR(csrPEM, "gt-gastown-furiosa", 0); err == nil {
		t.Fatal("signed a CSR whose CN does not match the expected identity")
	}
}

func TestSignPolecatCSR_InvalidExpectedCN(t *testing.T) {
	ca, err := GenerateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := testCSR(t, "not-a-polecat", nil)
	for _, cn := range []string{"not-a-polecat", "", "gt-", "gt-only"} {
		if _, err := ca.SignPolecatCSR(csrPEM, cn, 0); err == nil {
			t.Errorf("accepted invalid expected CN %q", cn)
		}
	}
}

func TestSignPolecatCSR_MalformedPEM(t *testing.T) {
	ca, err := GenerateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{nil, []byte("garbage"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1}})} {
		if _, err := ca.SignPolecatCSR(bad, "gt-gastown-furiosa", 0); err == nil {
			t.Error("accepted malformed CSR input")
		}
	}
}

// TestSignPolecatCSR_IgnoresRequestedExtensions verifies a hostile CSR cannot
// smuggle SANs into the issued cert — the template is built fresh from the
// validated CN and public key only.
func TestSignPolecatCSR_IgnoresRequestedExtensions(t *testing.T) {
	ca, err := GenerateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := testCSR(t, "gt-gastown-furiosa", func(r *x509.CertificateRequest) {
		r.DNSNames = []string{"evil.example.com"}
		r.EmailAddresses = []string{"root@example.com"}
	})
	certPEM, err := ca.SignPolecatCSR(csrPEM, "gt-gastown-furiosa", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.DNSNames) != 0 || len(cert.EmailAddresses) != 0 {
		t.Errorf("issued cert carries CSR-requested SANs: %v %v", cert.DNSNames, cert.EmailAddresses)
	}
	if ttl := time.Until(cert.NotAfter); ttl > time.Hour+time.Minute {
		t.Errorf("explicit ttl not honored: %v", ttl)
	}
}
