// gt-worker-client is the persistent worker service for the socket execution
// provider (docs/design/remote-polecat-execution-socket.md): it listens for
// the orchestrator's control connections and runs polecat sessions on this
// machine — per-session CSR over the connection, local mTLS relay, worktree
// cloned through the relay, optional work container, checkpoint loop and
// watchdog.
//
// Listeners: a unix socket gated by filesystem permissions + a pre-shared
// token (-token), or a TCP address with MANUAL mutual TLS (-tls-cert/-tls-key
// present this machine's cert; -tls-client-ca verifies the orchestrator).
// Enrollment-managed TLS (gt worker enroll) is a later increment.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/steveyegge/gastown/internal/workerclient"
)

func main() {
	var (
		listen      = flag.String("listen", "", "unix:///path/to.sock or TCP host:port (required)")
		token       = flag.String("token", "", "pre-shared token (required for unix listeners; refused on TCP)")
		tlsCert     = flag.String("tls-cert", "", "this machine's TLS cert (TCP)")
		tlsKey      = flag.String("tls-key", "", "this machine's TLS key (TCP)")
		tlsClientCA = flag.String("tls-client-ca", "", "CA that signs orchestrator client certs (TCP)")
		stateDir    = flag.String("state-dir", "/var/lib/gt-worker", "session state directory")
		proxyURL    = flag.String("proxy-url", "", "host proxy base URL, e.g. https://gt-host.example:9876 (required)")
		gtDir       = flag.String("gt-dir", "", "injected gastown bits dir for container mode")
		workerID    = flag.String("worker-id", "", "enrolled machine name reported in hello_ack")
		maxSessions = flag.Int("max-sessions", 1, "concurrent session cap")
		execModes   = flag.String("exec-modes", "native", "comma-separated supported exec modes (native,container)")
		docker      = flag.Bool("docker", false, "advertise a usable docker daemon")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *listen == "" || *proxyURL == "" {
		log.Error("missing required flags: -listen and -proxy-url are required")
		os.Exit(2)
	}

	svc, err := workerclient.New(workerclient.Config{
		WorkerID:    *workerID,
		Token:       *token,
		StateDir:    *stateDir,
		ProxyURL:    *proxyURL,
		GTDir:       *gtDir,
		MaxSessions: *maxSessions,
		ExecModes:   strings.Split(*execModes, ","),
		Docker:      *docker,
		Log:         log,
	})
	if err != nil {
		log.Error("service config invalid", "err", err)
		os.Exit(2)
	}

	var ln net.Listener
	if path, ok := strings.CutPrefix(*listen, "unix://"); ok {
		// §3.3: a unix listener is gated by fs permissions + the token.
		if *token == "" {
			log.Error("a unix listener requires -token (§3.3)")
			os.Exit(2)
		}
		_ = os.Remove(path) // stale socket from a previous run
		// Tighten umask around the bind so the socket is never briefly
		// exposed at default perms between Listen and Chmod (§3.3 fs gate
		// TOCTOU). Restore it immediately after.
		oldMask := syscall.Umask(0o077)
		ln, err = net.Listen("unix", path)
		syscall.Umask(oldMask)
		if err != nil {
			log.Error("listen", "err", err)
			os.Exit(1)
		}
		if err := os.Chmod(path, 0700); err != nil {
			log.Error("restrict socket permissions", "err", err)
			os.Exit(1)
		}
	} else {
		// §3.3: token auth is REFUSED on TCP — mutual TLS is mandatory.
		if *token != "" {
			log.Error("token auth is refused on TCP listeners (§3.3); use mutual TLS")
			os.Exit(2)
		}
		if *tlsCert == "" || *tlsKey == "" || *tlsClientCA == "" {
			log.Error("a TCP listener requires -tls-cert, -tls-key, and -tls-client-ca")
			os.Exit(2)
		}
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			log.Error("load machine cert", "err", err)
			os.Exit(1)
		}
		caPEM, err := os.ReadFile(*tlsClientCA)
		if err != nil {
			log.Error("read client CA", "err", err)
			os.Exit(1)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			log.Error("client CA file has no valid certificates", "file", *tlsClientCA)
			os.Exit(1)
		}
		tcp, err := net.Listen("tcp", *listen)
		if err != nil {
			log.Error("listen", "err", err)
			os.Exit(1)
		}
		ln = tls.NewListener(tcp, &tls.Config{
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pool,
			MinVersion:   tls.VersionTLS13,
		})
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Info("gt-worker-client listening", "addr", *listen, "workerID", *workerID, "execModes", *execModes)
	if err := svc.Serve(ctx, ln); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
	log.Info("gt-worker-client stopped (sessions flushed)")
}
