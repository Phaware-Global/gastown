// gt-worker-agent is the worker-side gastown supervisor for remote polecat
// execution (docs/design/remote-polecat-execution.md §3). This increment
// covers identity bootstrap + the local mTLS-terminating relay (§6.1, §7.2):
//
//  1. Generate the polecat's private key locally (it never leaves the worker)
//     and obtain a CA-signed client cert for gt-<rig>-<name> via the signer.
//  2. Run the local plaintext relay: the agent's gt/bd/git talk to it in the
//     clear on the worker; the relay forwards over mTLS to the host proxy.
//
// The checkpoint loop, interruption watcher, and self-release watchdog
// (§9.2–9.5) arrive in later increments. Provider backends may ship this
// program under a provider-specific name; the provider channel supplies the
// CSR-signing hop, defaulting here to the proxy admin API for host-local and
// test use.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/steveyegge/gastown/internal/worker"
)

func main() {
	var (
		rig      = flag.String("rig", "", "rig name (required)")
		name     = flag.String("name", "", "polecat name (required)")
		proxyURL = flag.String("proxy-url", "", "host proxy base URL, e.g. https://gt-host.example:9876 (required)")
		signURL  = flag.String("sign-url", "http://127.0.0.1:9877", "proxy admin base URL for CSR signing")
		listen   = flag.String("relay-listen", "127.0.0.1:9899", "local relay listen address (see design §6.1.1 for container networking)")
		stateDir = flag.String("state-dir", "", "identity/state directory (default: /dev/shm/gt-worker or $TMPDIR/gt-worker)")
		ttl      = flag.String("cert-ttl", "", "requested cert TTL (empty = server default)")
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

	go func() {
		<-ctx.Done()
		log.Info("shutting down relay")
		_ = relay.Close()
	}()

	log.Info("relay starting", "listen", *listen, "upstream", *proxyURL)
	if err := relay.Serve(*listen); err != nil {
		log.Error("relay error", "err", err)
		os.Exit(1)
	}
}
