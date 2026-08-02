// gt-worker-agent is the worker-side gastown supervisor for remote polecat
// execution (docs/design/remote-polecat-execution.md §3):
//
//  1. Generate the polecat's private key locally (it never leaves the worker)
//     and obtain a CA-signed client cert for gt-<rig>-<name> via the signer
//     (§7.2).
//  2. Run the local plaintext relay: the agent's gt/bd/git talk to it in the
//     clear on the worker; the relay forwards over mTLS to the host proxy
//     (§6.1).
//  3. With -worktree, run the lifecycle supervisor: the continuous checkpoint
//     loop (§9.2), the shutdown flush on interruption (§9.3), and the local
//     max-runtime + dead-man self-release watchdog (§9.5). Self-release in
//     this provider-neutral binary means EXITING with a distinguishing code
//     (3 = max-runtime, 4 = deadman) — the provider's service wrapper maps
//     that to its own release action (terminate the instance, end the
//     session).
//
// Provider backends may ship this program under a provider-specific name; the
// provider channel supplies the CSR-signing hop, defaulting here to the proxy
// admin API for host-local and test use.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/worker"
)

func main() {
	var (
		rig      = flag.String("rig", "", "rig name (required)")
		name     = flag.String("name", "", "polecat name (required)")
		proxyURL = flag.String("proxy-url", "", "host proxy base URL, e.g. https://gt-host.example:9876 (required)")
		signURL  = flag.String("sign-url", "http://127.0.0.1:9877", "proxy admin base URL for CSR signing")
		listen   = flag.String("relay-listen", "127.0.0.1:9899", "local relay listen address (see design §6.1.1 for container networking)")
		allowLAN = flag.Bool("allow-non-loopback-relay", false, "permit a non-loopback relay listen address (bridge-gateway wiring; MUST be firewalled to the container subnet — anything that reaches the relay acts as this polecat)")
		stateDir = flag.String("state-dir", "", "identity/state directory (default: /dev/shm/gt-worker or $TMPDIR/gt-worker)")
		ttl      = flag.String("cert-ttl", "", "requested cert TTL (empty = server default)")

		worktree   = flag.String("worktree", "", "polecat worktree path; enables the checkpoint loop + watchdog (design §9.2–9.5)")
		ckptEvery  = flag.Duration("checkpoint-interval", 5*time.Minute, "checkpoint interval ceiling")
		ckptRef    = flag.String("checkpoint-ref", "", "checkpoint ref (default refs/checkpoints/polecat/<name>)")
		gitRemote  = flag.String("git-remote", "origin", "git remote the checkpoint ref is pushed to (points at the relay in production)")
		maxRuntime = flag.Duration("max-runtime", 0, "worker-side absolute session cap; 0 disables (§9.5)")
		deadman    = flag.Duration("deadman-after", 0, "self-release after this long without control-plane contact; 0 = 4x checkpoint-interval, negative disables")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if *rig == "" || *name == "" || *proxyURL == "" {
		log.Error("missing required flags: -rig, -name, and -proxy-url are required")
		os.Exit(2)
	}

	dir := *stateDir
	if dir == "" {
		// Prefer tmpfs so the key never touches persistent disk (§7.2).
		if fi, err := os.Stat("/dev/shm"); err == nil && fi.IsDir() {
			dir = "/dev/shm/gt-worker"
		} else {
			dir = filepath.Join(os.TempDir(), "gt-worker")
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cn := "gt-" + *rig + "-" + *name
	signer := &worker.AdminSigner{AdminURL: *signURL, Rig: *rig, Name: *name, TTL: *ttl}
	id, err := worker.EnsureIdentity(ctx, dir, cn, signer)
	if err != nil {
		log.Error("identity bootstrap failed", "cn", cn, "err", err)
		os.Exit(1)
	}
	log.Info("identity ready", "cn", cn, "cert", id.CertFile)

	relay, err := worker.NewRelay(*proxyURL, id)
	if err != nil {
		log.Error("relay setup failed", "err", err)
		os.Exit(1)
	}
	relay.AllowNonLoopback = *allowLAN
	if *allowLAN {
		log.Warn("relay may bind a non-loopback address — anything that reaches it authenticates as this polecat; the address MUST be firewalled to the container bridge subnet", "listen", *listen)
	}

	// Shutdown ORDER matters (§9.3): the supervisor's final checkpoint flush
	// pushes THROUGH the relay, so the relay must outlive the supervisor. A
	// shutdown signal cancels the supervisor's context only; the relay's own
	// context is canceled after the supervisor (final flush included) has
	// finished — or directly by the signal when no supervisor is running.
	relayCtx, relayCancel := context.WithCancel(context.Background())
	defer relayCancel()
	reasonCh := make(chan worker.StopReason, 1)
	if *worktree != "" {
		ref := *ckptRef
		if ref == "" {
			ref = worker.CheckpointRefForPolecat(*name)
		}
		sup := worker.NewSupervisor(worker.SupervisorConfig{
			Checkpointer: &worker.Checkpointer{
				Worktree: *worktree,
				Ref:      ref,
				Remote:   *gitRemote,
				Debounce: 2 * time.Second,
			},
			Interval:     *ckptEvery,
			MaxRuntime:   *maxRuntime,
			DeadmanAfter: *deadman,
			Log:          log,
		})
		log.Info("supervisor starting", "worktree", *worktree, "ref", ref,
			"interval", *ckptEvery, "maxRuntime", *maxRuntime)
		go func() {
			reasonCh <- sup.Run(ctx) // signal ctx: interruption stops the supervisor first
			relayCancel()            // ...and only then the relay it flushed through
		}()
	} else {
		go func() {
			<-ctx.Done()
			relayCancel()
		}()
	}

	log.Info("relay starting", "listen", *listen, "upstream", *proxyURL)
	if err := relay.Serve(relayCtx, *listen); err != nil {
		log.Error("relay error", "err", err)
		os.Exit(1)
	}
	log.Info("relay stopped")

	if *worktree != "" {
		reason := <-reasonCh // supervisor already finished (it canceled relayCtx)
		log.Info("supervisor stopped", "reason", reason)
		switch reason {
		case worker.StopMaxRuntime:
			os.Exit(3)
		case worker.StopDeadman:
			os.Exit(4)
		}
	}
}
