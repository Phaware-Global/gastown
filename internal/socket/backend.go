package socket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/execution"
	"github.com/steveyegge/gastown/internal/sockproto"
	"github.com/steveyegge/gastown/internal/version"
)

// provisionTimeout bounds the full session_open → session_ready exchange
// (image pull can be slow, but not unbounded).
const provisionTimeout = 10 * time.Minute

// controlTimeout bounds a single control request (discover, teardown) so a
// hung worker can never block a caller that passed a deadline-less context.
// A var, not a const, so tests can exercise the self-imposed cap without
// waiting the full minute.
var controlTimeout = 60 * time.Second

// Signer signs a session CSR into a polecat cert — the daemon's proxy-CA
// hook, injected so this package does not import the proxy CA directly (and
// so tests can stub it). It is the socket form of core §7.2 step 2: the CSR
// arrives over the mTLS control connection, and the daemon signs it only for
// the identity of the session it opened (§4.2 channel binding).
type Signer interface {
	// SignSessionCSR signs csrPEM for the given polecat identity. rig/polecat
	// travel STRUCTURALLY rather than as a pre-built CN so no implementation
	// has to split one back apart: cnToIdentity splits on the last hyphen, so a
	// hyphenated polecat name makes that lossy.
	SignSessionCSR(ctx context.Context, csrPEM []byte, rig, polecat string) (certPEM, caPEM []byte, notAfter time.Time, err error)
}

// SocketBackend is the ExecutionBackend for pre-provisioned socket workers.
type SocketBackend struct {
	cfg      *config.ExecutionConfig
	settings *Settings

	// OrchestratorID / GTVersion identify this daemon in the handshake.
	OrchestratorID string
	GTVersion      string
	// Signer performs the proxy-CA CSR signing (§4.2). Required for Provision
	// to complete a fresh session; a nil Signer still allows Discover/Teardown
	// and reattach of an already-certified live session. New() installs the
	// admin-API signer, so a backend built through the registry always has one
	// — a nil Signer here means a caller constructed the struct directly.
	Signer Signer

	// dialFn is overridable in tests; nil uses the real dialer.
	dialFn func(ctx context.Context, s *Settings, orchestratorID, gtVersion string) (*conn, error)
}

// New builds a SocketBackend from a rig execution config, wired for real use:
// the CSR signer is installed here, so a backend resolved through
// execution.ForConfig can complete a session bringup. (Provision needs it for
// EVERY session — the worker generates its key locally and CSRs over the
// control connection regardless of exec mode — so a backend without one is
// useful for nothing but Discover/Teardown.)
func New(cfg *config.ExecutionConfig) (*SocketBackend, error) {
	s, err := parseSettings(cfg)
	if err != nil {
		return nil, err
	}
	signer, err := newAdminSigner(s.AdminURL)
	if err != nil {
		return nil, err
	}
	return &SocketBackend{
		cfg:            cfg,
		settings:       s,
		Signer:         signer,
		OrchestratorID: orchestratorID(),
		GTVersion:      version.GTVersion,
	}, nil
}

func (b *SocketBackend) dial(ctx context.Context) (*conn, error) {
	if b.dialFn != nil {
		return b.dialFn(ctx, b.settings, b.OrchestratorID, b.GTVersion)
	}
	return dial(ctx, b.settings, b.OrchestratorID, b.GTVersion)
}

// endpointFor builds the Endpoint handle for a session on this worker.
func (b *SocketBackend) endpointFor(spec execution.PolecatSpec) execution.Endpoint {
	return execution.Endpoint{
		Backend:  BackendName,
		ID:       spec.Session,
		Address:  b.settings.Address,
		Identity: spec.Identity(),
	}
}

// stagedWorkerClient reports whether a binary refresh left a new worker
// service staged: pushed, but not running until the worker is idle.
func stagedWorkerClient(pushed []PushResult) bool {
	for _, r := range pushed {
		if r.Name == "gt-worker-client" && r.Applied == "staged" {
			return true
		}
	}
	return false
}

// errWorkerRestarting marks a refusal from a worker that is exiting onto a
// pushed binary. Its supervisor brings it straight back, so this is transient
// by construction — see Provision's retry.
var errWorkerRestarting = errors.New("socket: worker is restarting")

// provisionRetries is how many extra attempts a transient worker restart gets.
// A supervisor restart is sub-second; three attempts over ~2s covers it without
// turning a genuinely dead worker into a long stall.
const provisionRetries = 2

// provisionRetryDelay is the wait before each retry.
var provisionRetryDelay = 500 * time.Millisecond

// errUpgradeStaged is the restart a provision brings about itself: its binary
// refresh staged a new worker service on an idle worker, which exits to apply
// it once this connection closes.
var errUpgradeStaged = fmt.Errorf("%w onto the binary this provision staged", errWorkerRestarting)

// stagedRestartWait is how long a worker that this provision sent into that
// restart may stay unreachable before the dial failure is believed. A respawn
// is usually sub-second, but the listener is not up until the new binary has
// started, so the window is wider than the ordinary retry budget.
const stagedRestartWait = 15 * time.Second

// dialError marks a failure to reach the worker at all, as opposed to a
// refusal from it.
type dialError struct{ err error }

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// Provision opens (or reattaches) a session on the worker (§5, core §9.4).
//
// A worker that is restarting onto a pushed binary (§4.1) is retried rather
// than reported: it refuses with a `restarting` code, or its connection simply
// drops, and either way its supervisor has it back within a second. Without
// this the binary refresh would not be the invisible thing the design claims —
// a polecat start that happened to race an upgrade would fail once, and nothing
// upstream retries (buildRemoteArgv calls Provision exactly once).
//
// Both attempts and delays live inside the caller's deadline, so a genuinely
// dead worker still fails promptly rather than stalling for the full timeout.
func (b *SocketBackend) Provision(ctx context.Context, spec execution.PolecatSpec) (execution.Endpoint, error) {
	// WithTimeout yields min(caller deadline, provisionTimeout): a
	// deadline-less caller is bounded by the cap; a shorter caller deadline
	// still wins.
	ctx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()

	var ep execution.Endpoint
	var err error
	var stagedAt time.Time // when this provision staged the worker's upgrade
	for attempt := 0; ; attempt++ {
		ep, err = b.provisionOnce(ctx, spec)
		if err == nil {
			return ep, nil
		}
		retry := errors.Is(err, errWorkerRestarting) && attempt < provisionRetries
		if errors.Is(err, errUpgradeStaged) && stagedAt.IsZero() {
			stagedAt = time.Now()
		}
		// The worker exits to apply the upgrade and is not listening until
		// its supervisor has the new binary up. A dial that fails in that
		// window is the restart this provision caused, not a dead worker.
		var de *dialError
		if !stagedAt.IsZero() && errors.As(err, &de) && time.Since(stagedAt) < stagedRestartWait {
			retry = true
		}
		if !retry {
			return ep, err
		}
		slog.Default().Info("socket: worker is restarting onto a pushed binary; retrying provision",
			"session", spec.Session, "attempt", attempt+1)
		select {
		case <-ctx.Done():
			return execution.Endpoint{}, fmt.Errorf("%w (deadline reached while it restarted)", err)
		case <-time.After(provisionRetryDelay):
		}
	}
}

// provisionOnce is one full attempt: dial, refresh binaries, open the session.
func (b *SocketBackend) provisionOnce(ctx context.Context, spec execution.PolecatSpec) (execution.Endpoint, error) {
	c, err := b.dial(ctx)
	if err != nil {
		return execution.Endpoint{}, &dialError{err}
	}
	defer func() { _ = c.close() }()

	// Binary freshness (§5): refresh a worker whose gt_version differs before
	// opening a session on it. Best-effort — a worker that cannot be refreshed
	// still runs sessions, since protocol version (not gt version) is the
	// compatibility gate, and failing a session a user is waiting on because of
	// a version bump would be the wrong trade.
	pushed, err := b.pushTo(ctx, c, false)
	if err != nil {
		slog.Default().Warn("socket: binary refresh skipped", "worker", c.ack.WorkerID, "err", err)
	}

	// Reattach if the handshake already reports this session live (daemon
	// restarted, worker did not) — no new session, idempotent per core §9.4.
	//
	// Only when it is on the branch asked for. The session id names the
	// polecat, not its work: a polecat that finished one bead and is started
	// on the next has a new branch, while the worker still holds the session,
	// worktree and branch of the old one. That session is torn down (which
	// flushes it) and a fresh one opened below. A worker that predates the
	// summary's branch reports none, so its session cannot be told apart from
	// a stale one and is replaced too: reusing it unseen is the failure this
	// guards against, and such a worker only takes its upgrade once the
	// session is gone.
	for i, sess := range c.ack.Sessions {
		if sess.Session != spec.Session || sess.State == "orphaned" {
			continue
		}
		if sess.Branch == spec.Branch {
			return b.endpointFor(spec), nil
		}
		slog.Default().Info("socket: replacing a live session that is on another branch",
			"session", spec.Session, "branch", sess.Branch, "want", spec.Branch)
		tdCtx, cancel := context.WithTimeout(ctx, controlTimeout)
		err := teardownOn(tdCtx, c, spec.Session)
		// The connection's deadline is the context's, and can fire first.
		timedOut := tdCtx.Err() != nil || errors.Is(err, os.ErrDeadlineExceeded)
		cancel()
		// A teardown that empties the worker is also when it applies a staged
		// binary: it exits for its supervisor before it can answer. So a
		// connection that drops here, rather than a refusal or a worker that
		// hangs, is taken for that restart, whether or not this provision did
		// the staging (an earlier one may have). The retry finds the session
		// gone and opens it; a worker that is really down fails the redial.
		if errors.Is(err, errNoReply) && !timedOut {
			return execution.Endpoint{}, fmt.Errorf("replacing the session on branch %q: %w: %w", sess.Branch, errUpgradeStaged, err)
		}
		if err != nil {
			return execution.Endpoint{}, fmt.Errorf("replacing the session on branch %q: %w", sess.Branch, err)
		}
		c.ack.Sessions = append(c.ack.Sessions[:i:i], c.ack.Sessions[i+1:]...)
		break
	}

	// max_sessions gate (§12 decision 1): the worker also enforces this, but
	// failing fast here gives a clearer error than a session_error.
	if cap := c.ack.Capabilities; cap != nil && cap.MaxSessions > 0 && len(c.ack.Sessions) >= cap.MaxSessions {
		return execution.Endpoint{}, fmt.Errorf("socket: worker at %s is at its session limit (%d)", b.settings.Address, cap.MaxSessions)
	}

	// A worker that predates agent files would drop them without an error and
	// start the agent with no settings, so no hooks and no guards. The binary
	// refresh above is best-effort and a busy worker defers its own upgrade,
	// so this is reachable; refuse rather than open that session.
	//
	// The handshake predates the refresh above. If that refresh just staged a
	// new worker binary, a worker with no sessions applies it as soon as this
	// connection closes, and the next handshake may well carry the capability:
	// retry rather than report a refusal the upgraded worker would not make.
	// A worker with sessions keeps the old binary until they end, so a retry
	// would only push the same binaries at it again: that one is refused.
	if len(spec.Files) > 0 && !c.ack.Capabilities.GetAgentFiles() {
		if stagedWorkerClient(pushed) && len(c.ack.Sessions) == 0 {
			return execution.Endpoint{}, fmt.Errorf("socket: worker at %s cannot receive the agent's settings file yet, a newer binary was just staged on it: %w", b.settings.Address, errUpgradeStaged)
		}
		return execution.Endpoint{}, fmt.Errorf("socket: worker at %s runs a binary that cannot receive the agent's settings file (gt %s): upgrade the worker, or wait for it to go idle and apply its pending update", b.settings.Address, c.ack.GTVersion)
	}

	open := &sockproto.Message{
		Type:               sockproto.TypeSessionOpen,
		Session:            spec.Session,
		Rig:                spec.Rig,
		Polecat:            spec.Polecat,
		ExecMode:           b.cfg.ExecMode,
		Image:              b.cfg.Image,
		NetworkMode:        b.cfg.NetworkMode(),
		CheckpointInterval: b.cfg.CheckpointInterval().String(),
		MaxRuntime:         durationOrEmpty(b.cfg),
		Branch:             spec.Branch,
		Files:              spec.Files,
	}
	resp, err := c.request(ctx, open)
	if err != nil {
		// A connection that dies mid-bringup is the other face of a restart:
		// the worker exited between our handshake and this request. Treat it as
		// transient — a worker that is really gone fails again on the retry.
		return execution.Endpoint{}, fmt.Errorf("socket: session_open: %w: %w", errWorkerRestarting, err)
	}

	// A container session's agent reaches the proxy only over mTLS with the
	// session cert, so it MUST go through the CSR exchange; a worker that
	// skips straight to session_ready would leave a container session with no
	// polecat identity. Enforce it rather than trust the worker's choice.
	// (Native no-cert modes may legitimately skip it — allowed for exec_mode
	// native; revisit when native lands.)
	if resp.Type == sockproto.TypeSessionReady && b.cfg.ExecMode != config.ExecModeNative {
		return execution.Endpoint{}, fmt.Errorf("socket: worker returned session_ready without a CSR exchange for a %s session — no polecat cert would be issued (§4.2)", b.cfg.ExecMode)
	}

	// The worker generates its key locally and returns a CSR (§4.2, core
	// §7.2): sign it (bound to the session's expected CN) and return the cert.
	if resp.Type == sockproto.TypeCSR {
		if b.Signer == nil {
			return execution.Endpoint{}, fmt.Errorf("socket: worker sent a CSR but no Signer is configured")
		}
		certPEM, caPEM, notAfter, err := b.Signer.SignSessionCSR(ctx, []byte(resp.CSRPEM), spec.Rig, spec.Polecat)
		if err != nil {
			return execution.Endpoint{}, fmt.Errorf("socket: signing session CSR: %w", err)
		}
		resp, err = c.request(ctx, &sockproto.Message{
			Type:     sockproto.TypeCert,
			Session:  spec.Session,
			CertPEM:  string(certPEM),
			CAPEM:    string(caPEM),
			NotAfter: notAfter,
		})
		if err != nil {
			return execution.Endpoint{}, fmt.Errorf("socket: sending cert: %w", err)
		}
	}

	switch resp.Type {
	case sockproto.TypeSessionReady:
		return b.endpointFor(spec), nil
	case sockproto.TypeSessionError, sockproto.TypeError:
		if resp.Code == "restarting" {
			return execution.Endpoint{}, fmt.Errorf("%w: %s", errWorkerRestarting, resp.Msg)
		}
		return execution.Endpoint{}, fmt.Errorf("socket: worker rejected session %s: %s: %s", spec.Session, resp.Code, resp.Msg)
	default:
		return execution.Endpoint{}, fmt.Errorf("socket: unexpected response %q to session bringup", resp.Type)
	}
}

// WrapCommand returns the launcher argv for the exec stream (§5): a thin
// `gt worker-attach` that opens an exec stream to the worker and pipes stdio.
// It is gt rather than the standalone gt-worker-attach so that only one
// executable on the orchestrator needs to be allowed to reach the worker.
// The launcher — not this argv — carries the agent command + non-secret env
// to the worker in the exec payload (core §7.4); secret env is delivered
// worker-side via the agent env file (§8), never here.
func (b *SocketBackend) WrapCommand(ep execution.Endpoint, agentArgv []string, env map[string]string) ([]string, error) {
	if len(agentArgv) == 0 {
		return nil, fmt.Errorf("socket: empty agent argv")
	}
	// The launcher needs the worker credential, but argv is world-readable via
	// ps — so the token/worker-name travel in the SESSION ENV the daemon sets
	// on the tmux pane (GT_WORKER_TOKEN / GT_WORKER_NAME), which the launcher
	// reads from its own environment. Only the address, session id, and the
	// agent argv (already non-secret, and quoted by the caller) go in argv.
	//
	// The launcher forwards the non-secret session env from that same
	// environment into the exec payload (core §7.4); nothing sensitive is
	// placed on a command line.
	if env != nil {
		if b.settings.Token != "" {
			env["GT_WORKER_TOKEN"] = b.settings.Token
		}
		if b.settings.TLS.WorkerName != "" {
			env["GT_WORKER_NAME"] = b.settings.TLS.WorkerName
		}
	}
	argv := []string{
		"gt", "worker-attach",
		"--address", ep.Address,
		"--session", ep.ID,
		"--",
	}
	return append(argv, agentArgv...), nil
}

// Teardown ends the session on the persistent machine (§6): graceful
// shutdown (flush) then teardown; the machine survives.
func (b *SocketBackend) Teardown(ctx context.Context, ep execution.Endpoint) error {
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()

	c, err := b.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.close() }()
	return teardownOn(ctx, c, ep.ID)
}

// errNoReply marks a request the worker never answered: the connection
// dropped or timed out, as opposed to a refusal.
var errNoReply = errors.New("no reply from the worker")

// teardownOn ends session on an open control connection.
func teardownOn(ctx context.Context, c *conn, session string) error {
	// Graceful stop first (§6 step 1-2): best-effort — a worker that already
	// tore the session down answers with an error we tolerate.
	_, _ = c.request(ctx, &sockproto.Message{
		Type:         sockproto.TypeShutdown,
		Session:      session,
		Reason:       "teardown",
		GraceSeconds: 60,
	})
	resp, err := c.request(ctx, &sockproto.Message{Type: sockproto.TypeTeardown, Session: session})
	if err != nil {
		return fmt.Errorf("socket: teardown: %w: %w", errNoReply, err)
	}
	if resp.Type == sockproto.TypeError {
		return fmt.Errorf("socket: teardown %s: %s: %s", session, resp.Code, resp.Msg)
	}
	return nil
}

// Discover lists live sessions on the worker by identity (§5, core §9.4).
func (b *SocketBackend) Discover(ctx context.Context, filter execution.IdentityTags) ([]execution.Endpoint, error) {
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()

	c, err := b.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.close() }()

	resp, err := c.request(ctx, &sockproto.Message{
		Type:    sockproto.TypeDiscover,
		Rig:     filter.Rig,
		Polecat: filter.Polecat,
	})
	if err != nil {
		return nil, fmt.Errorf("socket: discover: %w", err)
	}
	if resp.Type == sockproto.TypeError {
		return nil, fmt.Errorf("socket: discover: %s: %s", resp.Code, resp.Msg)
	}

	var eps []execution.Endpoint
	for _, sess := range resp.Sessions {
		// Client-side filter belt-and-suspenders (the worker filters too).
		if filter.Rig != "" && sess.Rig != filter.Rig {
			continue
		}
		if filter.Polecat != "" && sess.Polecat != filter.Polecat {
			continue
		}
		eps = append(eps, execution.Endpoint{
			Backend: BackendName,
			ID:      sess.Session,
			Address: b.settings.Address,
			Identity: execution.IdentityTags{
				Rig:     sess.Rig,
				Polecat: sess.Polecat,
				Session: sess.Session,
			},
		})
	}
	return eps, nil
}

// durationOrEmpty returns the configured max_runtime string only when
// explicitly set (matching the reaper's opt-in cap semantics), else "".
func durationOrEmpty(cfg *config.ExecutionConfig) string {
	if !cfg.MaxRuntimeSet() {
		return ""
	}
	return cfg.MaxRuntime().String()
}

var _ execution.Backend = (*SocketBackend)(nil)
