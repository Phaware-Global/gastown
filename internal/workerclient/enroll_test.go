package workerclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/execution"
	"github.com/steveyegge/gastown/internal/socket"
	"github.com/steveyegge/gastown/internal/sockproto"
	"github.com/steveyegge/gastown/internal/workerca"
)

// startEnrollListener runs the worker's one-shot enrollment mode on a
// loopback port and returns its address plus a channel carrying the result.
func startEnrollListener(t *testing.T, tlsDir, token string) (string, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- Enroll(ctx, ln, EnrollConfig{TLSDir: tlsDir, JoinToken: token}) }()
	return ln.Addr().String(), done
}

// TestEnrollmentEndToEnd runs the REAL §3.1 exchange between the real worker
// CA and the real worker-side enroll listener, then proves the enrolled
// material actually establishes a mutual-TLS control connection carrying a
// full session — i.e. enrollment produces usable mTLS, not just files.
func TestEnrollmentEndToEnd(t *testing.T) {
	caDir := t.TempDir()
	ca, err := workerca.LoadOrCreate(caDir)
	require.NoError(t, err)

	workerTLS := t.TempDir()
	token, err := workerca.NewJoinToken()
	require.NoError(t, err)
	addr, enrollDone := startEnrollListener(t, workerTLS, token)

	// ---- orchestrator side of enrollment ----
	w, err := ca.EnrollWorker(context.Background(), "gpu-box-1", addr, token)
	require.NoError(t, err)
	assert.Equal(t, "gpu-box-1", w.Name)
	assert.NotEmpty(t, w.Serial)
	require.NoError(t, <-enrollDone, "worker must exit enrollment mode cleanly")

	// Worker persisted its material; the private key stayed local (0600).
	for _, f := range []string{MachineCertFile, MachineKeyFile, WorkerCAFile, ClientCAFile, EnrolledName} {
		_, statErr := os.Stat(filepath.Join(workerTLS, f))
		require.NoError(t, statErr, "worker must persist %s", f)
	}
	keyInfo, err := os.Stat(filepath.Join(workerTLS, MachineKeyFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), keyInfo.Mode().Perm(), "machine key must be owner-only")

	// Registry records it as active.
	reg, err := ca.LoadRegistry()
	require.NoError(t, err)
	require.Len(t, reg.Workers, 1)
	assert.False(t, reg.Workers[0].Revoked)

	// ---- the enrolled material must actually work for mTLS ----
	proxyURL, proxyCA, _ := startProxy(t)

	machineCert, err := tls.LoadX509KeyPair(
		filepath.Join(workerTLS, MachineCertFile), filepath.Join(workerTLS, MachineKeyFile))
	require.NoError(t, err)
	clientCAPEM, err := os.ReadFile(filepath.Join(workerTLS, ClientCAFile))
	require.NoError(t, err)
	clientPool := x509.NewCertPool()
	require.True(t, clientPool.AppendCertsFromPEM(clientCAPEM))

	svc, err := New(Config{
		WorkerID: "gpu-box-1", StateDir: filepath.Join(t.TempDir(), "state"),
		ProxyURL: proxyURL, ExecModes: []string{"native"}, MaxSessions: 1,
	})
	require.NoError(t, err)

	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	tlsLn := tls.NewListener(tcp, &tls.Config{
		Certificates: []tls.Certificate{machineCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientPool,
		MinVersion:   tls.VersionTLS13,
	})
	svcCtx, svcCancel := context.WithCancel(context.Background())
	t.Cleanup(svcCancel)
	go func() { _ = svc.Serve(svcCtx, tlsLn) }()

	// The backend dials in auto-TLS mode, reading the enrollment-managed
	// material from the CA dir and pinning ServerName to the enrolled name.
	t.Setenv("GT_WORKER_CA_DIR", caDir)
	raw, err := json.Marshal(map[string]any{
		"address": tcp.Addr().String(),
		"tls":     map[string]string{"mode": "auto", "worker_name": "gpu-box-1"},
	})
	require.NoError(t, err)
	cfg := &config.ExecutionConfig{
		Backend: socket.BackendName, ExecMode: config.ExecModeNative,
		CheckpointStr: "2s",
		Extensions:    map[string]json.RawMessage{socket.BackendName: raw},
	}
	b, err := socket.New(cfg)
	require.NoError(t, err)
	b.Signer = &caSigner{ca: proxyCA}
	b.OrchestratorID = "town-test"

	ep, err := b.Provision(context.Background(), polecatSpec())
	require.NoError(t, err, "enrolled mTLS material must carry a real session")
	assert.Equal(t, "gt-demo-furiosa", ep.ID)

	// ---- revocation cuts the worker off ----
	require.NoError(t, ca.Revoke("gpu-box-1"))
	_, err = b.Discover(context.Background(), execution.IdentityTags{})
	require.Error(t, err, "a revoked worker must not be dialed")
	assert.Contains(t, err.Error(), "revoked")
}

func TestEnroll_RejectsBadToken(t *testing.T) {
	caDir := t.TempDir()
	ca, err := workerca.LoadOrCreate(caDir)
	require.NoError(t, err)
	workerTLS := t.TempDir()
	addr, _ := startEnrollListener(t, workerTLS, "the-right-token")

	_, err = ca.EnrollWorker(context.Background(), "gpu-box-1", addr, "WRONG")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "join token")

	// Nothing was persisted, and the listener stays up for the operator to
	// retry with the correct token.
	_, statErr := os.Stat(filepath.Join(workerTLS, MachineCertFile))
	assert.True(t, os.IsNotExist(statErr), "a failed enrollment must persist nothing")

	w, err := ca.EnrollWorker(context.Background(), "gpu-box-1", addr, "the-right-token")
	require.NoError(t, err, "a correct token must still enroll after a failed attempt")
	assert.Equal(t, "gpu-box-1", w.Name)
}

func TestEnroll_RejectsInvalidName(t *testing.T) {
	workerTLS := t.TempDir()
	addr, _ := startEnrollListener(t, workerTLS, "tok")

	// Drive the wire directly: the orchestrator helper validates names before
	// dialing, so this exercises the WORKER's own validation.
	nc, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer nc.Close()
	codec := sockproto.NewCodec(nc)
	require.NoError(t, codec.Send(&sockproto.Message{
		Type: sockproto.TypeEnroll, ID: "e", JoinToken: "tok", WorkerName: "../../etc/passwd",
	}))
	resp, err := codec.Recv()
	require.NoError(t, err)
	assert.Equal(t, sockproto.TypeError, resp.Type)
	assert.Equal(t, "bad_request", resp.Code)
	_, statErr := os.Stat(filepath.Join(workerTLS, MachineCertFile))
	assert.True(t, os.IsNotExist(statErr))
}

func TestEnroll_RejectsMismatchedCert(t *testing.T) {
	// A malicious/buggy orchestrator returning a cert that doesn't match the
	// key generated here (or doesn't chain to the supplied CA) must be
	// rejected before anything is persisted.
	workerTLS := t.TempDir()
	addr, _ := startEnrollListener(t, workerTLS, "tok")

	otherCA, err := workerca.LoadOrCreate(t.TempDir())
	require.NoError(t, err)

	nc, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer nc.Close()
	codec := sockproto.NewCodec(nc)
	require.NoError(t, codec.Send(&sockproto.Message{
		Type: sockproto.TypeEnroll, ID: "e", JoinToken: "tok", WorkerName: "gpu-box-1",
	}))
	csr, err := codec.Recv()
	require.NoError(t, err)
	require.Equal(t, sockproto.TypeEnrollCSR, csr.Type)

	// Sign a cert for a DIFFERENT key (a fresh CSR), so it can't match the
	// worker's key, and hand back a CA that doesn't match it either.
	badCert, _, err := otherCA.SignMachineCSR(freshCSR(t), "gpu-box-1")
	require.NoError(t, err)
	require.NoError(t, codec.Send(&sockproto.Message{
		Type: sockproto.TypeEnrollComplete, ID: csr.ID,
		CertPEM: string(badCert), CAPEM: string(otherCA.CertPEM), ClientCAPEM: string(otherCA.CertPEM),
	}))

	// The worker must not ack, and must persist nothing.
	_ = nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = codec.Recv()
	assert.Error(t, err, "worker must not ack a mismatched cert")
	_, statErr := os.Stat(filepath.Join(workerTLS, MachineCertFile))
	assert.True(t, os.IsNotExist(statErr), "a rejected cert must not be persisted")
}

// freshCSR builds a CSR for a throwaway key (used to mint a cert that cannot
// match the worker's own key).
func freshCSR(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: "x"}}, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

// TestWorkerRejectsPeerMachineCert pins the round-1 HIGH end-to-end: a
// SECOND enrolled worker's machine cert must NOT be able to drive this
// worker's control plane. Two independent barriers must hold — machine certs
// are ServerAuth-only, and the accept side pins the orchestrator CN.
func TestWorkerRejectsPeerMachineCert(t *testing.T) {
	caDir := t.TempDir()
	ca, err := workerca.LoadOrCreate(caDir)
	require.NoError(t, err)

	// Enroll the victim worker and a peer worker.
	victimTLS, peerTLS := t.TempDir(), t.TempDir()
	for _, e := range []struct{ name, dir string }{{"victim", victimTLS}, {"peer", peerTLS}} {
		tok, err := workerca.NewJoinToken()
		require.NoError(t, err)
		addr, done := startEnrollListener(t, e.dir, tok)
		_, err = ca.EnrollWorker(context.Background(), e.name, addr, tok)
		require.NoError(t, err)
		require.NoError(t, <-done)
	}

	// Victim listens with the enrolled material (CN-pinned accept side).
	tlsCfg, err := ServerTLSConfig(
		filepath.Join(victimTLS, MachineCertFile),
		filepath.Join(victimTLS, MachineKeyFile),
		filepath.Join(victimTLS, ClientCAFile))
	require.NoError(t, err)
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer tcp.Close()
	ln := tls.NewListener(tcp, tlsCfg)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Force the handshake so verification actually runs.
			if tc, ok := c.(*tls.Conn); ok {
				_ = tc.Handshake()
			}
			c.Close()
		}
	}()

	// The PEER dials the victim, presenting its own machine cert as a client
	// cert — the impersonation attempt.
	peerCert, err := tls.LoadX509KeyPair(
		filepath.Join(peerTLS, MachineCertFile), filepath.Join(peerTLS, MachineKeyFile))
	require.NoError(t, err)
	caPEM, err := os.ReadFile(filepath.Join(peerTLS, WorkerCAFile))
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(caPEM))

	conn, err := tls.Dial("tcp", tcp.Addr().String(), &tls.Config{
		Certificates: []tls.Certificate{peerCert},
		RootCAs:      pool,
		ServerName:   "victim",
		MinVersion:   tls.VersionTLS13,
	})
	if err == nil {
		// TLS 1.3 can defer the peer's verdict to the first read.
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err = conn.Read(make([]byte, 1))
		conn.Close()
	}
	require.Error(t, err, "a peer worker's machine cert must not authenticate as the orchestrator")
}

// TestServerTLSConfig_AcceptsOnlyOrchestratorCN unit-checks the pin.
func TestServerTLSConfig_AcceptsOnlyOrchestratorCN(t *testing.T) {
	assert.Equal(t, workerca.OrchestratorCN, OrchestratorCN,
		"the worker's pinned CN must match what the CA mints")
}

// TestEnroll_SilentPeerDoesNotWedge pins the enroll deadline + conn tracking:
// a peer that connects and says nothing must not block enrollment forever,
// and ctx cancellation must unblock shutdown.
func TestEnroll_SilentPeerDoesNotWedge(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Enroll(ctx, ln, EnrollConfig{TLSDir: t.TempDir(), JoinToken: "tok"})
	}()

	// A silent connection occupies the sequential accept loop.
	silent, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer silent.Close()
	time.Sleep(200 * time.Millisecond)

	// Cancellation must unblock the stuck Recv (the accepted conn is closed
	// too, not just the listener).
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a silent peer wedged enrollment: ctx cancel did not unblock it")
	}
}
