// Package workerclient implements the worker side of the socket execution
// provider (docs/design/remote-polecat-execution-socket.md): the persistent
// gt-worker-client service that accepts orchestrator control connections,
// opens polecat sessions (CSR over the connection, local relay, worktree,
// work container), runs each session's checkpoint loop + watchdog, and ends
// sessions on shutdown/teardown. It is the socket packaging of the same
// internal/worker building blocks gt-worker-agent uses.
//
// This increment covers the session service plus exec streaming (§4.3): a
// launcher attaches to a ready session, and the agent's stdio is piped over
// binary frames (see exec.go). The offline spool and container re-adoption
// after a service restart are later phases; sessions found in persisted state
// at startup are reported as "orphaned" for the daemon to reap and
// re-provision.
package workerclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/sockproto"
	"github.com/steveyegge/gastown/internal/worker"
)

// Config configures the worker service. TLS termination for TCP listeners is
// the caller's job (pass an already-TLS net.Listener to Serve); the service
// itself only implements the §3.3 unix pre-shared-token check.
type Config struct {
	// WorkerID is this machine's enrolled name, reported in hello_ack.
	WorkerID string
	// Token is the §3.3 pre-shared token. When non-empty, every connection
	// must open with an auth message carrying it (unix-socket mode).
	Token string
	// StateDir holds per-session state: worktrees/<rig>/<polecat>, identity
	// material, and sessions.json.
	StateDir string
	// ProxyURL is the host proxy base URL each session's relay dials.
	ProxyURL string
	// GTDir is the injected-gastown-bits dir bind-mounted into containers
	// (container mode only).
	GTDir string
	// MaxSessions caps concurrent sessions (§12 decision 1; default 1).
	MaxSessions int
	// ExecModes lists the modes this worker supports (default ["native"];
	// add "container" where docker exists).
	ExecModes []string
	// Docker reports a usable docker daemon (capabilities.docker).
	Docker bool
	// AgentEnvFile is an operator-managed KEY=VALUE file supplying worker-local
	// agent credentials (§8) — this provider's form of the externalized
	// agent-auth contract (core §7.1). Injected into the agent process
	// worker-side; never transmitted over the socket.
	AgentEnvFile string

	Log *slog.Logger
}

// session is one live polecat session. All mutable fields below are guarded
// by Service.mu; bringUp runs on its own goroutine while the connection's
// read loop keeps serving, so writes here and reads in teardown/shutdown must
// hold the lock.
type session struct {
	summary sockproto.SessionSummary // State mutated under s.mu

	cancel      context.CancelFunc // stops the supervisor (graceful §9.3)
	buildCancel context.CancelFunc // aborts an in-flight bringUp (dropped conn / early teardown)
	relay       *worker.Relay
	workEnv     *worker.WorkEnv // nil in native mode
	worktree    string
	execCancel  context.CancelFunc // cancels an attached exec stream (nil when none)
	certWanted  bool               // SignCSR is awaiting a cert
	certTaken   bool               // a cert has been claimed for this session
	tearingDown bool               // a teardown is in progress; suppress a racing orphan insert

	done      chan struct{} // closed when the supervisor has finished
	buildDone chan struct{} // closed when bringUp returns (success or fail)

	// certCh feeds the signed cert back into the in-flight bringup when the
	// orchestrator answers our csr message.
	certCh chan *sockproto.Message
}

// Service accepts control connections and manages sessions.
type Service struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session // by session id
	orphans  map[string]sockproto.SessionSummary

	// persistMu serializes sessions.json writes so concurrent persists can't
	// rename out of order and strand a stale snapshot.
	persistMu sync.Mutex

	// orphanHook, when set, is called at the very top of orphanSession before
	// it takes any lock — a test seam for driving the watchdog-vs-teardown
	// race deterministically. nil in production.
	orphanHook func()
}

// New builds a Service and loads persisted session state: sessions recorded
// by a previous process are reported as orphaned (§7 — the daemon reaps and
// re-provisions; live re-adoption is a later phase).
func New(cfg Config) (*Service, error) {
	if cfg.StateDir == "" {
		return nil, fmt.Errorf("workerclient: state dir is required")
	}
	if cfg.ProxyURL == "" {
		return nil, fmt.Errorf("workerclient: proxy url is required")
	}
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		return nil, fmt.Errorf("workerclient: create state dir: %w", err)
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 1
	}
	if len(cfg.ExecModes) == 0 {
		cfg.ExecModes = []string{"native"}
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &Service{
		cfg:      cfg,
		log:      cfg.Log,
		sessions: map[string]*session{},
		orphans:  map[string]sockproto.SessionSummary{},
	}
	s.loadPersisted()
	return s, nil
}

// Serve accepts control connections until ctx is canceled. The listener is
// caller-supplied so TCP callers wrap it in mTLS (tls.NewListener with
// RequireAndVerifyClientCert) and unix callers pass the raw listener plus a
// Token.
func (s *Service) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.shutdownAll()
				return nil
			}
			return err
		}
		go s.handle(ctx, nc)
	}
}

// connState is per-connection: the codec plus a write mutex, because session
// bringup completes on a goroutine while the read loop keeps serving.
type connState struct {
	codec  *sockproto.Codec
	nc     net.Conn
	sendMu sync.Mutex
	// writeFailed is sticky: once a frame write has failed (a launcher that
	// stopped draining, a dead peer), every later write short-circuits instead
	// of burning another frameWriteTimeout. Guarded by sendMu.
	writeFailed bool
}

func (c *connState) send(m *sockproto.Message) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.codec.Send(m)
}

// frameWriteTimeout bounds a single frame write. A launcher that stops draining
// (killed pane, partition with no FIN/RST, full recv buffer) fills the socket
// send buffer; without a deadline the pump would block inside WriteFrame while
// HOLDING sendMu, the agent would block on its own stdout pipe, cmd.Wait would
// never return, and the session — the whole worker at MaxSessions=1 — would
// wedge permanently. A unix socket has no keepalive backstop, so this deadline
// is the only bound.
const frameWriteTimeout = 60 * time.Second

// writeFrame writes one §4.3 frame under the same lock as message sends, so a
// frame's header and payload can never interleave with another writer's.
func (c *connState) writeFrame(t sockproto.FrameType, payload []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.writeLocked(func() error { return sockproto.WriteFrame(c.nc, t, payload) })
}

// writeExit writes the terminal exit frame.
func (c *connState) writeExit(code int) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.writeLocked(func() error { return sockproto.WriteExitFrame(c.nc, code) })
}

// writeLocked runs one deadline-bounded write. sendMu must be held.
func (c *connState) writeLocked(write func() error) error {
	if c.writeFailed {
		return errWriteFailed
	}
	_ = c.nc.SetWriteDeadline(time.Now().Add(frameWriteTimeout))
	defer c.nc.SetWriteDeadline(time.Time{})
	err := write()
	if err != nil {
		c.writeFailed = true
	}
	return err
}

// errWriteFailed reports a write attempted after this connection's outbound
// half was already known dead.
var errWriteFailed = errors.New("exec stream: outbound half is dead")

// expireReads pushes the connection's read deadline into the past, unblocking
// a goroutine parked in a socket read. Writes are unaffected.
func (c *connState) expireReads() error {
	return c.nc.SetReadDeadline(time.Now().Add(-time.Second))
}

// frameReader returns the reader an exec stream must read frames from: the
// codec's BUFFERED reader, since bytes past the attach preamble line may
// already be buffered and would be lost by reading the raw conn.
func (c *connState) frameReader() io.Reader { return c.codec.Reader() }

func (s *Service) handle(ctx context.Context, nc net.Conn) {
	defer nc.Close()
	// A per-connection context: a dropped connection cancels its in-flight
	// bringups immediately (freeing the session slot) instead of leaving them
	// blocked for the full bringup timeout waiting for a cert that can never
	// arrive on the dead connection.
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()
	c := &connState{codec: sockproto.NewCodec(nc), nc: nc}

	authed := s.cfg.Token == ""
	helloed := false

	for {
		m, err := c.codec.Recv()
		if err != nil {
			return
		}
		// §3.3: in token mode, auth must be the first message.
		if !authed {
			if m.Type != sockproto.TypeAuth || m.Token != s.cfg.Token {
				_ = c.send(&sockproto.Message{Type: sockproto.TypeError, Code: "auth", Msg: "authentication required"})
				return
			}
			authed = true
			continue
		}
		if !helloed && m.Type != sockproto.TypeHello {
			_ = c.send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Code: "proto", Msg: "hello required first"})
			return
		}

		switch m.Type {
		case sockproto.TypeHello:
			if m.ProtoVersion != sockproto.ProtoVersion {
				_ = c.send(&sockproto.Message{Type: sockproto.TypeError, Code: "proto_version",
					Msg: fmt.Sprintf("worker speaks proto %d", sockproto.ProtoVersion)})
				return
			}
			helloed = true
			_ = c.send(&sockproto.Message{
				Type:         sockproto.TypeHelloAck,
				ProtoVersion: sockproto.ProtoVersion,
				WorkerID:     s.cfg.WorkerID,
				OS:           runtime.GOOS,
				Arch:         runtime.GOARCH,
				Capabilities: &sockproto.Capabilities{
					Docker:      s.cfg.Docker,
					ExecModes:   s.cfg.ExecModes,
					MaxSessions: s.cfg.MaxSessions,
				},
				Sessions: s.summaries(),
			})
		case sockproto.TypePing:
			_ = c.send(&sockproto.Message{Type: sockproto.TypePong, ID: m.ID})
		case sockproto.TypeDiscover:
			_ = c.send(&sockproto.Message{Type: sockproto.TypeSessions, ID: m.ID, Sessions: s.filtered(m.Rig, m.Polecat)})
		case sockproto.TypeSessionOpen:
			s.handleSessionOpen(connCtx, c, m)
		case sockproto.TypeCert:
			s.handleCert(c, m)
		case sockproto.TypeShutdown:
			s.handleShutdown(c, m)
		case sockproto.TypeAttach:
			// An exec stream takes over this connection (§4.3): after the ack
			// it carries only binary frames, so the control loop must not read
			// from it again.
			s.handleAttach(connCtx, c, m)
			return
		case sockproto.TypeTeardown:
			s.handleTeardown(c, m)
		default:
			_ = c.send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Code: "unknown", Msg: "unknown message type " + m.Type})
		}
	}
}

// handleSessionOpen validates and starts session bringup. The reply to
// session_open is our csr (echoing its id); the cert message that follows
// feeds certCh, and bringup completes async, answering the CERT's id with
// session_ready or session_error.
func (s *Service) handleSessionOpen(ctx context.Context, c *connState, m *sockproto.Message) {
	// Validate the orchestrator-supplied identity fields BEFORE they reach any
	// filesystem or URL sink (the worker is a separate trust domain — HIGH).
	if err := validateSessionFields(m.Session, m.Rig, m.Polecat); err != nil {
		_ = c.send(&sockproto.Message{Type: sockproto.TypeSessionError, ID: m.ID, Session: m.Session, Code: "bad_request", Msg: err.Error()})
		return
	}
	modeOK := false
	for _, em := range s.cfg.ExecModes {
		if em == m.ExecMode {
			modeOK = true
		}
	}
	if !modeOK {
		_ = c.send(&sockproto.Message{Type: sockproto.TypeSessionError, ID: m.ID, Session: m.Session, Code: "exec_mode",
			Msg: fmt.Sprintf("exec_mode %q not supported (worker supports %v)", m.ExecMode, s.cfg.ExecModes)})
		return
	}

	s.mu.Lock()
	if _, live := s.sessions[m.Session]; live {
		s.mu.Unlock()
		_ = c.send(&sockproto.Message{Type: sockproto.TypeSessionError, ID: m.ID, Session: m.Session, Code: "exists", Msg: "session already live"})
		return
	}
	if len(s.sessions) >= s.cfg.MaxSessions {
		s.mu.Unlock()
		_ = c.send(&sockproto.Message{Type: sockproto.TypeSessionError, ID: m.ID, Session: m.Session, Code: "max_sessions",
			Msg: fmt.Sprintf("worker is at its session limit (%d)", s.cfg.MaxSessions)})
		return
	}
	sess := &session{
		summary: sockproto.SessionSummary{
			Session: m.Session, Rig: m.Rig, Polecat: m.Polecat,
			State: "starting", StartedAt: time.Now().UTC(),
		},
		certCh:    make(chan *sockproto.Message, 1),
		done:      make(chan struct{}),
		buildDone: make(chan struct{}),
	}
	s.sessions[m.Session] = sess
	delete(s.orphans, m.Session) // a re-opened identity replaces its orphan record
	s.mu.Unlock()

	// nolint is reported here, on the goroutine: bringUp's teardown path uses a
	// context.Background deadline on purpose (see the comment at that call) —
	// deriving it from ctx would make container teardown a no-op exactly when it
	// is needed, because the session ending is what cancels ctx.
	go s.bringUp(ctx, c, m, sess) //nolint:gosec // G118: deliberate detached teardown context
}

// handleCert routes the signed cert to the in-flight bringup. The claim
// (certTaken) is made UNDER the lock before delivery, so exactly one cert is
// ever accepted for a session — a duplicate always gets unexpected_cert, with
// no receive→set-flag window for a second cert to slip through.
func (s *Service) handleCert(c *connState, m *sockproto.Message) {
	s.mu.Lock()
	sess := s.sessions[m.Session]
	ok := sess != nil && sess.certWanted && !sess.certTaken
	if ok {
		sess.certTaken = true // claim it now, under the lock
	}
	s.mu.Unlock()
	if !ok {
		_ = c.send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Session: m.Session, Code: "unexpected_cert", Msg: "session is not awaiting a cert"})
		return
	}
	// certWanted means SignCSR is at (or about to reach) the receive; certCh
	// has cap 1, so this send never blocks the read loop.
	sess.certCh <- m
}

// connSigner adapts the §4.2 CSR-over-the-connection exchange to the
// worker.Signer interface: SignCSR sends our csr (echoing the session_open
// id) and blocks until handleCert feeds the orchestrator's answer.
type connSigner struct {
	svc    *Service
	c      *connState
	sess   *session
	openID string
	// certID receives the cert message's id so the final session_ready can
	// echo it (the §4 request/response pairing).
	certID string
}

func (cs *connSigner) SignCSR(ctx context.Context, csrPEM []byte) (certPEM, caPEM []byte, err error) {
	// Mark the session as awaiting a cert BEFORE emitting the CSR, so any cert
	// that arrives is delivered here (handleCert gates delivery on certWanted).
	cs.svc.mu.Lock()
	cs.sess.certWanted = true
	cs.svc.mu.Unlock()

	if err := cs.c.send(&sockproto.Message{
		Type:    sockproto.TypeCSR,
		ID:      cs.openID,
		Session: cs.sess.summary.Session,
		CSRPEM:  string(csrPEM),
	}); err != nil {
		return nil, nil, err
	}
	select {
	case m := <-cs.sess.certCh:
		cs.certID = m.ID
		if m.CertPEM == "" || m.CAPEM == "" {
			return nil, nil, fmt.Errorf("workerclient: cert message missing cert or ca")
		}
		return []byte(m.CertPEM), []byte(m.CAPEM), nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// bringUp is the §4.2 session bringup, run off the read loop: identity (CSR
// over the conn) → relay → worktree clone through the relay → work container
// (container mode) → checkpoint supervisor → session_ready.
func (s *Service) bringUp(ctx context.Context, c *connState, open *sockproto.Message, sess *session) {
	defer close(sess.buildDone)

	// Bringup is bounded (a stalled orchestrator or slow clone must not pin a
	// half-built session) AND cancelable by teardown/dropped-conn: ctx is the
	// per-connection context, and buildCancel lets an external teardown abort
	// this bringup immediately.
	buCtx, buCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer buCancel()
	s.mu.Lock()
	sess.buildCancel = buCancel
	s.mu.Unlock()

	idDir := filepath.Join(s.cfg.StateDir, "identity", sess.summary.Session)
	var relayCancel context.CancelFunc

	// fail cleans up whatever this bringup started (it holds the only refs)
	// and drops the session — WITHOUT going through teardownSession, which
	// waits on buildDone and would deadlock here. Idempotent with a racing
	// external teardown, which returns early once it sees the session gone.
	fail := func(id, code string, err error) {
		s.log.Warn("session bringup failed", "session", sess.summary.Session, "code", code, "err", err)
		if relayCancel != nil {
			relayCancel()
		}
		s.mu.Lock()
		worktree := sess.worktree
		we := sess.workEnv
		delete(s.sessions, sess.summary.Session)
		s.mu.Unlock()
		if we != nil {
			// Deliberately detached from ctx: this teardown runs BECAUSE the
			// session ended, which usually means ctx is already canceled. A
			// ctx-derived deadline here would make Teardown a no-op and leak
			// the work container.
			tctx, tcancel := context.WithTimeout(context.Background(), time.Minute)
			_ = we.Teardown(tctx)
			tcancel()
		}
		if worktree != "" {
			_ = os.RemoveAll(worktree)
		}
		_ = os.RemoveAll(idDir)
		s.persist()
		_ = c.send(&sockproto.Message{Type: sockproto.TypeSessionError, ID: id, Session: sess.summary.Session, Code: code, Msg: err.Error()})
	}

	// One definition of the CN, shared with the orchestrator that signs it: the
	// two must agree byte for byte or enrollment breaks (or worse, issues a
	// cert the authz layer reads as a different polecat).
	cn := worker.PolecatCN(sess.summary.Rig, sess.summary.Polecat)
	signer := &connSigner{svc: s, c: c, sess: sess, openID: open.ID}
	id, err := worker.EnsureIdentity(buCtx, idDir, cn, signer)
	if err != nil {
		fail(open.ID, "identity", err)
		return
	}

	// The final session_ready/session_error must echo the id of the LAST
	// request the orchestrator is awaiting a reply to: the cert message when a
	// CSR exchange happened, or the session_open itself when EnsureIdentity
	// reused an existing cert (no CSR). Getting this wrong hangs the backend,
	// which drops any reply whose id doesn't match its in-flight request.
	replyID := signer.certID
	if replyID == "" {
		replyID = open.ID
	}

	relay, err := worker.NewRelay(s.cfg.ProxyURL, id)
	if err != nil {
		fail(replyID, "relay", err)
		return
	}
	s.mu.Lock()
	sess.relay = relay
	s.mu.Unlock()
	relayCtx, rc := context.WithCancel(context.Background())
	relayCancel = rc
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		if err := relay.Serve(relayCtx, "127.0.0.1:0"); err != nil {
			s.log.Warn("session relay", "session", sess.summary.Session, "err", err)
		}
	}()
	var relayAddr net.Addr
	for i := 0; i < 100 && relayAddr == nil; i++ {
		relayAddr = relay.Addr()
		if relayAddr == nil {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if relayAddr == nil {
		fail(replyID, "relay", fmt.Errorf("relay did not bind"))
		return
	}
	relayURL := "http://" + relayAddr.String()

	// Worktree: cloned THROUGH the relay from the host .repo.git, so origin
	// already points at the control plane for checkpoint pushes (§5, §7). The
	// identity fields were validated at session_open; underRoot is defense in
	// depth so the join can never escape the state dir.
	worktree := filepath.Join(s.cfg.StateDir, "worktrees", sess.summary.Rig, sess.summary.Polecat)
	if err := underRoot(s.cfg.StateDir, worktree); err != nil {
		fail(replyID, "worktree", err)
		return
	}
	s.mu.Lock()
	sess.worktree = worktree
	s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(worktree), 0755); err != nil {
		fail(replyID, "worktree", err)
		return
	}
	_ = os.RemoveAll(worktree) // replace any stale tree (re-adoption is a later phase)
	cloneURL := relayURL + "/v1/git/" + url.PathEscape(sess.summary.Rig)
	if out, err := exec.CommandContext(buCtx, "git", "clone", "--", cloneURL, worktree).CombinedOutput(); err != nil {
		fail(replyID, "clone", fmt.Errorf("git clone via relay: %v: %s", err, strings.TrimSpace(string(out))))
		return
	}

	// Container mode: prepare the idle work container (§6.1.2 via the shared
	// WorkEnv). Native mode has nothing to prepare here.
	if open.ExecMode == "container" {
		_, portStr, _ := net.SplitHostPort(relayAddr.String())
		port := 0
		fmt.Sscanf(portStr, "%d", &port)
		we, err := worker.NewWorkEnv(worker.WorkEnvConfig{
			Rig:       sess.summary.Rig,
			Polecat:   sess.summary.Polecat,
			Session:   sess.summary.Session,
			Image:     open.Image,
			Worktree:  worktree,
			GTDir:     s.cfg.GTDir,
			Sandboxed: open.NetworkMode == "sandboxed",
			RelayPort: port,
		})
		if err == nil {
			s.mu.Lock()
			sess.workEnv = we
			s.mu.Unlock()
			err = we.Prepare(buCtx)
		}
		if err != nil {
			fail(replyID, "workenv", err)
			return
		}
	}

	// Checkpoint supervisor (§9.2, §9.5). The relay must outlive the
	// supervisor (its final flush pushes through it) — relayCancel runs after
	// sup.Run returns, mirroring gt-worker-agent's ordering.
	interval := 5 * time.Minute
	if open.CheckpointInterval != "" {
		if d, err := time.ParseDuration(open.CheckpointInterval); err == nil && d > 0 {
			interval = d
		}
	}
	var maxRuntime time.Duration
	if open.MaxRuntime != "" {
		if d, err := time.ParseDuration(open.MaxRuntime); err == nil && d > 0 {
			maxRuntime = d
		}
	}
	supCfg := worker.SupervisorConfig{
		Checkpointer: &worker.Checkpointer{
			Worktree: worktree,
			Ref:      worker.CheckpointRefForPolecat(sess.summary.Polecat),
			Debounce: 2 * time.Second,
		},
		Interval:   interval,
		MaxRuntime: maxRuntime,
		Log:        s.log,
	}
	if sess.workEnv != nil {
		supCfg.StopWork = sess.workEnv.StopWork
	}
	sup := worker.NewSupervisor(supCfg)
	// supCancel is not deferred on purpose: the supervisor outlives this call.
	// It is stored in sess.cancel under the lock below and invoked by teardown /
	// shutdown, which is the only thing that may stop a running session.
	supCtx, supCancel := context.WithCancel(context.Background()) //nolint:gosec // G118: cancel is stored in sess.cancel and called by teardown
	s.mu.Lock()
	sess.cancel = supCancel
	s.mu.Unlock()
	go func() {
		defer close(sess.done)
		reason := sup.Run(supCtx)
		relayCancel()
		<-relayDone
		s.log.Info("session supervisor stopped", "session", sess.summary.Session, "reason", reason)
		// A watchdog stop (max-runtime / dead-man) orphans the session (§7):
		// the machine keeps running, the daemon reaps on next contact. A
		// StopInterrupted came from teardown/shutdown, which do their own
		// cleanup — don't re-insert an orphan for a session being torn down.
		if reason != worker.StopInterrupted {
			s.orphanSession(sess)
		}
	}()

	s.mu.Lock()
	sess.summary.State = "ready"
	s.mu.Unlock()
	s.persist()

	_ = c.send(&sockproto.Message{
		Type:      sockproto.TypeSessionReady,
		ID:        replyID,
		Session:   sess.summary.Session,
		RelayAddr: relayAddr.String(),
	})
}

// handleShutdown runs the graceful §9.3 sequence for a session: cancel the
// supervisor (StopWork → final flush) and confirm.
func (s *Service) handleShutdown(c *connState, m *sockproto.Message) {
	s.mu.Lock()
	sess := s.sessions[m.Session]
	s.mu.Unlock()
	if sess == nil {
		_ = c.send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Session: m.Session, Code: "no_session", Msg: "no live session"})
		return
	}
	// Abort an in-flight bringup rather than wait for it: handleShutdown runs
	// synchronously on the connection read loop, and a still-enrolling bringup
	// is parked in SignCSR waiting for a cert that only THIS read loop can
	// deliver — waiting here would deadlock the whole connection until the
	// bringup timeout. A session that never reached ready has no worktree to
	// flush anyway. (Matches teardownSession's cancel-then-wait.)
	s.mu.Lock()
	if sess.buildCancel != nil {
		sess.buildCancel()
	}
	s.mu.Unlock()
	<-sess.buildDone
	s.mu.Lock()
	cancel := sess.cancel
	_, live := s.sessions[m.Session]
	s.mu.Unlock()
	if !live || cancel == nil {
		_ = c.send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Session: m.Session, Code: "no_session", Msg: "no live session"})
		return
	}
	// An attached agent must stop BEFORE the supervisor's final flush, or the
	// checkpoint captures a tree the agent is still writing to.
	s.mu.Lock()
	execCancel := sess.execCancel
	s.mu.Unlock()
	if execCancel != nil {
		execCancel()
	}
	cancel()
	<-sess.done
	_ = c.send(&sockproto.Message{
		Type:          sockproto.TypeShutdownComplete,
		ID:            m.ID,
		Session:       m.Session,
		CheckpointRef: worker.CheckpointRefForPolecat(sess.summary.Polecat),
	})
}

// handleTeardown ends the session and leaves the machine as if it never ran
// (§6): stop everything, remove the worktree (by default), shred identity,
// drop state.
func (s *Service) handleTeardown(c *connState, m *sockproto.Message) {
	clean := m.CleanWorktree == nil || *m.CleanWorktree

	s.mu.Lock()
	sess := s.sessions[m.Session]
	orphan, wasOrphan := s.orphans[m.Session]
	delete(s.orphans, m.Session)
	s.mu.Unlock()

	if sess == nil {
		if wasOrphan {
			// Tearing down an already-orphaned session: its relay is stopped
			// and identity shredded, but an orphan KEEPS its worktree and (in
			// container mode) its stopped work container — the watchdog dropped
			// the WorkEnv handle. Remove both by their deterministic
			// name/path so the machine is left clean.
			if s.cfg.Docker {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				if err := worker.RemoveWorkContainer(ctx, "", orphan.Rig, orphan.Polecat); err != nil {
					s.log.Warn("remove orphaned work container", "session", m.Session, "err", err)
				}
				cancel()
			}
			if clean {
				wt := filepath.Join(s.cfg.StateDir, "worktrees", orphan.Rig, orphan.Polecat)
				if err := underRoot(s.cfg.StateDir, wt); err == nil {
					_ = os.RemoveAll(wt)
				} else {
					s.log.Warn("refusing to remove worktree outside state dir", "session", m.Session, "err", err)
				}
			}
			_ = os.RemoveAll(filepath.Join(s.cfg.StateDir, "identity", m.Session))
			s.persist()
			_ = c.send(&sockproto.Message{Type: sockproto.TypeTeardownComplete, ID: m.ID, Session: m.Session})
			return
		}
		_ = c.send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Session: m.Session, Code: "no_session", Msg: "no such session"})
		return
	}
	s.teardownSession(sess, clean)
	_ = c.send(&sockproto.Message{Type: sockproto.TypeTeardownComplete, ID: m.ID, Session: m.Session})
}

// teardownSession is the §6 cleanup: supervisor down, container removed,
// relay stopped, worktree (optionally) removed, identity shredded, state
// dropped. It first ABORTS any in-flight bringup and waits for it to finish,
// so it never races bringUp's field writes or cleans up mid-build.
func (s *Service) teardownSession(sess *session, cleanWorktree bool) {
	// tearingDown suppresses a racing watchdog orphanSession insert; buildCancel
	// aborts an in-flight bringup so we never race its field writes.
	s.mu.Lock()
	sess.tearingDown = true
	if sess.buildCancel != nil {
		sess.buildCancel()
	}
	// Kill any attached exec first: otherwise a native agent keeps running
	// while we RemoveAll its worktree out from under it.
	if sess.execCancel != nil {
		sess.execCancel()
	}
	s.mu.Unlock()
	<-sess.buildDone

	// bringUp has returned; read its now-stable fields under the lock. If it
	// failed OR the watchdog orphaned it concurrently, the session is gone from
	// s.sessions — remove any orphan record it left so no phantom remains.
	s.mu.Lock()
	_, live := s.sessions[sess.summary.Session]
	cancel := sess.cancel
	workEnv := sess.workEnv
	relay := sess.relay
	worktree := sess.worktree
	s.mu.Unlock()
	if !live {
		// bringUp failed (it cleaned up its own partial state) OR the watchdog
		// orphaned the session while we waited on buildDone. Either way finish
		// the cleanup: remove any orphan record and its resources so nothing
		// leaks and no phantom survives on disk.
		s.mu.Lock()
		_, orphaned := s.orphans[sess.summary.Session]
		delete(s.orphans, sess.summary.Session)
		s.mu.Unlock()
		if orphaned {
			if workEnv != nil {
				ctx, c := context.WithTimeout(context.Background(), time.Minute)
				_ = workEnv.Teardown(ctx)
				c()
			}
			if cleanWorktree && worktree != "" {
				_ = os.RemoveAll(worktree)
			}
			_ = os.RemoveAll(filepath.Join(s.cfg.StateDir, "identity", sess.summary.Session))
			s.persist()
		}
		return
	}

	if cancel != nil {
		cancel()
		<-sess.done
	}
	if workEnv != nil {
		ctx, c := context.WithTimeout(context.Background(), time.Minute)
		if err := workEnv.Teardown(ctx); err != nil {
			s.log.Warn("work container teardown", "session", sess.summary.Session, "err", err)
		}
		c()
	}
	if relay != nil {
		_ = relay.Close()
	}
	if cleanWorktree && worktree != "" {
		_ = os.RemoveAll(worktree)
	}
	_ = os.RemoveAll(filepath.Join(s.cfg.StateDir, "identity", sess.summary.Session))

	s.mu.Lock()
	delete(s.sessions, sess.summary.Session)
	// Also drop any orphan the watchdog's orphanSession inserted while we were
	// blocked on <-sess.done above: the supervisor goroutine calls
	// orphanSession BEFORE closing done, so by now its insert (if any) has
	// happened and this delete deterministically removes the phantom.
	delete(s.orphans, sess.summary.Session)
	s.mu.Unlock()
	s.persist()
}

// orphanSession moves a self-stopped session (watchdog) into the orphan set.
// Called from the supervisor goroutine after bringUp has published its fields,
// so a plain locked read is safe. The relay is stopped and the session cert
// shredded: an orphan is dead (re-adoption is a later phase), so a
// re-provision of the same identity re-enrolls with a fresh cert.
func (s *Service) orphanSession(sess *session) {
	if s.orphanHook != nil {
		s.orphanHook()
	}
	// The tearingDown check and the session→orphan move happen in ONE critical
	// section so a concurrent teardown cannot observe live==true and then have
	// this insert land behind its back. If a teardown is in progress it OWNS
	// the full cleanup — leave the session in place (do NOT delete it) so
	// teardown's live-read still sees it and removes worktree/container/state;
	// deleting here would route teardown into its no-op !live path and leak
	// those resources.
	s.mu.Lock()
	if sess.tearingDown {
		s.mu.Unlock()
		return
	}
	relay := sess.relay
	delete(s.sessions, sess.summary.Session)
	sum := sess.summary
	sum.State = "orphaned"
	s.orphans[sum.Session] = sum
	s.mu.Unlock()

	// An orphan is dead (re-adoption is a later phase): stop its relay and
	// shred its session cert so a re-provision re-enrolls with a fresh one.
	if relay != nil {
		_ = relay.Close()
	}
	_ = os.RemoveAll(filepath.Join(s.cfg.StateDir, "identity", sess.summary.Session))
	s.persist()
}

// shutdownAll flushes every live session on service stop (§7: local SIGTERM
// triggers the same flush across all sessions).
func (s *Service) shutdownAll() {
	s.mu.Lock()
	var live []*session
	for _, sess := range s.sessions {
		live = append(live, sess)
	}
	s.mu.Unlock()
	for _, sess := range live {
		<-sess.buildDone // let an in-flight bringup finish before stopping it
		s.mu.Lock()
		cancel := sess.cancel
		s.mu.Unlock()
		if cancel != nil {
			cancel()
			<-sess.done
		}
	}
	s.persist()
}

// summaries returns all sessions (live + orphaned) for hello_ack.
func (s *Service) summaries() []sockproto.SessionSummary {
	return s.filtered("", "")
}

// filtered returns sessions matching the discover filters.
func (s *Service) filtered(rig, polecat string) []sockproto.SessionSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []sockproto.SessionSummary
	add := func(sum sockproto.SessionSummary) {
		if rig != "" && sum.Rig != rig {
			return
		}
		if polecat != "" && sum.Polecat != polecat {
			return
		}
		out = append(out, sum)
	}
	for _, sess := range s.sessions {
		add(sess.summary)
	}
	for _, sum := range s.orphans {
		add(sum)
	}
	return out
}

// ---- persisted state (sessions.json) ----

func (s *Service) stateFile() string {
	return filepath.Join(s.cfg.StateDir, "sessions.json")
}

// persist writes the current session set. Sessions are persisted so a
// service restart can report them (as orphans, until re-adoption lands).
//
// The whole snapshot-and-write is serialized under persistMu: two concurrent
// persists must not rename out of order (which could leave a stale snapshot
// on disk even after the in-memory maps are empty). Because the snapshot is
// taken WHEN persist runs (not when it was called), the last persist to run
// always reflects the latest map state.
func (s *Service) persist() {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	s.mu.Lock()
	all := []sockproto.SessionSummary{} // non-nil → empty marshals as [] not null
	for _, sess := range s.sessions {
		all = append(all, sess.summary)
	}
	for _, sum := range s.orphans {
		all = append(all, sum)
	}
	s.mu.Unlock()
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return
	}
	tmp := s.stateFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		s.log.Warn("persist sessions", "err", err)
		return
	}
	if err := os.Rename(tmp, s.stateFile()); err != nil {
		s.log.Warn("persist sessions", "err", err)
	}
}

// loadPersisted reads sessions.json; anything recorded by a previous process
// is orphaned (its supervisor/relay/container handles are gone).
func (s *Service) loadPersisted() {
	data, err := os.ReadFile(s.stateFile())
	if err != nil {
		return
	}
	var all []sockproto.SessionSummary
	if err := json.Unmarshal(data, &all); err != nil {
		s.log.Warn("corrupt sessions.json ignored", "err", err)
		return
	}
	for _, sum := range all {
		sum.State = "orphaned"
		s.orphans[sum.Session] = sum
	}
}
