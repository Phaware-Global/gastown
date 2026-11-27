package socket

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/execution"
	"github.com/steveyegge/gastown/internal/sockproto"
	"github.com/steveyegge/gastown/internal/worker"
)

// fakeWorker is a minimal gt-worker-client stand-in for driving the backend:
// it speaks the §4 control protocol over a unix socket, records the messages
// it received, and behaves per a small script.
type fakeWorker struct {
	t    *testing.T
	ln   net.Listener
	addr string

	stop chan struct{} // closed at cleanup; hang blocks on it (conn stays open)

	mu       sync.Mutex
	sessions []sockproto.SessionSummary
	received []string // message types, in order

	// push_binaries state: what arrived, per name.
	pushes  map[string]*pushedBinary
	pushBuf map[string][]byte

	// restartingUntilAttempt makes the worker answer session_open with the
	// transient "restarting" refusal for the first N attempts.
	restartingUntilAttempt int
	openAttempts           int
	containerPlatform      string
	containerClient        string
	noAgentFiles           bool // an older worker: hello_ack lacks the agent_files capability
	// stageWorkerClient makes a pushed gt-worker-client "staged" rather than
	// installed, as a running worker does. The upgrade takes effect for the
	// next handshake: it then reports agent_files whatever noAgentFiles says.
	stageWorkerClient bool
	upgraded          bool

	// knobs
	refusePush   string // non-empty → answer a terminal push chunk with this error code
	gtVersion    string
	os, arch     string
	maxSessions  int
	skipCSR      bool   // reply session_ready directly (reattach-style / native no-cert path)
	failOpenWith string // non-empty → session_error code
	refuseHello  bool
	chatty       bool // interleave ping + empty-id stray messages before each reply
	hang         bool // accept + handshake, then never answer a request

	openBranch string // branch of the last session_open
	// exitOnTeardown: the teardown ends the session and then drops the
	// connection unanswered, as a worker does when it exits to apply a staged
	// binary once its last session is gone.
	exitOnTeardown bool
}

// pushedBinary is a completed push_binaries transfer as the fake worker saw it.
type pushedBinary struct {
	data []byte
	sha  string
}

// handlePush accumulates chunks and answers the terminal one, mirroring the
// real worker's contract closely enough to test the sender.
func (w *fakeWorker) handlePush(codec *sockproto.Codec, m *sockproto.Message) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pushBuf == nil {
		w.pushBuf = map[string][]byte{}
		w.pushes = map[string]*pushedBinary{}
	}
	key := m.Name
	if m.Platform != "" {
		key = m.Platform + "/" + m.Name
	}
	if m.Data != "" {
		chunk, err := base64.StdEncoding.DecodeString(m.Data)
		require.NoError(w.t, err)
		w.pushBuf[key] = append(w.pushBuf[key], chunk...)
	}
	if !m.EOF {
		return
	}
	if w.refusePush != "" {
		_ = codec.Send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Code: w.refusePush, Msg: "refused"})
		return
	}
	w.pushes[key] = &pushedBinary{data: w.pushBuf[key], sha: m.SHA256}
	delete(w.pushBuf, key)
	applied := "installed"
	if w.stageWorkerClient && m.Name == "gt-worker-client" {
		applied = "staged"
		// As on a real worker, a staged binary is applied only once no
		// session is left.
		w.upgraded = len(w.sessions) == 0
	}
	_ = codec.Send(&sockproto.Message{Type: sockproto.TypePushBinaryAck, ID: m.ID,
		Name: m.Name, Platform: m.Platform, Applied: applied})
}

// sessionOpenAttempts reports how many session_opens arrived.
func (w *fakeWorker) sessionOpenAttempts() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.openAttempts
}

// pushed returns the completed transfers.
func (w *fakeWorker) pushed() map[string]*pushedBinary {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]*pushedBinary{}
	for k, v := range w.pushes {
		out[k] = v
	}
	return out
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
	w := &fakeWorker{t: t, ln: ln, addr: "unix://" + sock, maxSessions: 1, stop: make(chan struct{})}
	go w.serve()
	t.Cleanup(func() { close(w.stop); ln.Close() })
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
			agentFiles := !w.noAgentFiles || w.upgraded
			w.mu.Unlock()
			workerOS, workerArch := w.os, w.arch
			if workerOS == "" {
				workerOS, workerArch = runtime.GOOS, runtime.GOARCH
			}
			_ = codec.Send(&sockproto.Message{
				Type:         sockproto.TypeHelloAck,
				ProtoVersion: sockproto.ProtoVersion,
				WorkerID:     "fake",
				GTVersion:    w.gtVersion,
				OS:           workerOS,
				Arch:         workerArch,
				Capabilities: &sockproto.Capabilities{Docker: true, ExecModes: []string{"container", "native"},
					MaxSessions: w.maxSessions, ContainerPlatform: w.containerPlatform,
					ContainerClient: w.containerClient, AgentFiles: agentFiles},
				Sessions: sess,
			})
		case sockproto.TypePushBinary:
			w.handlePush(codec, m)
		case sockproto.TypeSessionOpen:
			w.mu.Lock()
			w.openAttempts++
			w.openBranch = m.Branch
			restarting := w.openAttempts <= w.restartingUntilAttempt
			w.mu.Unlock()
			if restarting {
				_ = codec.Send(&sockproto.Message{Type: sockproto.TypeSessionError, ID: m.ID, Session: m.Session,
					Code: "restarting", Msg: "worker is restarting onto a new binary; retry"})
				continue
			}
			if w.hang {
				<-w.stop // handshake done, but never answer — keep the conn open
				return
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
				<-w.stop
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
			if w.hang {
				<-w.stop
				return
			}
			reply(&sockproto.Message{Type: sockproto.TypeShutdownComplete, ID: m.ID, Session: m.Session, CheckpointRef: "refs/checkpoints/polecat/" + m.Session})
		case sockproto.TypeTeardown:
			if w.hang {
				<-w.stop
				return
			}
			w.removeSession(m.Session)
			if w.exitOnTeardown {
				w.mu.Lock()
				w.upgraded = true
				w.mu.Unlock()
				return
			}
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

// stubSigner records the identity it was asked to sign for and returns fake
// material.
type stubSigner struct {
	gotCN  string
	gotCSR []byte
	err    error
}

func (s *stubSigner) SignSessionCSR(_ context.Context, csrPEM []byte, rig, polecat string) ([]byte, []byte, time.Time, error) {
	s.gotCN = worker.PolecatCN(rig, polecat)
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

// The session id names the polecat, so a polecat started on its next bead
// finds the session of the previous one still live on the worker. Reattaching
// would leave the agent in the old worktree on the old branch.
func TestProvision_LiveSessionOnTheSameBranchIsReused(t *testing.T) {
	w := newFakeWorker(t)
	w.preload(sockproto.SessionSummary{Session: "gt-MyRig-furiosa", Rig: "MyRig", Polecat: "furiosa", State: "ready", Branch: "polecat/furiosa/gt-a"})
	b, _ := testBackend(t, w)

	s := spec()
	s.Branch = "polecat/furiosa/gt-a"
	_, err := b.Provision(context.Background(), s)
	require.NoError(t, err)
	assert.Equal(t, []string{"auth", "hello"}, w.got())
}

func TestProvision_LiveSessionOnAnotherBranchIsReplaced(t *testing.T) {
	for name, held := range map[string]string{
		"another branch":              "polecat/furiosa/gt-a",
		"no branch (an older worker)": "",
	} {
		t.Run(name, func(t *testing.T) {
			w := newFakeWorker(t) // maxSessions 1: the replaced session must not count
			w.preload(sockproto.SessionSummary{Session: "gt-MyRig-furiosa", Rig: "MyRig", Polecat: "furiosa", State: "ready", Branch: held})
			b, _ := testBackend(t, w)

			s := spec()
			s.Branch = "polecat/furiosa/gt-b"
			_, err := b.Provision(context.Background(), s)
			require.NoError(t, err)
			// Flushed and torn down first, then opened afresh on the new branch.
			assert.Equal(t, []string{"auth", "hello", "shutdown", "teardown", "session_open", "cert"}, w.got())
			w.mu.Lock()
			defer w.mu.Unlock()
			assert.Equal(t, "polecat/furiosa/gt-b", w.openBranch)
		})
	}
}

// Tearing the stale session down empties a worker that has a binary staged,
// by this provision or an earlier one. It exits to apply it before answering
// the teardown and is not listening until its supervisor has it back. That is
// a restart to retry through, not a failed provision.
func TestProvision_ReplacementRetriesThroughTheWorkersUpgradeRestart(t *testing.T) {
	for name, staged := range map[string]bool{"staged by this provision": true, "staged earlier": false} {
		t.Run(name, func(t *testing.T) {
			b, w, s := stagedUpgradeBackend(t)
			if !staged {
				b.GTVersion = w.gtVersion // nothing to push
			}
			w.exitOnTeardown = true
			w.preload(sockproto.SessionSummary{Session: "gt-MyRig-furiosa", Rig: "MyRig", Polecat: "furiosa", State: "ready", Branch: "polecat/furiosa/gt-a"})
			dials := 0
			b.dialFn = func(ctx context.Context, st *Settings, id, ver string) (*conn, error) {
				dials++
				if dials == 2 {
					return nil, errors.New("dial unix: connect: connection refused")
				}
				return dial(ctx, st, id, ver)
			}

			s.Branch = "polecat/furiosa/gt-b"
			_, err := b.Provision(context.Background(), s)
			require.NoError(t, err)
			assert.Equal(t, 3, dials)
			assert.Equal(t, 1, w.sessionOpenAttempts())
			w.mu.Lock()
			defer w.mu.Unlock()
			assert.Equal(t, "polecat/furiosa/gt-b", w.openBranch)
		})
	}
}

// A stale session that cannot be torn down fails the provision: opening over
// it would be refused, and reattaching to it is the bug.
func TestProvision_FailedReplacementFailsTheProvision(t *testing.T) {
	w := newFakeWorker(t)
	w.preload(sockproto.SessionSummary{Session: "gt-MyRig-furiosa", Rig: "MyRig", Polecat: "furiosa", State: "ready", Branch: "polecat/furiosa/gt-a"})
	w.hang = true
	b, _ := testBackend(t, w)
	old := controlTimeout
	controlTimeout = 200 * time.Millisecond
	t.Cleanup(func() { controlTimeout = old })

	s := spec()
	s.Branch = "polecat/furiosa/gt-b"
	_, err := b.Provision(context.Background(), s)
	require.ErrorContains(t, err, "replacing the session")
	assert.NotContains(t, w.got(), "session_open")
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

// A worker on a binary that predates agent files would drop them silently and
// start the agent without its settings. The session must not be opened.
func TestProvision_WorkerWithoutAgentFilesIsRefused(t *testing.T) {
	w := newFakeWorker(t)
	w.noAgentFiles = true
	b, _ := testBackend(t, w)

	withFiles := spec()
	withFiles.Files = map[string]string{"/host/polecats/.claude/settings.json": "{}"}
	_, err := b.Provision(context.Background(), withFiles)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "settings file")
	assert.NotContains(t, w.got(), sockproto.TypeSessionOpen, "no session may be opened on that worker")

	// Without files there is nothing to lose, so the same worker is still usable.
	_, err = b.Provision(context.Background(), spec())
	require.NoError(t, err)
}

// An idle worker on an older binary is refreshed by this very provision: the
// new gt-worker-client is staged and applied when the connection closes. The
// handshake this attempt holds still describes the old binary, so the gate
// would refuse a session the upgraded worker accepts. It must retry instead.
func TestProvision_RetriesWhenItsOwnPushStagedTheUpgrade(t *testing.T) {
	prev := provisionRetryDelay
	provisionRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { provisionRetryDelay = prev })

	w := newFakeWorker(t)
	w.noAgentFiles = true
	w.stageWorkerClient = true
	w.gtVersion = "1.2.2"
	b, _ := testBackend(t, w)
	b.GTVersion = "1.2.3"

	root := t.TempDir()
	native := filepath.Join(root, PlatformDir(runtime.GOOS, runtime.GOARCH))
	require.NoError(t, os.MkdirAll(native, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(native, "gt-worker-client"), []byte("payload"), 0755))
	defer SetBinarySourceForTest(root)()

	withFiles := spec()
	withFiles.Files = map[string]string{"/host/polecats/.claude/settings.json": "{}"}
	_, err := b.Provision(context.Background(), withFiles)
	require.NoError(t, err, "the upgrade this provision staged must be retried onto, not reported")
	assert.Equal(t, 1, w.sessionOpenAttempts(), "the session is opened once, on the upgraded worker")
}

// stagedUpgradeBackend is a backend whose worker runs an older binary without
// agent_files, stages a pushed gt-worker-client, and has that binary on hand.
func stagedUpgradeBackend(t *testing.T) (*SocketBackend, *fakeWorker, execution.PolecatSpec) {
	t.Helper()
	prev := provisionRetryDelay
	provisionRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { provisionRetryDelay = prev })

	w := newFakeWorker(t)
	w.noAgentFiles = true
	w.stageWorkerClient = true
	w.gtVersion = "1.2.2"
	b, _ := testBackend(t, w)
	b.GTVersion = "1.2.3"

	root := t.TempDir()
	native := filepath.Join(root, PlatformDir(runtime.GOOS, runtime.GOARCH))
	require.NoError(t, os.MkdirAll(native, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(native, "gt-worker-client"), []byte("payload"), 0755))
	t.Cleanup(SetBinarySourceForTest(root))

	withFiles := spec()
	withFiles.Files = map[string]string{"/host/polecats/.claude/settings.json": "{}"}
	return b, w, withFiles
}

// A worker with live sessions stages the upgrade but does not apply it, so a
// retry would only push the same binaries again at a worker that is serving
// agents, and fail the same way. It must be refused once, with the remedy.
func TestProvision_DoesNotRetryAStagedUpgradeOnABusyWorker(t *testing.T) {
	b, w, withFiles := stagedUpgradeBackend(t)
	w.maxSessions = 2
	w.preload(sockproto.SessionSummary{Session: "gt-MyRig-nux", Rig: "MyRig", Polecat: "nux", State: "ready"})

	dials := 0
	b.dialFn = func(ctx context.Context, s *Settings, id, ver string) (*conn, error) {
		dials++
		return dial(ctx, s, id, ver)
	}

	_, err := b.Provision(context.Background(), withFiles)
	require.Error(t, err)
	assert.NotErrorIs(t, err, errWorkerRestarting)
	assert.Contains(t, err.Error(), "upgrade the worker, or wait for it to go idle")
	assert.Equal(t, 1, dials, "a busy worker must not be re-dialed and re-pushed")
	assert.Equal(t, 0, w.sessionOpenAttempts())
}

// Closing the connection that staged the upgrade makes an idle worker exit so
// its supervisor can start the new binary. Until it is listening again a dial
// fails outright; that is the restart this provision caused, not a dead worker.
func TestProvision_RetriesADialFailureAfterStagingTheUpgrade(t *testing.T) {
	b, w, withFiles := stagedUpgradeBackend(t)

	dials := 0
	b.dialFn = func(ctx context.Context, s *Settings, id, ver string) (*conn, error) {
		dials++
		if dials == 2 || dials == 3 {
			return nil, errors.New("dial unix: connect: connection refused")
		}
		return dial(ctx, s, id, ver)
	}

	_, err := b.Provision(context.Background(), withFiles)
	require.NoError(t, err, "the worker being down right after the staged push is its restart")
	assert.Equal(t, 4, dials)
	assert.Equal(t, 1, w.sessionOpenAttempts())
}

// Without a staged upgrade behind it, a dial failure is a worker that is not
// there: it is reported at once, as before.
func TestProvision_DoesNotRetryAnUnrelatedDialFailure(t *testing.T) {
	b, _, withFiles := stagedUpgradeBackend(t)

	dials := 0
	b.dialFn = func(context.Context, *Settings, string, string) (*conn, error) {
		dials++
		return nil, errors.New("dial unix: connect: connection refused")
	}

	_, err := b.Provision(context.Background(), withFiles)
	require.Error(t, err)
	assert.Equal(t, 1, dials)
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
		"gt", "worker-attach", "--address", w.addr, "--session", "gt-MyRig-furiosa",
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
	// A worker that handshakes then never replies must not hang a
	// DEADLINE-LESS caller — this is what the controlTimeout self-defense
	// exists for. Shrink the cap so the test is fast, and pass a bare
	// context.Background() so the bound can ONLY come from the self-imposed
	// timeout (a caller deadline would mask it). Assert the error wraps the
	// self-timeout, not something else.
	orig := controlTimeout
	controlTimeout = 300 * time.Millisecond
	t.Cleanup(func() { controlTimeout = orig })

	w := newHangingWorker(t)
	b, _ := testBackend(t, w)

	start := time.Now()
	_, err := b.Discover(context.Background(), execution.IdentityTags{})
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrDeadlineExceeded, "the bound must come from the self-imposed controlTimeout's socket deadline")
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 300*time.Millisecond, "must wait for the self-timeout, not fail earlier")
	assert.Less(t, elapsed, 5*time.Second, "and not hang")
}

func TestTeardown_BoundsHungWorker(t *testing.T) {
	orig := controlTimeout
	controlTimeout = 300 * time.Millisecond
	t.Cleanup(func() { controlTimeout = orig })

	w := newHangingWorker(t)
	b, _ := testBackend(t, w)

	err := b.Teardown(context.Background(), execution.Endpoint{Backend: BackendName, ID: "s", Address: w.addr})
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrDeadlineExceeded)
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
