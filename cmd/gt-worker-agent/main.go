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
	"errors"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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

		execMode     = flag.String("exec-mode", "native", "execution model (§6): \"native\" (agent runs on the worker directly; no container) or \"container\" (idle work container prepared here, agent docker exec'd in by the orchestrator)")
		image        = flag.String("image", "", "work image (required in container mode; §6.2 contract)")
		gtDir        = flag.String("gt-dir", "", "host dir with injected gastown bits, mounted ro at /opt/gt (required in container mode)")
		containerNet = flag.String("container-network", "bridge", "container networking (§6.1.1): \"bridge\" (relay via host.docker.internal; hardening works) or \"host\" (trusted rigs only)")
		sandboxed    = flag.Bool("sandboxed", false, "rig egress posture is sandboxed; refuses host networking (§6.1.1)")
		dockerSock   = flag.Bool("mount-docker-sock", false, "bind-mount /var/run/docker.sock into the work container (§10; trusted rigs only)")
		agentBinary  = flag.String("agent-binary", "", "agent runtime binary to preflight on the image PATH (§6.3)")
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

	// Container mode (§6.1.2): build the work environment now so its
	// StopWork can be wired into the supervisor; Prepare runs once the relay
	// is listening (the design's ordering: cert → relay up → docker run).
	var workEnv *worker.WorkEnv
	if *execMode == "container" {
		if *worktree == "" {
			log.Error("container mode requires -worktree (bind-mounted into the work container)")
			os.Exit(2)
		}
		_, portStr, err := net.SplitHostPort(*listen)
		port, convErr := strconv.Atoi(portStr)
		if err != nil || convErr != nil || port <= 0 {
			log.Error("container mode requires an explicit -relay-listen port (the container must be pointed at it)", "listen", *listen)
			os.Exit(2)
		}
		workEnv, err = worker.NewWorkEnv(worker.WorkEnvConfig{
			Rig:               *rig,
			Polecat:           *name,
			Session:           cn,
			Image:             *image,
			Worktree:          *worktree,
			GTDir:             *gtDir,
			ContainerNetwork:  *containerNet,
			Sandboxed:         *sandboxed,
			MountDockerSocket: *dockerSock,
			RelayPort:         port,
			AgentBinary:       *agentBinary,
		})
		if err != nil {
			log.Error("work environment config invalid", "err", err)
			os.Exit(2)
		}
	} else if *execMode != "native" {
		log.Error("invalid -exec-mode", "mode", *execMode)
		os.Exit(2)
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
		supCfg := worker.SupervisorConfig{
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
		}
		if workEnv != nil {
			// §9.3 step 1: stop the work container before the final flush
			// (this is also what lets the flush skip the quiescence guard —
			// the writer is provably stopped).
			supCfg.StopWork = workEnv.StopWork
		}
		sup := worker.NewSupervisor(supCfg)
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

	// Prepare the idle work container once the relay is actually listening
	// (§6.1.2 ordering); a Prepare failure ends the session — cancel the
	// signal context so the supervisor runs its shutdown and everything
	// unwinds.
	prepFailed := make(chan struct{})
	if workEnv != nil {
		go func() {
			for relay.Addr() == nil {
				select {
				case <-relayCtx.Done():
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
			if err := workEnv.Prepare(ctx); err != nil {
				// An operator shutdown signal during the prep window cancels
				// the in-flight docker calls — that is a normal interrupt,
				// not a preparation fault; the supervisor's StopInterrupted
				// path drives the clean exit. Only an error that actually
				// wraps the cancellation counts: a genuine fault racing the
				// signal keeps its exit-1 diagnostics.
				if errors.Is(err, context.Canceled) {
					log.Info("work environment preparation interrupted by shutdown")
					return
				}
				log.Error("work environment preparation failed", "err", err)
				close(prepFailed)
				cancel()
				return
			}
			log.Info("idle work container ready", "container", workEnv.ContainerName(), "proxyURL", workEnv.ProxyURL())
		}()
	}

	log.Info("relay starting", "listen", *listen, "upstream", *proxyURL)
	if err := relay.Serve(relayCtx, *listen); err != nil {
		log.Error("relay error", "err", err)
		os.Exit(1)
	}
	log.Info("relay stopped")

	exitCode := 0
	if *worktree != "" {
		reason := <-reasonCh // supervisor already finished (it canceled relayCtx)
		log.Info("supervisor stopped", "reason", reason)
		switch reason {
		case worker.StopMaxRuntime:
			exitCode = 3
		case worker.StopDeadman:
			exitCode = 4
		}
	}

	// The session is over for every path that reaches here: remove the work
	// container (the §9.5 container-half of release; StopWork already ran in
	// the supervisor's shutdown sequence).
	if workEnv != nil {
		tdCtx, tdCancel := context.WithTimeout(context.Background(), time.Minute)
		if err := workEnv.Teardown(tdCtx); err != nil {
			log.Warn("work container teardown", "err", err)
		}
		tdCancel()
	}
	select {
	case <-prepFailed:
		exitCode = 1
	default:
	}
	os.Exit(exitCode)
}
