// Package workerclient implements the worker side of the socket execution
// provider (docs/design/remote-polecat-execution-socket.md): the persistent
// gt-worker-client service that accepts orchestrator control connections,
// opens polecat sessions (CSR over the connection, local relay, worktree,
// work container), runs each session's checkpoint loop + watchdog, and ends
// sessions on shutdown/teardown. It is the socket packaging of the same
// internal/worker building blocks gt-worker-agent uses.
//
// This increment covers the session service (spec §11 phases 1-2 worker
// side). Enrollment (§3.1), exec streaming (§4.3), the offline spool, and
// container re-adoption after a service restart are later phases; sessions
// found in persisted state at startup are reported as "orphaned" for the
// daemon to reap and re-provision.
package workerclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
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

	Log *slog.Logger
}

// session is one live polecat session.
type session struct {
	summary sockproto.SessionSummary

	cancel   context.CancelFunc // stops the supervisor (graceful §9.3)
	done     chan struct{}      // closed when the supervisor has finished
	relay    *worker.Relay
	workEnv  *worker.WorkEnv // nil in native mode
	worktree string

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
		ln.Close()
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
	sendMu sync.Mutex
}

func (c *connState) send(m *sockproto.Message) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.codec.Send(m)
}

func (s *Service) handle(ctx context.Context, nc net.Conn) {
	defer nc.Close()
	c := &connState{codec: sockproto.NewCodec(nc)}

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
			s.handleSessionOpen(ctx, c, m)
		case sockproto.TypeCert:
			s.handleCert(c, m)
		case sockproto.TypeShutdown:
			s.handleShutdown(c, m)
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
	if m.Session == "" || m.Rig == "" || m.Polecat == "" {
		_ = c.send(&sockproto.Message{Type: sockproto.TypeSessionError, ID: m.ID, Session: m.Session, Code: "bad_request", Msg: "session, rig, and polecat are required"})
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
		certCh: make(chan *sockproto.Message, 1),
		done:   make(chan struct{}),
	}
	s.sessions[m.Session] = sess
	delete(s.orphans, m.Session) // a re-opened identity replaces its orphan record
	s.mu.Unlock()

	go s.bringUp(ctx, c, m, sess)
}

// handleCert routes the signed cert to the in-flight bringup.
func (s *Service) handleCert(c *connState, m *sockproto.Message) {
	s.mu.Lock()
	sess := s.sessions[m.Session]
	s.mu.Unlock()
	if sess == nil {
		_ = c.send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Session: m.Session, Code: "no_session", Msg: "no session awaiting a cert"})
		return
	}
	select {
	case sess.certCh <- m:
	default:
		_ = c.send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Session: m.Session, Code: "unexpected_cert", Msg: "session is not awaiting a cert"})
	}
}

// connSigner adapts the §4.2 CSR-over-the-connection exchange to the
// worker.Signer interface: SignCSR sends our csr (echoing the session_open
// id) and blocks until handleCert feeds the orchestrator's answer.
type connSigner struct {
	c      *connState
	sess   *session
	openID string
	// certID receives the cert message's id so the final session_ready can
	// echo it (the §4 request/response pairing).
	certID string
}

func (cs *connSigner) SignCSR(ctx context.Context, csrPEM []byte) (certPEM, caPEM []byte, err error) {
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
	fail := func(id, code string, err error) {
		s.log.Warn("session bringup failed", "session", sess.summary.Session, "code", code, "err", err)
		s.teardownSession(sess, true)
		_ = c.send(&sockproto.Message{Type: sockproto.TypeSessionError, ID: id, Session: sess.summary.Session, Code: code, Msg: err.Error()})
	}

	// Bringup is bounded: a stalled orchestrator (never sends cert) or a
	// slow clone must not pin a half-built session forever.
	buCtx, buCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer buCancel()

	cn := "gt-" + sess.summary.Rig + "-" + sess.summary.Polecat
	idDir := filepath.Join(s.cfg.StateDir, "identity", sess.summary.Session)
	signer := &connSigner{c: c, sess: sess, openID: open.ID}
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
	sess.relay = relay
	relayCtx, relayCancel := context.WithCancel(context.Background())
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
		relayCancel()
		fail(replyID, "relay", fmt.Errorf("relay did not bind"))
		return
	}
	relayURL := "http://" + relayAddr.String()

	// Worktree: cloned THROUGH the relay from the host .repo.git, so origin
	// already points at the control plane for checkpoint pushes (§5, §7).
	sess.worktree = filepath.Join(s.cfg.StateDir, "worktrees", sess.summary.Rig, sess.summary.Polecat)
	if err := os.MkdirAll(filepath.Dir(sess.worktree), 0755); err != nil {
		relayCancel()
		fail(replyID, "worktree", err)
		return
	}
	_ = os.RemoveAll(sess.worktree) // replace any stale tree (re-adoption is a later phase)
	cloneURL := relayURL + "/v1/git/" + sess.summary.Rig
	if out, err := exec.CommandContext(buCtx, "git", "clone", "--", cloneURL, sess.worktree).CombinedOutput(); err != nil {
		relayCancel()
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
			Worktree:  sess.worktree,
			GTDir:     s.cfg.GTDir,
			Sandboxed: open.NetworkMode == "sandboxed",
			RelayPort: port,
		})
		if err == nil {
			err = we.Prepare(buCtx)
		}
		if err != nil {
			relayCancel()
			fail(replyID, "workenv", err)
			return
		}
		sess.workEnv = we
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
			Worktree: sess.worktree,
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
	supCtx, supCancel := context.WithCancel(context.Background())
	sess.cancel = supCancel
	go func() {
		defer close(sess.done)
		reason := sup.Run(supCtx)
		relayCancel()
		<-relayDone
		s.log.Info("session supervisor stopped", "session", sess.summary.Session, "reason", reason)
		// A watchdog stop (max-runtime / dead-man) orphans the session (§7):
		// the machine keeps running, the daemon reaps on next contact.
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
	if sess == nil || sess.cancel == nil {
		_ = c.send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Session: m.Session, Code: "no_session", Msg: "no live session"})
		return
	}
	sess.cancel()
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
	s.mu.Lock()
	sess := s.sessions[m.Session]
	_, wasOrphan := s.orphans[m.Session]
	delete(s.orphans, m.Session)
	s.mu.Unlock()

	if sess == nil {
		if wasOrphan {
			s.persist()
			_ = c.send(&sockproto.Message{Type: sockproto.TypeTeardownComplete, ID: m.ID, Session: m.Session})
			return
		}
		_ = c.send(&sockproto.Message{Type: sockproto.TypeError, ID: m.ID, Session: m.Session, Code: "no_session", Msg: "no such session"})
		return
	}
	clean := m.CleanWorktree == nil || *m.CleanWorktree
	s.teardownSession(sess, clean)
	_ = c.send(&sockproto.Message{Type: sockproto.TypeTeardownComplete, ID: m.ID, Session: m.Session})
}

// teardownSession is the §6 cleanup: supervisor down, container removed,
// relay stopped, worktree (optionally) removed, identity shredded, state
// dropped.
func (s *Service) teardownSession(sess *session, cleanWorktree bool) {
	if sess.cancel != nil {
		sess.cancel()
		<-sess.done
	}
	if sess.workEnv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if err := sess.workEnv.Teardown(ctx); err != nil {
			s.log.Warn("work container teardown", "session", sess.summary.Session, "err", err)
		}
		cancel()
	}
	if sess.relay != nil {
		_ = sess.relay.Close()
	}
	if cleanWorktree && sess.worktree != "" {
		_ = os.RemoveAll(sess.worktree)
	}
	_ = os.RemoveAll(filepath.Join(s.cfg.StateDir, "identity", sess.summary.Session))

	s.mu.Lock()
	delete(s.sessions, sess.summary.Session)
	s.mu.Unlock()
	s.persist()
}

// orphanSession moves a self-stopped session (watchdog) into the orphan set.
// The relay is stopped and the session cert is shredded: an orphan is dead
// (re-adoption is a later phase), so a re-provision of the same identity must
// re-enroll with a fresh cert rather than reuse the stale one.
func (s *Service) orphanSession(sess *session) {
	if sess.relay != nil {
		_ = sess.relay.Close()
	}
	_ = os.RemoveAll(filepath.Join(s.cfg.StateDir, "identity", sess.summary.Session))

	s.mu.Lock()
	delete(s.sessions, sess.summary.Session)
	sum := sess.summary
	sum.State = "orphaned"
	s.orphans[sum.Session] = sum
	s.mu.Unlock()
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
		if sess.cancel != nil {
			sess.cancel()
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
func (s *Service) persist() {
	s.mu.Lock()
	var all []sockproto.SessionSummary
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
