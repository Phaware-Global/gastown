package socket

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/sockproto"
	"github.com/steveyegge/gastown/internal/workerca"
)

// dialTimeout bounds establishing the control connection.
const dialTimeout = 15 * time.Second

// conn is a control connection to a gt-worker-client, guarding the sequential
// request/response protocol with a mutex (§4: control messages are
// sequential by design).
type conn struct {
	mu    sync.Mutex
	nc    net.Conn
	codec *sockproto.Codec
	ack   *sockproto.Message // hello_ack captured at handshake (capabilities + sessions)
	nonce int
}

// dial establishes and handshakes a control connection per §3.2 (mTLS/unix)
// and §3.3 (unix token), leaving it ready for session messages.
func dial(ctx context.Context, s *Settings, orchestratorID, gtVersion string) (*conn, error) {
	nc, err := dialTransport(ctx, s)
	if err != nil {
		return nil, err
	}
	c := &conn{nc: nc, codec: sockproto.NewCodec(nc)}

	// §3.3: on a unix socket in token mode, auth is the first message.
	if s.tlsMode() == tlsModeNone {
		if err := c.codec.Send(&sockproto.Message{Type: sockproto.TypeAuth, Token: s.Token}); err != nil {
			nc.Close()
			return nil, err
		}
	}

	// §3.2 handshake: hello → hello_ack (with version negotiation).
	if err := c.codec.Send(&sockproto.Message{
		Type:           sockproto.TypeHello,
		ProtoVersion:   sockproto.ProtoVersion,
		GTVersion:      gtVersion,
		OrchestratorID: orchestratorID,
	}); err != nil {
		nc.Close()
		return nil, err
	}
	ack, err := c.codec.Recv()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("socket: handshake: %w", err)
	}
	if ack.Type == sockproto.TypeError {
		nc.Close()
		return nil, fmt.Errorf("socket: worker refused connection: %s: %s", ack.Code, ack.Msg)
	}
	if ack.Type != sockproto.TypeHelloAck {
		nc.Close()
		return nil, fmt.Errorf("socket: expected hello_ack, got %q", ack.Type)
	}
	if ack.ProtoVersion != sockproto.ProtoVersion {
		nc.Close()
		return nil, fmt.Errorf("socket: worker speaks proto version %d, orchestrator speaks %d", ack.ProtoVersion, sockproto.ProtoVersion)
	}
	c.ack = ack
	return c, nil
}

// dialTransport opens the raw connection: unix socket, or TCP with the §3
// TLS material.
func dialTransport(ctx context.Context, s *Settings) (net.Conn, error) {
	d := net.Dialer{Timeout: dialTimeout}
	if s.isUnix() {
		return d.DialContext(ctx, "unix", s.unixPath())
	}
	// Refuse a revoked machine before presenting any credential: revocation
	// (`gt worker revoke`) must cut a worker off immediately, not merely when
	// its cert eventually expires (§3.1).
	if err := checkNotRevoked(s.tlsMode(), s.TLS.WorkerName); err != nil {
		return nil, err
	}
	tlsCfg, err := clientTLS(s)
	if err != nil {
		return nil, err
	}
	td := tls.Dialer{NetDialer: &d, Config: tlsCfg}
	return td.DialContext(ctx, "tcp", s.Address)
}

// checkNotRevoked consults the enrolled-worker registry before an auto-TLS
// dial. It FAILS CLOSED on a present-but-unreadable registry: a corrupted,
// truncated, or permission-broken workers.json must not silently disable
// revocation for the whole fleet. The only tolerated absence is a registry
// that genuinely does not exist (manual-TLS deployments never create one).
//
// mode is the effective TLS mode; only auto mode is registry-managed.
func checkNotRevoked(mode, name string) error {
	if name == "" || mode != tlsModeAuto {
		return nil
	}
	dir, err := autoTLSDir()
	if err != nil {
		return fmt.Errorf("socket: cannot resolve the worker CA dir to check revocation: %w", err)
	}
	reg, err := workerca.LoadRegistryFrom(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// No registry at all: nothing has ever been enrolled here.
			return nil
		}
		return fmt.Errorf("socket: cannot read the enrolled-worker registry to check revocation (refusing to dial): %w", err)
	}
	for _, w := range reg.Workers {
		if w.Name == name && w.Revoked {
			return fmt.Errorf("socket: worker %q is revoked (re-enroll with `gt worker enroll %s` to restore it)", name, name)
		}
	}
	return nil
}

// clientTLS builds the mutual-TLS config for a TCP worker (§3): present the
// orchestrator client cert, verify the worker against the worker CA, pinned
// to the enrolled worker name.
func clientTLS(s *Settings) (*tls.Config, error) {
	caFile, certFile, keyFile := s.TLS.CAFile, s.TLS.CertFile, s.TLS.KeyFile
	if s.tlsMode() == tlsModeAuto {
		dir, err := autoTLSDir()
		if err != nil {
			return nil, err
		}
		// Names come from workerca so the enrollment writer and this reader
		// can never drift apart.
		caFile = filepath.Join(dir, workerca.WorkerCACertFile)
		certFile = filepath.Join(dir, workerca.OrchestratorCertFile)
		keyFile = filepath.Join(dir, workerca.OrchestratorKeyFile)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("socket: load orchestrator client cert (run `gt worker enroll`?): %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("socket: read worker CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("socket: worker CA file %s has no valid certificates", caFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
		// Pin the enrolled machine identity: the worker cert's CN must be the
		// name the operator enrolled, so a different machine presenting a
		// worker-CA-signed cert is still rejected.
		ServerName: s.TLS.WorkerName,
	}, nil
}

// request sends a request with a fresh nonce and returns the first response
// echoing that ID (skipping unrelated async messages like pong).
func (c *conn) request(ctx context.Context, req *sockproto.Message) (*sockproto.Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.nonce++
	req.ID = fmt.Sprintf("r%d", c.nonce)
	if dl, ok := ctx.Deadline(); ok {
		_ = c.nc.SetDeadline(dl)
		defer func() { _ = c.nc.SetDeadline(time.Time{}) }()
	}
	if err := c.codec.Send(req); err != nil {
		return nil, err
	}
	// The reply must echo our exact nonce. Ping/pong keepalive and any other
	// async worker traffic (including a message that omits the id) is skipped
	// — never mistaken for the reply. The socket deadline set above bounds
	// this loop, so a flood of non-matching messages cannot spin forever.
	for {
		resp, err := c.codec.Recv()
		if err != nil {
			return nil, err
		}
		if resp.ID == req.ID {
			return resp, nil
		}
		// else: keepalive or stray async message — keep reading for our reply.
	}
}

// sendOnly writes a message that expects no reply — push_binaries chunks,
// which stream without a round trip per chunk. It takes the same lock as
// request so a chunk can never interleave with another exchange.
func (c *conn) sendOnly(ctx context.Context, m *sockproto.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.nc.SetWriteDeadline(dl)
		defer c.nc.SetWriteDeadline(time.Time{})
	}
	return c.codec.Send(m)
}

// close shuts the control connection.
func (c *conn) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nc.Close()
}

// autoTLSDir returns the enrollment-managed material directory (§8 auto
// mode): $GT_WORKER_CA_DIR or ~/.gt/worker-ca.
func autoTLSDir() (string, error) {
	if d := os.Getenv("GT_WORKER_CA_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("socket: resolve home for worker-ca dir: %w", err)
	}
	return filepath.Join(home, ".gt", "worker-ca"), nil
}
