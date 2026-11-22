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
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
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

	// An unreadable cwd is passed on as "": git then fails to find a branch,
	// which stops gt done when the session named one.
	cwd, _ := os.Getwd()
	if err := pushWorkBeforeDone(toolName, os.Args[1:], cwd, os.Getenv("GT_BRANCH")); err != nil {
		fmt.Fprintf(os.Stderr, "gt-proxy-client: %v\n", err)
		os.Exit(1)
	}

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

// pushTimeout bounds the pre-done push and its fallback checks. It matches the
// exec RPC's own ceiling: past that the proxied gt done could not run anyway.
const pushTimeout = 5 * time.Minute

// pushWorkBeforeDone pushes the polecat's branch to the host before a proxied
// `gt done`. gt done runs on the host, against the host's copy of the branch;
// commits that exist only in this worktree would be invisible to it, and the
// bead would complete without the work. A failed push is therefore an error
// that stops gt done.
//
// sessionBranch is the work branch the session was started on (GT_BRANCH).
// When it is set it is authoritative: gt done is refused from a detached
// HEAD, from another branch, or from outside the worktree, because each of
// those would forward gt done with nothing pushed. Without it (a session that
// predates the branch being sent, or a detached host worktree, which reports
// "HEAD") only a checked-out polecat/ branch is pushed and anything else is
// left alone.
func pushWorkBeforeDone(tool string, args []string, dir, sessionBranch string) error {
	if tool != "gt" || len(args) == 0 || args[0] != "done" {
		return nil
	}
	if sessionBranch == "HEAD" {
		sessionBranch = ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()

	out, err := runGit(exec.CommandContext(ctx, "git", "symbolic-ref", "--quiet", "--short", "HEAD"), dir)
	branch := strings.TrimSpace(out)
	if sessionBranch != "" {
		if err != nil {
			return fmt.Errorf("gt done must run on the session's branch %s, but no branch is checked out here (detached HEAD, or not the worktree): nothing was pushed to the host", sessionBranch)
		}
		if branch != sessionBranch {
			return fmt.Errorf("gt done must run on the session's branch %s, but %s is checked out: nothing was pushed to the host", sessionBranch, branch)
		}
	} else if err != nil || !strings.HasPrefix(branch, "polecat/") {
		return nil // not a worktree, a detached HEAD or some other branch: nothing of ours to push
	}

	// push.default=current sends HEAD to the same-named branch on the host,
	// whatever upstream the worktree has configured.
	pushOut, pushErr := runGit(exec.CommandContext(ctx, "git", "-c", "push.default=current", "push", "origin", "HEAD"), dir)
	if pushErr == nil {
		return nil
	}
	// Host-side gt done can rewrite the branch (it rebases onto the target) and
	// then fail at a later step. On the retry this push is rejected as
	// non-fast-forward although the host already holds every patch. In that
	// case there is nothing left to push and gt done can go ahead.
	if hostHasAllPatches(ctx, dir, branch) {
		return nil
	}
	hint := ""
	switch {
	case strings.Contains(pushOut, "non-fast-forward") || strings.Contains(pushOut, "fetch first"):
		hint = fmt.Sprintf("\nThe host's copy of the branch has moved. Run: git pull --rebase origin %s", branch)
	case strings.Contains(pushOut, "Could not update working tree") || strings.Contains(pushOut, "would be overwritten"):
		hint = "\nThe host's worktree for this polecat has local changes or an untracked file in the way. It cannot be fixed from here: escalate so an operator can clean it."
	}
	return fmt.Errorf("pushing %s to the host before gt done: %v: %s%s", branch, pushErr, strings.TrimSpace(pushOut), hint)
}

// hostHasAllPatches reports whether every commit on HEAD is already on the
// host's copy of branch, by patch rather than by hash, so a rebased equivalent
// counts. The revision range is fed on stdin, not the command line.
func hostHasAllPatches(ctx context.Context, dir, branch string) bool {
	if _, err := runGit(exec.CommandContext(ctx, "git", "fetch", "--quiet", "origin"), dir); err != nil {
		return false
	}
	cmd := exec.CommandContext(ctx, "git", "rev-list", "--cherry-pick", "--right-only", "--no-merges", "--stdin")
	cmd.Stdin = strings.NewReader("refs/remotes/origin/" + branch + "...HEAD\n")
	out, err := runGit(cmd, dir)
	return err == nil && strings.TrimSpace(out) == ""
}

// runGit runs a git command in dir and returns its combined output. It never
// prompts: a credential prompt on a relay would otherwise block gt done with
// no output. Callers build the command from literal arguments.
func runGit(cmd *exec.Cmd, dir string) (string, error) {
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
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
