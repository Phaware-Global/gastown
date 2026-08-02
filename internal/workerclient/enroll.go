package workerclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/sockproto"
)

// Enrollment material file names, written into the worker's TLS dir.
const (
	MachineCertFile = "machine.crt"
	MachineKeyFile  = "machine.key"
	WorkerCAFile    = "worker-ca.crt"   // signs this machine's cert
	ClientCAFile    = "client-ca.crt"   // verifies the orchestrator's client cert
	EnrolledName    = "worker-name.txt" // the assigned enrolled name
)

// EnrollConfig configures a one-shot enrollment listener (§3.1 step 1).
type EnrollConfig struct {
	// TLSDir receives the machine keypair, the worker CA, the client CA, and
	// the assigned name.
	TLSDir string
	// JoinToken is the single-use, operator-carried token the orchestrator
	// must present. Required.
	JoinToken string
	Log       *slog.Logger
}

// Enroll runs ONE enrollment exchange on ln and returns when it completes (or
// ctx is canceled). The listener is plaintext by design: enrollment is a
// bootstrap where neither side yet has the other's CA, so trust comes from the
// single-use join token carried out-of-band by the operator (§3.1 step 2).
// The machine private key is generated HERE and never leaves the machine —
// only the CSR crosses.
//
// Enrollment is one-shot on purpose: the process exits enrollment mode as soon
// as one exchange succeeds, so a leaked token cannot be replayed against a
// still-listening enroller.
func Enroll(ctx context.Context, ln net.Listener, cfg EnrollConfig) error {
	if cfg.JoinToken == "" {
		return fmt.Errorf("workerclient: enrollment requires a join token")
	}
	if cfg.TLSDir == "" {
		return fmt.Errorf("workerclient: enrollment requires a TLS dir")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(cfg.TLSDir, 0700); err != nil {
		return fmt.Errorf("workerclient: create tls dir: %w", err)
	}

	// Track the in-flight connection so ctx cancellation closes IT too — a
	// peer that connects and says nothing would otherwise block the sequential
	// accept loop (and hang shutdown) until its deadline.
	var curMu sync.Mutex
	var cur net.Conn
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		curMu.Lock()
		if cur != nil {
			cur.Close()
		}
		curMu.Unlock()
	}()

	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("workerclient: enrollment accept: %w", err)
		}
		curMu.Lock()
		cur = nc
		curMu.Unlock()
		err = enrollOnce(nc, cfg, log)
		nc.Close()
		curMu.Lock()
		cur = nil
		curMu.Unlock()
		if err == nil {
			log.Info("enrollment complete; exiting enrollment mode")
			return nil
		}
		// A failed attempt (bad token, malformed exchange) does NOT end
		// enrollment — the operator may simply have fat-fingered the address —
		// but the token stays required for every attempt.
		log.Warn("enrollment attempt failed", "err", err)
	}
}

// enrollOnce runs the exchange on one connection.
func enrollOnce(nc net.Conn, cfg EnrollConfig, log *slog.Logger) error {
	// Bound the whole attempt: a silent peer must not wedge enrollment.
	_ = nc.SetDeadline(time.Now().Add(enrollAttemptTimeout))
	codec := sockproto.NewCodec(nc)

	m, err := codec.Recv()
	if err != nil {
		return err
	}
	if m.Type != sockproto.TypeEnroll {
		_ = codec.Send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Code: "proto", Msg: "expected enroll"})
		return fmt.Errorf("expected enroll, got %q", m.Type)
	}
	// Constant-time compare: the token is the only thing gating enrollment.
	if subtle.ConstantTimeCompare([]byte(m.JoinToken), []byte(cfg.JoinToken)) != 1 {
		_ = codec.Send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Code: "auth", Msg: "invalid join token"})
		return fmt.Errorf("invalid join token")
	}
	if !validEnrolledName(m.WorkerName) {
		_ = codec.Send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Code: "bad_request", Msg: "invalid worker name"})
		return fmt.Errorf("invalid worker name %q", m.WorkerName)
	}

	// Generate the machine keypair locally; only the CSR crosses the wire.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate machine key: %w", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: m.WorkerName}}, key)
	if err != nil {
		return fmt.Errorf("create machine CSR: %w", err)
	}
	if err := codec.Send(&sockproto.Message{
		Type:   sockproto.TypeEnrollCSR,
		ID:     m.ID,
		CSRPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})),
	}); err != nil {
		return err
	}

	resp, err := codec.Recv()
	if err != nil {
		return err
	}
	if resp.Type == sockproto.TypeError {
		return fmt.Errorf("orchestrator refused enrollment: %s: %s", resp.Code, resp.Msg)
	}
	if resp.Type != sockproto.TypeEnrollComplete {
		return fmt.Errorf("expected enroll_complete, got %q", resp.Type)
	}
	if resp.CertPEM == "" || resp.CAPEM == "" || resp.ClientCAPEM == "" {
		return fmt.Errorf("enroll_complete missing cert, worker CA, or client CA")
	}
	// The returned cert must actually match the key we just generated, chain
	// to the worker CA we were handed, and carry the name we were assigned —
	// otherwise we'd persist material we can never authenticate with.
	if err := verifyMachineCert(resp.CertPEM, resp.CAPEM, m.WorkerName, key); err != nil {
		return err
	}
	// ClientCAPEM becomes the anchor this worker trusts to authenticate FUTURE
	// orchestrator client certs — a far more durable grant than one session, so
	// validate it rather than persisting whatever arrived. In the standard flow
	// the same worker CA signs both, so require that; a split client CA would
	// be a deliberate protocol change, not something to accept silently.
	if err := verifyClientCA(resp.ClientCAPEM, resp.CAPEM); err != nil {
		return err
	}

	keyPEM, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal machine key: %w", err)
	}
	writes := []struct {
		name string
		data []byte
		perm os.FileMode
	}{
		{MachineKeyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyPEM}), 0600},
		{MachineCertFile, []byte(resp.CertPEM), 0644},
		{WorkerCAFile, []byte(resp.CAPEM), 0644},
		{ClientCAFile, []byte(resp.ClientCAPEM), 0644},
		{EnrolledName, []byte(m.WorkerName + "\n"), 0644},
	}
	for _, w := range writes {
		if err := writeFileAtomic(filepath.Join(cfg.TLSDir, w.name), w.data, w.perm); err != nil {
			return err
		}
	}

	// Material is on disk: this machine IS enrolled. A failed ack must not
	// leave the one-shot listener open for a second party holding the token to
	// re-drive enrollment and overwrite it — log and treat as terminal.
	if err := codec.Send(&sockproto.Message{Type: sockproto.TypeEnrollAck, ID: m.ID, WorkerName: m.WorkerName}); err != nil {
		log.Warn("enrollment material persisted but the ack could not be sent; treating enrollment as complete",
			"name", m.WorkerName, "err", err)
	}
	log.Info("enrolled", "name", m.WorkerName, "tlsDir", cfg.TLSDir)
	return nil
}

// verifyMachineCert checks the signed cert is usable before persisting it:
// it must parse, chain to the supplied worker CA, carry the assigned name, and
// match our private key.
func verifyMachineCert(certPEM, caPEM, name string, key *ecdsa.PrivateKey) error {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return fmt.Errorf("machine cert is not a CERTIFICATE PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse machine cert: %w", err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return fmt.Errorf("machine cert does not match the key generated here")
	}
	if cert.Subject.CommonName != name {
		return fmt.Errorf("machine cert CN %q != assigned name %q", cert.Subject.CommonName, name)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return fmt.Errorf("worker CA PEM has no valid certificates")
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   name,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("machine cert does not chain to the supplied worker CA: %w", err)
	}
	return nil
}

// validEnrolledName mirrors workerca.ValidWorkerName without importing the
// orchestrator-side package into the worker binary.
func validEnrolledName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 63 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case (r == '.' || r == '_' || r == '-') && i > 0:
		default:
			return false
		}
	}
	return true
}

// writeFileAtomic writes via a temp sibling + rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

// enrollAttemptTimeout bounds one enrollment attempt end-to-end.
const enrollAttemptTimeout = 60 * time.Second

// verifyClientCA checks the CA the worker is about to trust for future
// orchestrator client certs: it must parse, be a CA, and (in the standard
// single-CA flow) be the same CA that signed this machine's cert.
func verifyClientCA(clientCAPEM, workerCAPEM string) error {
	block, _ := pem.Decode([]byte(clientCAPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return fmt.Errorf("client CA is not a CERTIFICATE PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse client CA: %w", err)
	}
	if !cert.IsCA {
		return fmt.Errorf("client CA certificate is not a CA")
	}
	if clientCAPEM != workerCAPEM {
		return fmt.Errorf("client CA does not match the worker CA; refusing to trust a separate client-auth anchor")
	}
	return nil
}
