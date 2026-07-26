// gt-proxy-client is the pass-through binary installed as both `gt` and `bd`
// wherever a polecat runs off the orchestrator host — inside a work container,
// or on a remote worker.
//
// With GT_PROXY_URL set it forwards os.Args[1:] to the control plane and
// proxies the response, in one of two shapes: DIRECT mTLS when
// GT_PROXY_CERT/KEY/CA are supplied, or RELAY (plaintext to a loopback URL)
// when they are not — the remote-worker case, where the worker's local relay
// holds the session identity and terminates mTLS upstream, so the agent never
// holds the session key. With GT_PROXY_URL unset it execs the real binary at
// /usr/local/bin/gt.real (or GT_REAL_BIN).
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type execRequest struct {
	Argv []string `json:"argv"`
}

type execResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

func main() {
	// Required environment variables:
	//   GT_PROXY_URL  — proxy base URL (e.g. https://172.17.0.1:9876)
	//   GT_PROXY_CERT — path to PEM client cert (issued by proxy CA)
	//   GT_PROXY_KEY  — path to PEM client private key
	//   GT_PROXY_CA   — path to PEM proxy CA cert (used to verify server cert)
	// Optional:
	//   GT_PROXY_RELAY — "1" permits relay mode to a non-loopback relay (containers)
	//   GT_REAL_BIN   — fallback binary path (default /usr/local/bin/gt.real)
	proxyURL := os.Getenv("GT_PROXY_URL")
	certFile := os.Getenv("GT_PROXY_CERT")
	keyFile := os.Getenv("GT_PROXY_KEY")
	// GT_PROXY_CA is the CA cert for the proxy's server TLS cert.
	// This is the same CA cert as GIT_SSL_CAINFO (which git uses to trust the proxy),
	// but passed separately so the Go HTTP client can also trust the proxy server cert.
	caFile := os.Getenv("GT_PROXY_CA")

	if proxyURL == "" {
		// No proxy configured at all — not a proxied environment, so this is an
		// ordinary invocation: exec the real binary silently.
		execReal()
		return
	}

	httpClient, err := proxyHTTPClient(proxyURL, certFile, keyFile, caFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gt-proxy-client: %v\n", err)
		os.Exit(1)
	}

	// Determine argv: prepend the binary name so the server knows which tool we are.
	argv := os.Args // os.Args[0] is the binary path; the server needs the tool name as argv[0].
	// Replace argv[0] with the tool name (gt or bd) based on the binary name.
	toolName := toolNameFromArg0(os.Args[0])
	argv = append([]string{toolName}, os.Args[1:]...)

	body, err := json.Marshal(execRequest{Argv: argv})
	if err != nil {
		fmt.Fprintf(os.Stderr, "gt-proxy-client: encode request: %v\n", err)
		os.Exit(1)
	}

	resp, err := httpClient.Post(proxyURL+"/v1/exec", "application/json", bytes.NewReader(body)) //nolint:gosec // proxyURL is from trusted env var GT_PROXY_URL
	if err != nil {
		fmt.Fprintf(os.Stderr, "gt-proxy-client: proxy request failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close on response body

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "gt-proxy-client: server error %d: %s\n", resp.StatusCode, msg)
		os.Exit(1)
	}

	var result execResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(os.Stderr, "gt-proxy-client: decode response: %v\n", err)
		os.Exit(1)
	}

	if result.Stdout != "" {
		_, _ = fmt.Fprint(os.Stdout, result.Stdout)
	}
	if result.Stderr != "" {
		_, _ = fmt.Fprint(os.Stderr, result.Stderr)
	}
	os.Exit(result.ExitCode)
}

// toolNameFromArg0 extracts "gt" or "bd" from the argv[0] binary path.
// proxyHTTPClient builds the client for the configured proxy endpoint. There
// are two sanctioned shapes, and which one applies is decided by whether client
// material was supplied — never by guessing:
//
//   - DIRECT mTLS (GT_PROXY_CERT/KEY/CA all set): the caller holds a polecat
//     cert and talks to gt-proxy-server itself.
//   - RELAY (no client material): the caller talks to a worker-LOCAL relay that
//     holds the session identity and terminates mTLS upstream
//     (docs/design/remote-polecat-execution.md §7.2). This is the remote-worker
//     shape: the agent must never hold the session key, so it cannot present a
//     client cert, and requiring one is what made `gt` unusable on a worker —
//     the four-var gate silently fell through to gt.real instead.
//
// Relay mode requires a LOOPBACK url, or GT_PROXY_RELAY=1 for the container
// case where the relay binds the bridge gateway. Plaintext to an arbitrary host
// with neither would be an unauthenticated control-plane call to something this
// process cannot verify.
func proxyHTTPClient(proxyURL, certFile, keyFile, caFile string) (*http.Client, error) {
	someMaterial := certFile != "" || keyFile != "" || caFile != ""
	allMaterial := certFile != "" && keyFile != "" && caFile != ""

	if someMaterial && !allMaterial {
		// A partial set is a misconfiguration, not a mode. Falling back to
		// plaintext here would silently drop mTLS the operator asked for.
		return nil, fmt.Errorf("GT_PROXY_CERT/GT_PROXY_KEY/GT_PROXY_CA must be set together (or all omitted for relay mode); got cert=%t key=%t ca=%t",
			certFile != "", keyFile != "", caFile != "")
	}

	if !allMaterial {
		// A container's relay is legitimately NOT loopback — it binds the docker
		// bridge gateway, firewalled to the container subnet (worker.Relay's
		// AllowNonLoopback) — so relay mode there must be stated rather than
		// inferred. GT_PROXY_RELAY=1 is that statement, and only the worker is
		// in a position to make it: the orchestrator cannot supply the URL it
		// would apply to (endpoints are refused from the wire).
		if os.Getenv("GT_PROXY_RELAY") != "1" {
			if err := requireLoopback(proxyURL); err != nil {
				return nil, err
			}
		}
		return &http.Client{Timeout: 5 * time.Minute}, nil
	}

	clientCert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}
	caPEM, err := os.ReadFile(caFile) //nolint:gosec // caFile is from trusted env var GT_PROXY_CA
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("invalid CA PEM in %s", caFile)
	}
	return &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{clientCert},
			MinVersion:   tls.VersionTLS12,
			RootCAs:      pool,
		}},
	}, nil
}

// requireLoopback rejects a non-loopback relay URL.
func requireLoopback(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("GT_PROXY_URL %q: %w", rawURL, err)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("GT_PROXY_URL %q has no host", rawURL)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("GT_PROXY_URL %q is not loopback and no client certificate was supplied: refusing to send control-plane commands to an unverified host (set GT_PROXY_CERT/KEY/CA for direct mTLS, or point at the worker's local relay)", rawURL)
}

func toolNameFromArg0(arg0 string) string {
	return filepath.Base(arg0)
}

// execReal replaces the current process with the real binary.
func execReal() {
	realBin := os.Getenv("GT_REAL_BIN")
	if realBin == "" {
		realBin = "/usr/local/bin/gt.real"
	}
	if err := syscall.Exec(realBin, os.Args, os.Environ()); err != nil { //nolint:gosec // realBin is from GT_REAL_BIN or hardcoded default
		fmt.Fprintf(os.Stderr, "gt-proxy-client: exec %s: %v\n", realBin, err)
		os.Exit(1)
	}
}
