package socket

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/worker"
)

// DefaultAdminURL is the proxy's default loopback admin base URL (the
// gt-proxy-server default, docs/proxy-server.md).
const DefaultAdminURL = "http://127.0.0.1:9877"

// adminSigner is the daemon-side CSR signer for socket sessions: the worker
// generates its key locally and sends a CSR over the control connection
// (§4.2), and this signs it through the proxy's admin API — the same path
// gt-worker-agent uses (internal/worker.AdminSigner), reached here on the
// worker's behalf because the worker itself must never touch the CA.
//
// This is what makes a socket rig work from `gt polecat start`: without a
// Signer, Provision fails on the CSR for every session, container or native.
type adminSigner struct {
	adminURL string
}

// newAdminSigner validates the admin URL and returns a signer for it.
//
// The admin API is UNAUTHENTICATED by design — its whole protection is that it
// binds to the orchestrator's loopback — so a non-loopback admin URL is
// refused rather than dialed: it would ship every session CSR to an unverified
// signer and then hand the worker back whatever CA that host returned, making
// the returned CA the trust root for the session's proxy connection.
func newAdminSigner(adminURL string) (*adminSigner, error) {
	if adminURL == "" {
		adminURL = DefaultAdminURL
	}
	u, err := url.Parse(adminURL)
	if err != nil {
		return nil, fmt.Errorf("socket: admin_url %q: %w", adminURL, err)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("socket: admin_url %q has no host", adminURL)
	}
	// Hostnames are case-insensitive, so "Localhost" is the same host.
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("socket: admin_url %q is not loopback — the proxy admin API is unauthenticated and must never be reached over a network", adminURL)
		}
	}
	return &adminSigner{adminURL: adminURL}, nil
}

// noRedirectClient is the HTTP client the signer uses. Validating the admin URL
// only proves where the FIRST request goes: a 302 would carry the exchange off
// the loopback, and the CA that comes back becomes the session's trust root. So
// redirects are refused outright — a local admin API has no reason to issue one.
func noRedirectClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("socket: proxy admin API attempted a redirect to %s; refusing to carry a session CSR off the loopback", req.URL.Redacted())
		},
	}
}

// SignSessionCSR signs a session CSR for the given polecat identity and
// returns the leaf cert, the proxy CA, and the leaf's expiry.
func (a *adminSigner) SignSessionCSR(ctx context.Context, csrPEM []byte, rig, polecat string) (certPEM, caPEM []byte, notAfter time.Time, err error) {
	// The admin server enforces the binding: it REFUSES a CSR whose CN is not
	// gt-<rig>-<name>, so the identity comes from these arguments (which the
	// backend takes from the session it opened) and never from worker-supplied
	// bytes. That refusal is the §4.2 channel binding.
	signer := &worker.AdminSigner{AdminURL: a.adminURL, Rig: rig, Name: polecat, Client: noRedirectClient()}
	certPEM, caPEM, err = signer.SignCSR(ctx, csrPEM)
	if err != nil {
		return nil, nil, time.Time{}, err
	}

	// Verify what we are about to hand the worker. The server binds the CN, but
	// this side is what installs the cert as the session's identity: a cert for
	// the wrong CN would give the session another polecat's authz silently, so
	// check rather than trust.
	leaf, err := parseLeaf(certPEM)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	if want := worker.PolecatCN(rig, polecat); leaf.Subject.CommonName != want {
		return nil, nil, time.Time{}, fmt.Errorf("socket: signed cert CN is %q, want %q — refusing to install it as this session's identity",
			leaf.Subject.CommonName, want)
	}

	// The CA travels with the leaf and becomes the worker's trust root for the
	// relay, so check the pair is actually a pair: a CA that did not sign this
	// leaf is either a mixed-up response or a substituted root, and either way
	// the session's mTLS would fail later with something far less obvious.
	ca, err := parseLeaf(caPEM)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("socket: parsing returned CA: %w", err)
	}
	if !ca.IsCA {
		return nil, nil, time.Time{}, fmt.Errorf("socket: returned CA %q is not a CA certificate", ca.Subject.CommonName)
	}
	if err := leaf.CheckSignatureFrom(ca); err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("socket: signed cert was not issued by the returned CA (%q): %w — refusing to install mismatched session material",
			ca.Subject.CommonName, err)
	}
	return certPEM, caPEM, leaf.NotAfter, nil
}

// parseLeaf decodes the first CERTIFICATE block of a PEM chain.
func parseLeaf(certPEM []byte) (*x509.Certificate, error) {
	for rest := certPEM; len(rest) > 0; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("socket: parsing signed cert: %w", err)
		}
		return leaf, nil
	}
	return nil, fmt.Errorf("socket: signed cert contains no CERTIFICATE block")
}
