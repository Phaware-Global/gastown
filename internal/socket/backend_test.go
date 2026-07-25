package socket

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/execution"
	"github.com/steveyegge/gastown/internal/sockproto"
)

// fakeWorker is a minimal gt-worker-client stand-in for driving the backend:
// it speaks the §4 control protocol over a unix socket, records the messages
// it received, and behaves per a small script.
type fakeWorker struct {
	t    *testing.T
	ln   net.Listener
	addr string

	mu       sync.Mutex
	sessions []sockproto.SessionSummary
	received []string // message types, in order

	// knobs
	maxSessions  int
	skipCSR      bool   // reply session_ready directly (reattach-style / native no-cert path)
	failOpenWith string // non-empty → session_error code
	refuseHello  bool
	chatty       bool // interleave ping + empty-id stray messages before each reply
	hang         bool // accept + handshake, then never answer a request
}

func newFakeWorker(t *testing.T) *fakeWorker {
	t.Helper()
	// Unix socket paths are capped (~104 bytes on macOS), well under a deep
	// t.TempDir() path — use a short os.MkdirTemp under the system tmp root.
	dir, err := os.MkdirTemp("", "gtw")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "w.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	w := &fakeWorker{t: t, ln: ln, addr: "unix://" + sock, maxSessions: 1}
	go w.serve()
	t.Cleanup(func() { ln.Close() })
	return w
}

func (w *fakeWorker) settings() *Settings {
	return &Settings{Address: w.addr, TLS: TLSConfig{Mode: tlsModeNone}, Token: "t0k"}
}

func (w *fakeWorker) got() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.received...)
}

func (w *fakeWorker) serve() {
	for {
		c, err := w.ln.Accept()
		if err != nil {
			return
		}
		go w.handle(c)
	}
}

func (w *fakeWorker) record(t string) {
	w.mu.Lock()
	w.received = append(w.received, t)
	w.mu.Unlock()
}

func (w *fakeWorker) handle(nc net.Conn) {
	defer nc.Close()
	codec := sockproto.NewCodec(nc)
	// reply injects chatty noise (a ping + an empty-id stray) before the real
	// reply when the chatty knob is set, so the backend's exact-ID matcher is
	// exercised.
	reply := func(m *sockproto.Message) {
		if w.chatty {
			_ = codec.Send(&sockproto.Message{Type: sockproto.TypePing})
			_ = codec.Send(&sockproto.Message{Type: sockproto.TypeError, Code: "stray", Msg: "no id here"})
		}
		_ = codec.Send(m)
	}
	for {
		m, err := codec.Recv()
		if err != nil {
			return
		}
		w.record(m.Type)
		switch m.Type {
		case sockproto.TypeAuth:
			// token accepted silently
		case sockproto.TypeHello:
			if w.refuseHello {
				_ = codec.Send(&sockproto.Message{Type: sockproto.TypeError, Code: "proto", Msg: "nope"})
				return
			}
			w.mu.Lock()
			sess := append([]sockproto.SessionSummary(nil), w.sessions...)
			w.mu.Unlock()
			_ = codec.Send(&sockproto.Message{
				Type:         sockproto.TypeHelloAck,
				ProtoVersion: sockproto.ProtoVersion,
				WorkerID:     "fake",
				Capabilities: &sockproto.Capabilities{Docker: true, ExecModes: []string{"container", "native"}, MaxSessions: w.maxSessions},
				Sessions:     sess,
			})
		case sockproto.TypeSessionOpen:
			if w.hang {
				return // handshake done, but never answer the request
			}
			if w.failOpenWith != "" {
				reply(&sockproto.Message{Type: sockproto.TypeSessionError, ID: m.ID, Code: w.failOpenWith, Msg: "bad"})
				continue
			}
			if w.skipCSR {
				w.addSession(m)
				reply(&sockproto.Message{Type: sockproto.TypeSessionReady, ID: m.ID, Session: m.Session, RelayAddr: "127.0.0.1:9899"})
				continue
			}
			// Ask for a cert: emit a CSR (a fake PEM string is fine — the
			// backend just forwards it to the Signer).
			reply(&sockproto.Message{Type: sockproto.TypeCSR, ID: m.ID, Session: m.Session, CSRPEM: "-----FAKE CSR-----"})
		case sockproto.TypeCert:
			w.addSession(m)
			reply(&sockproto.Message{Type: sockproto.TypeSessionReady, ID: m.ID, Session: m.Session, RelayAddr: "127.0.0.1:9899"})
		case sockproto.TypeDiscover:
			if w.hang {
				return
			}
			w.mu.Lock()
			var out []sockproto.SessionSummary
			for _, s := range w.sessions {
				if m.Rig != "" && s.Rig != m.Rig {
					continue
				}
				if m.Polecat != "" && s.Polecat != m.Polecat {
					continue
				}
				out = append(out, s)
			}
			w.mu.Unlock()
			reply(&sockproto.Message{Type: sockproto.TypeSessions, ID: m.ID, Sessions: out})
		case sockproto.TypeShutdown:
			reply(&sockproto.Message{Type: sockproto.TypeShutdownComplete, ID: m.ID, Session: m.Session, CheckpointRef: "refs/checkpoints/polecat/" + m.Session})
		case sockproto.TypeTeardown:
			w.removeSession(m.Session)
			reply(&sockproto.Message{Type: sockproto.TypeTeardownComplete, ID: m.ID, Session: m.Session})
		default:
			_ = codec.SendErr(m.ID, "unknown", m.Type)
		}
	}
}

// newHangingWorker returns a worker that completes the handshake but never
// answers a control request (§ hung-worker timeout coverage).
func newHangingWorker(t *testing.T) *fakeWorker {
	w := newFakeWorker(t)
	w.hang = true
	return w
}

func (w *fakeWorker) addSession(m *sockproto.Message) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sessions = append(w.sessions, sockproto.SessionSummary{
		Session: m.Session, Rig: m.Rig, Polecat: m.Polecat, State: "ready", StartedAt: time.Unix(1, 0),
	})
}

func (w *fakeWorker) removeSession(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.sessions[:0]
	for _, s := range w.sessions {
		if s.Session != id {
			out = append(out, s)
		}
	}
	w.sessions = out
}

func (w *fakeWorker) preload(sess sockproto.SessionSummary) {
	w.mu.Lock()
	w.sessions = append(w.sessions, sess)
	w.mu.Unlock()
}

// stubSigner records the CN it was asked to sign and returns fake material.
type stubSigner struct {
	gotCN  string
	gotCSR []byte
	err    error
}

func (s *stubSigner) SignSessionCSR(_ context.Context, csrPEM []byte, cn string) ([]byte, []byte, time.Time, error) {
	s.gotCN = cn
	s.gotCSR = csrPEM
	if s.err != nil {
		return nil, nil, time.Time{}, s.err
	}
	return []byte("-----CERT-----"), []byte("-----CA-----"), time.Unix(2, 0), nil
}

func testBackend(t *testing.T, w *fakeWorker) (*SocketBackend, *stubSigner) {
	t.Helper()
	raw, err := json.Marshal(w.settings())
	require.NoError(t, err)
	cfg := &config.ExecutionConfig{
		Backend:  BackendName,
		ExecMode: config.ExecModeContainer,
		Image:    "dev:latest",
		Extensions: map[string]json.RawMessage{
			BackendName: raw,
		},
	}
	b, err := New(cfg)
	require.NoError(t, err)
	signer := &stubSigner{}
	b.Signer = signer
	b.OrchestratorID = "town-1"
	b.GTVersion = "test"
	return b, signer
}

func spec() execution.PolecatSpec {
	return execution.PolecatSpec{Rig: "MyRig", Polecat: "furiosa", Session: "gt-MyRig-furiosa"}
}

func TestProvision_FullCSRExchange(t *testing.T) {
	w := newFakeWorker(t)
	b, signer := testBackend(t, w)

	ep, err := b.Provision(context.Background(), spec())
	require.NoError(t, err)
	assert.Equal(t, BackendName, ep.Backend)
	assert.Equal(t, "gt-MyRig-furiosa", ep.ID)
	assert.Equal(t, w.addr, ep.Address)
	assert.Equal(t, "MyRig", ep.Identity.Rig)

	// The CSR was signed bound to the session's expected CN (§4.2 binding).
	assert.Equal(t, "gt-MyRig-furiosa", signer.gotCN)
	assert.Equal(t, "-----FAKE CSR-----", string(signer.gotCSR))

	// Message ordering: auth → hello → session_open → cert.
	assert.Equal(t, []string{"auth", "hello", "session_open", "cert"}, w.got())
}

func TestProvision_Reattach(t *testing.T) {
	w := newFakeWorker(t)
	w.preload(sockproto.SessionSummary{Session: "gt-MyRig-furiosa", Rig: "MyRig", Polecat: "furiosa", State: "ready"})
	b, signer := testBackend(t, w)

	ep, err := b.Provision(context.Background(), spec())
	require.NoError(t, err)
	assert.Equal(t, "gt-MyRig-furiosa", ep.ID)
	// Reattach: no session_open, no CSR signing.
	assert.Equal(t, "", signer.gotCN)
	assert.Equal(t, []string{"auth", "hello"}, w.got())
}

func TestProvision_OrphanedSessionIsNotReattached(t *testing.T) {
	w := newFakeWorker(t)
	w.preload(sockproto.SessionSummary{Session: "gt-MyRig-furiosa", Rig: "MyRig", Polecat: "furiosa", State: "orphaned"})
	w.maxSessions = 5 // don't trip the limit on the orphan
	b, _ := testBackend(t, w)

	_, err := b.Provision(context.Background(), spec())
	require.NoError(t, err)
	// An orphaned session is reaped, not reattached: a fresh session_open runs.
	assert.Contains(t, w.got(), "session_open")
}

func TestProvision_SessionLimit(t *testing.T) {
	w := newFakeWorker(t)
	w.maxSessions = 1
	w.preload(sockproto.SessionSummary{Session: "other", Rig: "MyRig", Polecat: "nux", State: "ready"})
	b, _ := testBackend(t, w)

	_, err := b.Provision(context.Background(), spec())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session limit")
}

func TestProvision_SessionError(t *testing.T) {
	w := newFakeWorker(t)
	w.failOpenWith = "bad_image"
	b, _ := testBackend(t, w)

	_, err := b.Provision(context.Background(), spec())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad_image")
}

func TestProvision_CSRWithoutSignerFails(t *testing.T) {
	w := newFakeWorker(t)
	b, _ := testBackend(t, w)
	b.Signer = nil

	_, err := b.Provision(context.Background(), spec())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no Signer")
}

func TestDiscover_FiltersByIdentity(t *testing.T) {
	w := newFakeWorker(t)
	w.preload(sockproto.SessionSummary{Session: "s1", Rig: "MyRig", Polecat: "furiosa", State: "ready"})
	w.preload(sockproto.SessionSummary{Session: "s2", Rig: "MyRig", Polecat: "nux", State: "ready"})
	w.preload(sockproto.SessionSummary{Session: "s3", Rig: "Other", Polecat: "max", State: "ready"})
	b, _ := testBackend(t, w)

	all, err := b.Discover(context.Background(), execution.IdentityTags{Rig: "MyRig"})
	require.NoError(t, err)
	assert.Len(t, all, 2)

	one, err := b.Discover(context.Background(), execution.IdentityTags{Rig: "MyRig", Polecat: "furiosa"})
	require.NoError(t, err)
	require.Len(t, one, 1)
	assert.Equal(t, "s1", one[0].ID)
	assert.Equal(t, BackendName, one[0].Backend)
}

func TestTeardown_ShutdownThenTeardown(t *testing.T) {
	w := newFakeWorker(t)
	w.preload(sockproto.SessionSummary{Session: "gt-MyRig-furiosa", Rig: "MyRig", Polecat: "furiosa", State: "ready"})
	b, _ := testBackend(t, w)

	err := b.Teardown(context.Background(), execution.Endpoint{Backend: BackendName, ID: "gt-MyRig-furiosa", Address: w.addr})
	require.NoError(t, err)
	got := w.got()
	assert.Contains(t, got, "shutdown")
	assert.Contains(t, got, "teardown")
	// Session gone from the worker afterward.
	eps, err := b.Discover(context.Background(), execution.IdentityTags{})
	require.NoError(t, err)
	assert.Empty(t, eps)
}

func TestProvision_HelloRefusedSurfaces(t *testing.T) {
	w := newFakeWorker(t)
	w.refuseHello = true
	b, _ := testBackend(t, w)

	_, err := b.Provision(context.Background(), spec())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refused")
}

func TestWrapCommand_LauncherArgv(t *testing.T) {
	w := newFakeWorker(t)
	b, _ := testBackend(t, w)
	ep := execution.Endpoint{Backend: BackendName, ID: "gt-MyRig-furiosa", Address: w.addr}

	argv, err := b.WrapCommand(ep, []string{"claude", "--model", "opus", "do it"}, map[string]string{"GT_ROLE": "polecat"})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"gt-worker-attach", "--address", w.addr, "--session", "gt-MyRig-furiosa",
		"--", "claude", "--model", "opus", "do it",
	}, argv)
}

func TestForConfig_ResolvesSocketBackend(t *testing.T) {
	raw, err := json.Marshal(&Settings{Address: "10.0.0.1:9878", TLS: TLSConfig{Mode: tlsModeAuto, WorkerName: "gpu-1"}})
	require.NoError(t, err)
	cfg := &config.ExecutionConfig{
		Backend:    BackendName,
		ExecMode:   config.ExecModeContainer,
		Image:      "dev:latest",
		Extensions: map[string]json.RawMessage{BackendName: raw},
	}
	be, err := execution.ForConfig(cfg)
	require.NoError(t, err)
	_, ok := be.(*SocketBackend)
	assert.True(t, ok, "ForConfig must resolve the registered socket backend, got %T", be)
}

func TestProvision_ContainerRequiresCSR(t *testing.T) {
	w := newFakeWorker(t)
	w.skipCSR = true          // worker jumps straight to session_ready, no CSR
	b, _ := testBackend(t, w) // testBackend uses exec_mode container

	_, err := b.Provision(context.Background(), spec())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "without a CSR exchange")
}

func TestProvision_NativeAllowsCertless(t *testing.T) {
	w := newFakeWorker(t)
	w.skipCSR = true
	raw, err := json.Marshal(w.settings())
	require.NoError(t, err)
	cfg := &config.ExecutionConfig{
		Backend:    BackendName,
		ExecMode:   config.ExecModeNative,
		Extensions: map[string]json.RawMessage{BackendName: raw},
	}
	b, err := New(cfg)
	require.NoError(t, err)

	ep, err := b.Provision(context.Background(), spec())
	require.NoError(t, err)
	assert.Equal(t, "gt-MyRig-furiosa", ep.ID)
}

func TestDiscover_BoundsHungWorker(t *testing.T) {
	// A worker that accepts the connection, handshakes, but never replies to
	// discover must not hang the caller (deadline-less ctx). The controlTimeout
	// self-defense bounds it; we shrink the socket deadline via a short ctx to
	// keep the test fast while still proving the un-hung path.
	w := newHangingWorker(t)
	b, _ := testBackend(t, w)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := b.Discover(ctx, execution.IdentityTags{})
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "discover must return promptly against a hung worker")
}

func TestProvision_SkipsAsyncTrafficMatchesExactID(t *testing.T) {
	w := newFakeWorker(t)
	w.chatty = true // interleave a ping and an empty-id stray before each reply
	b, signer := testBackend(t, w)

	ep, err := b.Provision(context.Background(), spec())
	require.NoError(t, err)
	assert.Equal(t, "gt-MyRig-furiosa", ep.ID)
	assert.Equal(t, "gt-MyRig-furiosa", signer.gotCN, "must still complete the CSR exchange through the noise")
}
