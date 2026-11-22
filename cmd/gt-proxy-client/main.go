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
	Argv  []string `json:"argv"`
	Stdin string   `json:"stdin,omitempty"`
}

// maxForwardedStdin bounds the hook input forwarded with a request. The server
// caps the whole request body at 1 MiB.
const maxForwardedStdin = 512 << 10

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
	unsaved, err := pushWorkBeforeDone(toolName, os.Args[1:], cwd, os.Getenv("GT_BRANCH"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "gt-proxy-client: %v\n", err)
		os.Exit(1)
	}
	argv = withCleanupStatus(argv, unsaved)

	stdin, err := stdinForRequest(toolName, os.Args[1:], os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gt-proxy-client: %v\n", err)
		os.Exit(1)
	}

	body, err := json.Marshal(execRequest{Argv: argv, Stdin: stdin})
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

// pushWorkBeforeDone pushes the polecat's work to the host before a proxied
// `gt done`. gt done runs on the host and reads the commits the session
// pushed to its work ref there; commits that exist only in this worktree
// would be invisible to it, and the bead would complete without the work. A
// failed push is therefore an error that stops gt done, and so are tracked
// changes that were never committed.
//
// sessionBranch is the work branch the session was started on (GT_BRANCH).
// When it is set it is authoritative: gt done is refused from a detached
// HEAD, from another branch, or from outside the worktree, because each of
// those would forward gt done with nothing pushed. Without it (a session that
// predates the branch being sent) only a checked-out polecat/ branch is
// pushed and anything else is left alone.
//
// All of that guards a COMPLETED exit, the one that submits work. With
// --status ESCALATED or DEFERRED the polecat is reporting that it is stuck or
// parking, and must be able to even when it cannot push or has not committed:
// there the push is best effort, and a failure is a warning.
//
// A best-effort exit that left work behind returns the cleanup status that
// describes it: uncommitted, or unpushed, which also covers a worktree that
// could not be examined, since nothing was pushed from it. The host cannot
// see this worktree: it reads its own, which
// sits at the branch's start point, and would record the polecat as clean.
// The caller forwards the status so the host records the risk instead.
func pushWorkBeforeDone(tool string, args []string, dir, sessionBranch string) (unsaved string, err error) {
	if tool != "gt" || len(args) == 0 || args[0] != "done" {
		return "", nil
	}
	if sessionBranch == "HEAD" {
		sessionBranch = ""
	}
	if status := doneStatus(args[1:]); status != "COMPLETED" {
		unsaved, err := pushWork(dir, sessionBranch, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gt-proxy-client: warning: %v (continuing, --status %s does not submit work)\n", err, status)
		}
		return unsaved, nil
	}
	_, err = pushWork(dir, sessionBranch, true)
	return "", err
}

// withCleanupStatus appends --cleanup-status=<status> to a gt done argv when
// there is unsaved work to report and the caller did not pass the flag itself.
func withCleanupStatus(argv []string, status string) []string {
	if status == "" {
		return argv
	}
	for _, a := range argv {
		if a == "--cleanup-status" || strings.HasPrefix(a, "--cleanup-status=") {
			return argv
		}
	}
	return append(argv, "--cleanup-status="+status)
}

// doneStatus returns the --status of a gt done argument list, upper-cased;
// COMPLETED when it is not given, as gt done itself defaults.
func doneStatus(args []string) string {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--status="); ok {
			return strings.ToUpper(v)
		}
		if a == "--status" && i+1 < len(args) {
			return strings.ToUpper(args[i+1])
		}
	}
	return "COMPLETED"
}

// pushWork sends the session's committed work to its work ref on the host.
// With submitting set, tracked changes that are not committed are an error
// and nothing is pushed; without it they are left behind and what is
// committed is still sent.
//
// unsaved is the cleanup status for work this call left behind: uncommitted
// for tracked changes that were not committed, unpushed when the push failed
// or the worktree could not be examined, and empty when everything committed
// reached the host. It is never "unknown": the host does not record that
// value, and would then treat the polecat as clean.
func pushWork(dir, sessionBranch string, submitting bool) (unsaved string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()

	out, headErr := runGit(exec.CommandContext(ctx, "git", "symbolic-ref", "--quiet", "--short", "HEAD"), dir)
	branch := strings.TrimSpace(out)
	if sessionBranch != "" {
		if headErr != nil {
			return "unpushed", fmt.Errorf("gt done must run on the session's branch %s, but no branch is checked out here (detached HEAD, or not the worktree): nothing was pushed to the host", sessionBranch)
		}
		if branch != sessionBranch {
			return "unpushed", fmt.Errorf("gt done must run on the session's branch %s, but %s is checked out: nothing was pushed to the host", sessionBranch, branch)
		}
	} else if headErr != nil || !strings.HasPrefix(branch, "polecat/") {
		return "", nil // not a worktree, a detached HEAD or some other branch: nothing of ours to push
	}

	// gt done on the host reads the pushed commits, never this worktree, so
	// tracked changes that are not committed would be silently left out of
	// the work. (A local polecat's gt done commits them itself; here that is
	// the agent's job.) Untracked files are not work and are ignored.
	if status, err := runGit(exec.CommandContext(ctx, "git", "status", "--porcelain", "--untracked-files=no"), dir); err != nil {
		return "unpushed", fmt.Errorf("checking for uncommitted changes before gt done: %v: %s", err, strings.TrimSpace(status))
	} else if strings.TrimSpace(status) != "" {
		if submitting {
			return "uncommitted", fmt.Errorf("uncommitted changes to tracked files: commit them (or discard them) and run gt done again; nothing was pushed to the host:\n%s", strings.TrimRight(status, "\n"))
		}
		fmt.Fprintf(os.Stderr, "gt-proxy-client: warning: uncommitted changes to tracked files are not sent to the host:\n%s\n", strings.TrimRight(status, "\n"))
		unsaved = "uncommitted"
	}

	// The session's clone sends the work branch to its work ref on the host
	// (remote.origin.push, set when the worker brought the session up), so a
	// plain push with no refspec is all that is needed. Only this session
	// writes that ref; the lease lets a rewritten history (an amend or a
	// rebase after an earlier push) replace it, and nothing else.
	pushOut, pushErr := runGit(exec.CommandContext(ctx, "git", "push", "--force-with-lease", "origin"), dir)
	if pushErr == nil {
		return unsaved, nil
	}
	if unsaved == "" {
		unsaved = "unpushed"
	}
	return unsaved, fmt.Errorf("pushing %s to the host before gt done: %v: %s", branch, pushErr, strings.TrimSpace(pushOut))
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

// stdinForRequest returns the stdin to forward with a proxied call. Only
// `gt tap …` gets one: those are the agent's hook handlers, which are handed
// the tool input on a pipe that the hook runner closes. No other command reads
// stdin here — an agent's shell can leave it open, and reading it would hang.
func stdinForRequest(tool string, args []string, in io.Reader) (string, error) {
	if tool != "gt" || len(args) == 0 || args[0] != "tap" {
		return "", nil
	}
	data, err := io.ReadAll(io.LimitReader(in, maxForwardedStdin+1))
	if err != nil {
		return "", fmt.Errorf("read hook input from stdin: %w", err)
	}
	if len(data) > maxForwardedStdin {
		return "", fmt.Errorf("hook input on stdin exceeds %d bytes", maxForwardedStdin)
	}
	return string(data), nil
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
