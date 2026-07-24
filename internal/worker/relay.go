package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

// Relay is the worker-local plaintext relay (design §6.1): the agent's
// gt/bd/git talk plaintext HTTP to this listener, and the relay adds the
// polecat's client cert and forwards over mTLS to the host proxy. mTLS
// terminates entirely here, in the worker service — the private key never
// enters the work container or the agent's env.
//
// The listen address is the §6.1.1 decision point: 127.0.0.1:9899 for
// native/host-networking, the docker bridge gateway (or 0.0.0.0 firewalled
// to the bridge subnet) for bridge-networked containers. Anything that can
// reach the listener authenticates to the host proxy AS this polecat, so
// non-loopback binds are refused unless AllowNonLoopback is set — and then
// the address MUST be firewalled to the container bridge subnet.
type Relay struct {
	proxy *httputil.ReverseProxy

	// AllowNonLoopback permits binding a non-loopback listen address (the
	// §6.1.1 bridge-gateway wiring). Off by default: an open relay equals
	// full control-plane impersonation of the polecat.
	AllowNonLoopback bool

	lnMu sync.Mutex
	ln   net.Listener
}

// NewRelay builds a relay that forwards every request to upstream (the host
// proxy base URL, e.g. https://host.example:9876) authenticated as id.
func NewRelay(upstream string, id *Identity) (*Relay, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("parse upstream url: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("upstream must be https, got %q", upstream)
	}
	tlsCfg, err := id.ClientTLS()
	if err != nil {
		return nil, err
	}

	rp := httputil.NewSingleHostReverseProxy(u)
	rp.Transport = &http.Transport{
		TLSClientConfig:       tlsCfg,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       2 * time.Minute,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute, // git streams can be slow to first byte
	}
	return &Relay{proxy: rp}, nil
}

// Serve listens on listenAddr and blocks serving relay traffic until ctx is
// canceled or the listener is closed via Close. It reports the bound address
// through Addr once listening (listenAddr may use port 0 in tests).
//
// Non-loopback listen addresses are refused unless AllowNonLoopback is set
// (see the type comment).
func (r *Relay) Serve(ctx context.Context, listenAddr string) error {
	if !r.AllowNonLoopback {
		if host, _, err := net.SplitHostPort(listenAddr); err != nil || host == "" || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			return fmt.Errorf("relay listen address %q is not a loopback IP: anything that reaches the relay acts as this polecat on the control plane — bind 127.0.0.1/::1, or set AllowNonLoopback for a bridge-gateway bind firewalled to the container subnet", listenAddr)
		}
	}

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("relay listen: %w", err)
	}
	r.lnMu.Lock()
	r.ln = ln
	r.lnMu.Unlock()

	srv := &http.Server{
		Handler: r.proxy,
		// Bound only the header read: request and response BODIES are live
		// git pack streams (push uploads, clone downloads) with no safe upper
		// duration — an absolute Read/WriteTimeout would sever a large clone
		// mid-stream. Slow-loris exposure is bounded by ReadHeaderTimeout +
		// IdleTimeout plus the transport's own dial/TLS/response-header
		// timeouts, on a loopback-by-default listener.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	// Stop serving when ctx is canceled — including a ctx that was already
	// canceled before the listener bound (a shutdown signal racing startup
	// must not leave the relay serving forever).
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = r.Close()
		case <-stop:
		}
	}()

	err = srv.Serve(ln)
	if err == http.ErrServerClosed || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Addr returns the bound listener address, or nil before Serve has bound it.
func (r *Relay) Addr() net.Addr {
	r.lnMu.Lock()
	defer r.lnMu.Unlock()
	if r.ln == nil {
		return nil
	}
	return r.ln.Addr()
}

// Close stops the relay listener. In-flight requests are not drained; the
// worker-agent shutdown sequence stops the agent first (§9.3), so nothing
// should be mid-request by the time the relay closes.
func (r *Relay) Close() error {
	r.lnMu.Lock()
	defer r.lnMu.Unlock()
	if r.ln == nil {
		return nil
	}
	err := r.ln.Close()
	r.ln = nil
	return err
}
