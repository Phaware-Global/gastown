package socket

import (
	"context"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/execution"
	"github.com/steveyegge/gastown/internal/sockproto"
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
		// GTVersion is left empty until push_binaries (§11 phase 4) actually
		// consumes it; nothing reads it today, and plumbing the version
		// constant down from internal/cmd would be an import cycle for a field
		// with no reader.
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

// Provision opens (or reattaches) a session on the worker (§5, core §9.4).
func (b *SocketBackend) Provision(ctx context.Context, spec execution.PolecatSpec) (execution.Endpoint, error) {
	// WithTimeout yields min(caller deadline, provisionTimeout): a
	// deadline-less caller is bounded by the cap; a shorter caller deadline
	// still wins.
	ctx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()

	c, err := b.dial(ctx)
	if err != nil {
		return execution.Endpoint{}, err
	}
	defer func() { _ = c.close() }()

	// Reattach if the handshake already reports this session live (daemon
	// restarted, worker did not) — no new session, idempotent per core §9.4.
	for _, sess := range c.ack.Sessions {
		if sess.Session == spec.Session && sess.State != "orphaned" {
			return b.endpointFor(spec), nil
		}
	}

	// max_sessions gate (§12 decision 1): the worker also enforces this, but
	// failing fast here gives a clearer error than a session_error.
	if cap := c.ack.Capabilities; cap != nil && cap.MaxSessions > 0 && len(c.ack.Sessions) >= cap.MaxSessions {
		return execution.Endpoint{}, fmt.Errorf("socket: worker at %s is at its session limit (%d)", b.settings.Address, cap.MaxSessions)
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
	}
	resp, err := c.request(ctx, open)
	if err != nil {
		return execution.Endpoint{}, fmt.Errorf("socket: session_open: %w", err)
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
		return execution.Endpoint{}, fmt.Errorf("socket: worker rejected session %s: %s: %s", spec.Session, resp.Code, resp.Msg)
	default:
		return execution.Endpoint{}, fmt.Errorf("socket: unexpected response %q to session bringup", resp.Type)
	}
}

// WrapCommand returns the launcher argv for the exec stream (§5): a thin
// gt-worker-attach that opens an exec stream to the worker and pipes stdio.
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
		"gt-worker-attach",
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

	// Graceful stop first (§6 step 1-2): best-effort — a worker that already
	// tore the session down answers with an error we tolerate.
	_, _ = c.request(ctx, &sockproto.Message{
		Type:         sockproto.TypeShutdown,
		Session:      ep.ID,
		Reason:       "teardown",
		GraceSeconds: 60,
	})
	resp, err := c.request(ctx, &sockproto.Message{Type: sockproto.TypeTeardown, Session: ep.ID})
	if err != nil {
		return fmt.Errorf("socket: teardown: %w", err)
	}
	if resp.Type == sockproto.TypeError {
		return fmt.Errorf("socket: teardown %s: %s: %s", ep.ID, resp.Code, resp.Msg)
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
