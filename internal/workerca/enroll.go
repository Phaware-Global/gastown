package workerca

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"time"

	"github.com/steveyegge/gastown/internal/sockproto"
)

// enrollTimeout bounds the whole enrollment exchange.
const enrollTimeout = 60 * time.Second

// NewJoinToken returns a fresh 256-bit join token, hex-encoded — the
// out-of-band secret the operator carries to the worker (§3.1 step 2).
func NewJoinToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("workerca: generate join token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// EnrollWorker performs the orchestrator half of enrollment against a worker
// listening in enrollment mode (§3.1 step 2): connect, present the join token
// and the assigned name, receive the machine CSR, sign it with the worker CA,
// and return the machine cert plus both CAs. On success the worker is recorded
// in the registry.
//
// The connection is plaintext by design — this is the bootstrap where neither
// side has the other's CA yet; the single-use, operator-carried join token is
// what authenticates it. Nothing secret crosses: the machine key stays on the
// worker, and everything the daemon sends (certs, CA certs) is public
// material.
func (ca *CA) EnrollWorker(ctx context.Context, name, address, joinToken string) (*Worker, error) {
	if !ValidWorkerName(name) {
		return nil, fmt.Errorf("workerca: invalid worker name %q (want [A-Za-z0-9][A-Za-z0-9._-]{0,62})", name)
	}
	if joinToken == "" {
		return nil, fmt.Errorf("workerca: a join token is required")
	}
	orchCert, _, caPEM, err := ca.OrchestratorMaterial()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, enrollTimeout)
	defer cancel()

	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("workerca: dial worker %s: %w", address, err)
	}
	defer nc.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
	}
	codec := sockproto.NewCodec(nc)

	if err := codec.Send(&sockproto.Message{
		Type:       sockproto.TypeEnroll,
		ID:         "enroll",
		JoinToken:  joinToken,
		WorkerName: name,
	}); err != nil {
		return nil, err
	}

	m, err := codec.Recv()
	if err != nil {
		return nil, fmt.Errorf("workerca: awaiting machine CSR: %w", err)
	}
	if m.Type == sockproto.TypeError {
		return nil, fmt.Errorf("workerca: worker refused enrollment: %s: %s", m.Code, m.Msg)
	}
	if m.Type != sockproto.TypeEnrollCSR || m.CSRPEM == "" {
		return nil, fmt.Errorf("workerca: expected enroll_csr, got %q", m.Type)
	}

	// The CN is bound to the operator-chosen name here, not taken from the CSR.
	certPEM, notAfter, err := ca.SignMachineCSR([]byte(m.CSRPEM), name)
	if err != nil {
		_ = codec.Send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Code: "sign", Msg: err.Error()})
		return nil, err
	}
	if err := codec.Send(&sockproto.Message{
		Type:        sockproto.TypeEnrollComplete,
		ID:          m.ID,
		WorkerName:  name,
		CertPEM:     string(certPEM),
		CAPEM:       string(caPEM),
		ClientCAPEM: string(caPEM), // the worker CA also signs the orchestrator client cert
		NotAfter:    notAfter,
	}); err != nil {
		return nil, err
	}
	_ = orchCert // presented on future connections, not during enrollment

	ack, err := codec.Recv()
	if err != nil {
		return nil, fmt.Errorf("workerca: awaiting enrollment ack: %w", err)
	}
	if ack.Type == sockproto.TypeError {
		return nil, fmt.Errorf("workerca: worker rejected material: %s: %s", ack.Code, ack.Msg)
	}
	if ack.Type != sockproto.TypeEnrollAck {
		return nil, fmt.Errorf("workerca: expected enroll_ack, got %q", ack.Type)
	}

	serial, err := certSerialHex(certPEM)
	if err != nil {
		return nil, err
	}
	w := Worker{
		Name:       name,
		Address:    address,
		Serial:     serial,
		EnrolledAt: time.Now().UTC(),
		NotAfter:   notAfter,
	}
	if err := ca.Record(w); err != nil {
		return nil, err
	}
	return &w, nil
}
