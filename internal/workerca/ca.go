// Package workerca implements the orchestrator's WORKER CA and enrolled-worker
// registry (docs/design/remote-polecat-execution-socket.md §3.1).
//
// This CA is deliberately distinct from the proxy CA: the proxy CA mints
// POLECAT identities (which authenticate to gt-proxy-server and can reach
// Dolt/git through it), while the worker CA mints MACHINE identities (which
// only prove "I am the enrolled worker at this address"). Keeping them apart
// means a stolen machine cert lets an attacker be a worker — accept sessions —
// but never mint a polecat identity or call the proxy (§10).
//
// Material lives under a single directory (default ~/.gt/worker-ca):
//
//	worker-ca.crt/.key      the worker CA (signs machine certs)
//	orchestrator.crt/.key   this daemon's client cert, presented to workers
//	workers.json            the enrolled-worker registry (name → cert info)
package workerca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// File names within the material directory. These are a CONTRACT with the
// socket backend's auto-TLS mode (internal/socket.clientTLS reads
// WorkerCACertFile / OrchestratorCertFile / OrchestratorKeyFile from this same
// directory) — renaming one without the other silently breaks every enrolled
// worker's dial.
const (
	// WorkerCACertFile is the worker CA certificate: it verifies machine
	// certs, and (since it also signs the orchestrator client cert) is what
	// workers pin as the client CA.
	WorkerCACertFile = "worker-ca.crt"
	// OrchestratorCertFile / OrchestratorKeyFile are the daemon's client
	// credentials, presented to every enrolled worker.
	OrchestratorCertFile = "orchestrator.crt"
	OrchestratorKeyFile  = "orchestrator.key"
	// RegistryFile is the enrolled-worker registry.
	RegistryFile = "workers.json"

	caKeyFile     = "worker-ca.key"
	registryTmp   = "workers.json.tmp"
	orchestatorCN = "gt-orchestrator"
)

// Lifetimes. Machine certs are long-lived (an enrolled machine is a durable
// operator decision, unlike an ephemeral polecat session cert); rotation is an
// explicit re-enroll.
const (
	caTTL      = 10 * 365 * 24 * time.Hour
	machineTTL = 365 * 24 * time.Hour
	orchTTL    = 365 * 24 * time.Hour
)

// CA is the worker CA plus its material directory.
type CA struct {
	Dir     string
	Cert    *x509.Certificate
	CertPEM []byte
	Key     *ecdsa.PrivateKey
}

// DefaultDir returns the material directory: $GT_WORKER_CA_DIR or
// ~/.gt/worker-ca (matching the socket backend's auto-TLS lookup).
func DefaultDir() (string, error) {
	if d := os.Getenv("GT_WORKER_CA_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("workerca: resolve home: %w", err)
	}
	return filepath.Join(home, ".gt", "worker-ca"), nil
}

// LoadOrCreate loads the worker CA from dir, creating it (and the
// orchestrator's own client cert) on first use.
func LoadOrCreate(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("workerca: create dir: %w", err)
	}
	ca, err := load(dir)
	if err == nil {
		return ca, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	return create(dir)
}

func load(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, WorkerCACertFile))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("workerca: parse CA keypair: %w", err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("workerca: parse CA cert: %w", err)
	}
	if time.Now().After(cert.NotAfter) {
		return nil, fmt.Errorf("workerca: CA expired at %v; remove %s and re-enroll workers", cert.NotAfter, dir)
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("workerca: CA key is not ECDSA")
	}
	return &CA{Dir: dir, Cert: cert, CertPEM: certPEM, Key: key}, nil
}

func create(dir string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("workerca: generate CA key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "GasTown Worker CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(caTTL),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("workerca: create CA cert: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM, err := marshalKey(key)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(dir, WorkerCACertFile), certPEM, 0644); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(dir, caKeyFile), keyPEM, 0600); err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("workerca: parse new CA cert: %w", err)
	}
	ca := &CA{Dir: dir, Cert: cert, CertPEM: certPEM, Key: key}

	// The orchestrator's own client cert, which workers verify against this
	// same CA (§3.1 step 2 returns it as the client-cert CA).
	if err := ca.ensureOrchestratorCert(); err != nil {
		return nil, err
	}
	return ca, nil
}

// ensureOrchestratorCert mints the daemon's client cert if absent.
func (ca *CA) ensureOrchestratorCert() error {
	certPath := filepath.Join(ca.Dir, OrchestratorCertFile)
	if _, err := os.Stat(certPath); err == nil {
		return nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("workerca: generate orchestrator key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: orchestatorCN},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(orchTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return fmt.Errorf("workerca: create orchestrator cert: %w", err)
	}
	keyPEM, err := marshalKey(key)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(ca.Dir, OrchestratorKeyFile), keyPEM, 0600)
}

// SignMachineCSR signs a worker's machine CSR (§3.1 step 2). The CN is bound
// SERVER-SIDE to the enrolled name the operator chose — whatever CN the CSR
// requests is ignored — and every other CSR-requested extension (SANs, EKUs,
// basic constraints) is dropped: the cert is built fresh so a malicious CSR
// cannot smuggle CA:TRUE or extra identities past the CA.
//
// The machine cert carries name as a DNS SAN so the orchestrator's TLS dial
// can pin ServerName to the enrolled name (§3.1 step 3).
func (ca *CA) SignMachineCSR(csrPEM []byte, name string) (certPEM []byte, notAfter time.Time, err error) {
	if !ValidWorkerName(name) {
		return nil, time.Time{}, fmt.Errorf("workerca: invalid worker name %q", name)
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, time.Time{}, fmt.Errorf("workerca: expected a CERTIFICATE REQUEST PEM block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("workerca: parse CSR: %w", err)
	}
	// Proof of possession, then a key-strength floor (same discipline as the
	// proxy CA's polecat path).
	if err := csr.CheckSignature(); err != nil {
		return nil, time.Time{}, fmt.Errorf("workerca: CSR signature: %w", err)
	}
	if err := checkKeyStrength(csr.PublicKey); err != nil {
		return nil, time.Time{}, fmt.Errorf("workerca: %w", err)
	}

	serial, err := newSerial()
	if err != nil {
		return nil, time.Time{}, err
	}
	expiry := time.Now().Add(machineTTL)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     expiry,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		// A worker is a TLS server on the control connection AND presents this
		// identity; client auth is included so the same cert works if a future
		// reverse-connection mode lands.
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("workerca: sign machine CSR: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), expiry, nil
}

// OrchestratorMaterial returns the daemon's client cert/key PEM and the worker
// CA cert PEM — what a worker needs to verify the daemon (§3.1 step 2).
func (ca *CA) OrchestratorMaterial() (certPEM, keyPEM, caPEM []byte, err error) {
	if err := ca.ensureOrchestratorCert(); err != nil {
		return nil, nil, nil, err
	}
	certPEM, err = os.ReadFile(filepath.Join(ca.Dir, OrchestratorCertFile))
	if err != nil {
		return nil, nil, nil, err
	}
	keyPEM, err = os.ReadFile(filepath.Join(ca.Dir, OrchestratorKeyFile))
	if err != nil {
		return nil, nil, nil, err
	}
	return certPEM, keyPEM, ca.CertPEM, nil
}

// ---- enrolled-worker registry ----

// Worker is one enrolled machine.
type Worker struct {
	Name       string    `json:"name"`
	Address    string    `json:"address"`
	Serial     string    `json:"serial"` // hex; revoke targets this
	EnrolledAt time.Time `json:"enrolled_at"`
	NotAfter   time.Time `json:"not_after"`
	Revoked    bool      `json:"revoked,omitempty"`
	RevokedAt  time.Time `json:"revoked_at,omitzero"`
}

// Registry is the persisted enrolled-worker set.
type Registry struct {
	Workers []Worker `json:"workers"`
}

// LoadRegistry reads workers.json (empty registry if absent).
func (ca *CA) LoadRegistry() (*Registry, error) {
	data, err := os.ReadFile(filepath.Join(ca.Dir, RegistryFile))
	if os.IsNotExist(err) {
		return &Registry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workerca: read registry: %w", err)
	}
	var r Registry
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("workerca: parse registry: %w", err)
	}
	return &r, nil
}

// SaveRegistry writes workers.json atomically, sorted by name.
func (ca *CA) SaveRegistry(r *Registry) error {
	sort.Slice(r.Workers, func(i, j int) bool { return r.Workers[i].Name < r.Workers[j].Name })
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("workerca: marshal registry: %w", err)
	}
	tmp := filepath.Join(ca.Dir, registryTmp)
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("workerca: write registry: %w", err)
	}
	return os.Rename(tmp, filepath.Join(ca.Dir, RegistryFile))
}

// Record adds or replaces a worker entry (re-enrollment rotates in place).
func (ca *CA) Record(w Worker) error {
	r, err := ca.LoadRegistry()
	if err != nil {
		return err
	}
	replaced := false
	for i := range r.Workers {
		if r.Workers[i].Name == w.Name {
			r.Workers[i] = w
			replaced = true
			break
		}
	}
	if !replaced {
		r.Workers = append(r.Workers, w)
	}
	return ca.SaveRegistry(r)
}

// Revoke marks an enrolled worker revoked. The daemon refuses to dial a
// revoked worker; the serial is recorded so a future CRL/denylist can use it.
func (ca *CA) Revoke(name string) error {
	r, err := ca.LoadRegistry()
	if err != nil {
		return err
	}
	for i := range r.Workers {
		if r.Workers[i].Name == name {
			if r.Workers[i].Revoked {
				return fmt.Errorf("workerca: worker %q is already revoked", name)
			}
			r.Workers[i].Revoked = true
			r.Workers[i].RevokedAt = time.Now().UTC()
			return ca.SaveRegistry(r)
		}
	}
	return fmt.Errorf("workerca: no enrolled worker named %q", name)
}

// Lookup returns the registry entry for name.
func (ca *CA) Lookup(name string) (*Worker, error) {
	r, err := ca.LoadRegistry()
	if err != nil {
		return nil, err
	}
	for i := range r.Workers {
		if r.Workers[i].Name == name {
			return &r.Workers[i], nil
		}
	}
	return nil, fmt.Errorf("workerca: no enrolled worker named %q", name)
}

// LoadRegistryFrom reads the enrolled-worker registry from a material dir
// WITHOUT loading (or creating) the CA — for consumers that only need to
// consult enrollment state, such as the socket backend's revocation check.
func LoadRegistryFrom(dir string) (*Registry, error) {
	data, err := os.ReadFile(filepath.Join(dir, RegistryFile))
	if err != nil {
		return nil, err
	}
	var r Registry
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("workerca: parse registry: %w", err)
	}
	return &r, nil
}
