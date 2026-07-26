package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/workerclient"
)

// gt worker service — supervise gt-worker-client on this machine
// (docs/design/remote-polecat-execution-socket.md §11 phase 5).
//
// Enrollment gives a machine its identity; this makes it a machine that is
// actually RUNNING, across reboots and crashes. A worker that stays down does
// not merely idle: the orchestrator reports its sessions orphaned and
// re-provisions them elsewhere, so "comes back by itself" is the whole point.
var workerServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Run gt-worker-client as a supervised background service",
}

var workerServiceInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install and start the worker service (macOS launchd)",
	Long: `Install a launchd job that keeps gt-worker-client running on this machine.

Run this on the WORKER, after it has been enrolled from the orchestrator
(gt worker enroll <name> --generate-token). The proxy URL is the orchestrator's
gt-proxy-server, reachable from this machine — not a loopback address, unless
the orchestrator IS this machine.

Secrets never go on the command line: a unix listener's pre-shared token is
read from GT_WORKER_TOKEN, which the job sources from <state-dir>/worker.env
(create it 0600). Agent credentials come from --agent-env-file, which the
worker reads itself and never transmits.

Example (TCP, mTLS, enrolled as this machine's name):

  gt worker service install \
      --listen 0.0.0.0:9878 \
      --proxy-url https://orchestrator.local:9876 \
      --worker-name mac-mini-1 \
      --agent-env-file ~/Library/Application\ Support/gt-worker/agent.env`,
	RunE: func(cmd *cobra.Command, args []string) error {
		o := workerServiceOpts{}
		o.Listen, _ = cmd.Flags().GetString("listen")
		o.ProxyURL, _ = cmd.Flags().GetString("proxy-url")
		o.StateDir, _ = cmd.Flags().GetString("state-dir")
		o.WorkerName, _ = cmd.Flags().GetString("worker-name")
		o.AgentEnvFile, _ = cmd.Flags().GetString("agent-env-file")
		o.TLSDir, _ = cmd.Flags().GetString("tls-dir")
		o.ExecModes, _ = cmd.Flags().GetString("exec-modes")
		o.GTDir, _ = cmd.Flags().GetString("gt-dir")
		o.MaxSessions, _ = cmd.Flags().GetInt("max-sessions")

		plan, err := planWorkerService(o)
		if err != nil {
			return err
		}
		if err := installWorkerService(plan); err != nil {
			return err
		}
		fmt.Printf("Installed and started com.gastown.worker\n  plist:  %s\n  binary: %s\n  state:  %s\n  log:    %s\n  gt/bd:  %s -> %s\n",
			plan.PlistPath, plan.Binary, plan.StateDir, filepath.Join(plan.StateDir, "worker.log"),
			plan.ShimDir, plan.ProxyClient)
		if strings.HasPrefix(o.Listen, "unix://") {
			fmt.Printf("\nUnix listener: put the pre-shared token in %s as\n  GT_WORKER_TOKEN=…\n(mode 0600), then: gt worker service restart\n",
				filepath.Join(plan.StateDir, "worker.env"))
		}
		return nil
	},
}

// workerServiceOpts is the operator's requested worker configuration.
type workerServiceOpts struct {
	Listen       string
	ProxyURL     string
	StateDir     string
	WorkerName   string
	AgentEnvFile string
	TLSDir       string
	ExecModes    string
	GTDir        string
	MaxSessions  int
}

// workerServicePlan is a validated, fully-resolved job description: every path
// absolute and checked, every argument decided. Separating it from the install
// keeps the validation testable without touching launchd.
type workerServicePlan struct {
	Binary      string
	StateDir    string
	PlistPath   string
	Args        []string
	Path        string
	Plist       []byte
	ShimDir     string // <state-dir>/bin, first on the agent's PATH
	ProxyClient string // what the gt/bd shims point at
}

// planWorkerService validates opts and renders the job.
//
// Everything that can be wrong is caught HERE: a launchd job with a bad path or
// missing TLS material loads happily and then refuses every connection, and the
// operator finds out from a provision error on a different machine.
func planWorkerService(o workerServiceOpts) (*workerServicePlan, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("gt worker service currently supports macOS/launchd only; on Linux run gt-worker-client under systemd (see docs/socket-worker.md)")
	}
	if o.Listen == "" || o.ProxyURL == "" {
		return nil, fmt.Errorf("--listen and --proxy-url are required")
	}
	unix := strings.HasPrefix(o.Listen, "unix://")
	if !unix && o.TLSDir == "" {
		return nil, fmt.Errorf("a TCP listener requires --tls-dir (the enrollment output holding %s/%s/%s)",
			workerclient.MachineCertFile, workerclient.MachineKeyFile, workerclient.ClientCAFile)
	}

	stateDir := o.StateDir
	if stateDir == "" {
		var err error
		if stateDir, err = defaultWorkerStateDir(); err != nil {
			return nil, err
		}
	}
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("resolving --state-dir: %w", err)
	}

	binary, err := workerClientPath()
	if err != nil {
		return nil, err
	}

	args := []string{"-listen", o.Listen, "-proxy-url", o.ProxyURL, "-state-dir", stateDir}
	if o.WorkerName != "" {
		args = append(args, "-worker-id", o.WorkerName)
	}
	if o.AgentEnvFile != "" {
		abs, err := filepath.Abs(o.AgentEnvFile)
		if err != nil {
			return nil, fmt.Errorf("resolving --agent-env-file: %w", err)
		}
		if _, err := os.Stat(abs); err != nil {
			return nil, fmt.Errorf("--agent-env-file %s: %w (the worker reads it at startup; create it 0600)", abs, err)
		}
		args = append(args, "-agent-env-file", abs)
	}
	if o.TLSDir != "" {
		abs, err := filepath.Abs(o.TLSDir)
		if err != nil {
			return nil, fmt.Errorf("resolving --tls-dir: %w", err)
		}
		// Exactly the names enrollment wrote, checked now: a typo'd path would
		// otherwise surface as a TLS handshake failure on the orchestrator's
		// first provision, far from its cause.
		cert := filepath.Join(abs, workerclient.MachineCertFile)
		key := filepath.Join(abs, workerclient.MachineKeyFile)
		clientCA := filepath.Join(abs, workerclient.ClientCAFile)
		for _, f := range []string{cert, key, clientCA} {
			if _, err := os.Stat(f); err != nil {
				return nil, fmt.Errorf("--tls-dir %s: %w (enroll this machine first: gt-worker-client enroll -tls-dir %s …)", abs, err, abs)
			}
		}
		args = append(args, "-tls-cert", cert, "-tls-key", key, "-tls-client-ca", clientCA)
	}
	if o.ExecModes != "" {
		args = append(args, "-exec-modes", o.ExecModes)
		if strings.Contains(o.ExecModes, "container") {
			args = append(args, "-docker")
		}
	}
	if o.MaxSessions > 0 {
		args = append(args, "-max-sessions", fmt.Sprint(o.MaxSessions))
	}
	if o.GTDir != "" {
		args = append(args, "-gt-dir", o.GTDir)
	}

	// The agent's `gt` and `bd` are gt-proxy-client under those names — that is
	// the ONLY way a remote agent reaches the control plane (docs/proxy-server.md).
	// Resolve it now so a missing client is an install-time error, not an agent
	// that starts fine and then cannot call `gt done`.
	proxyClient, err := proxyClientPath()
	if err != nil {
		return nil, err
	}

	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = config.ShellQuote(a)
	}
	shimDir := filepath.Join(stateDir, "bin")
	// The shim dir goes FIRST: the agent inherits this PATH, and on a machine
	// that is both orchestrator and worker the real `gt` is also installed — the
	// agent must get the proxy shim, while the operator's own shell keeps the
	// real binary.
	jobPath := shimDir + ":" + workerServicePath(binary)
	envFile := filepath.Join(stateDir, "worker.env")
	logPath := filepath.Join(stateDir, "worker.log")

	// Assemble the sh -c line here, where the parts are known and can be quoted
	// individually. The binary path and state dir are operator-supplied, so they
	// get the same treatment as the arguments: a path with a quote or $(…) in it
	// would otherwise run as code at job start.
	shellCommand := fmt.Sprintf("set -a; [ -r %s ] && . %s; set +a; exec %s %s",
		config.ShellQuote(envFile), config.ShellQuote(envFile),
		config.ShellQuote(binary), strings.Join(quoted, " "))

	plist, err := templates.RenderWorkerLaunchd(templates.WorkerSupervisorData{
		ShellCommand: shellCommand,
		StateDir:     stateDir,
		LogPath:      logPath,
		Path:         jobPath,
	})
	if err != nil {
		return nil, err
	}
	plistPath, err := templates.WorkerLaunchdPlistPath()
	if err != nil {
		return nil, err
	}
	return &workerServicePlan{
		Binary: binary, StateDir: stateDir, PlistPath: plistPath,
		Args: quoted, Path: jobPath, Plist: plist,
		ShimDir: shimDir, ProxyClient: proxyClient,
	}, nil
}

// installShims points `gt` and `bd` at gt-proxy-client in the worker's own bin
// dir — NOT in the gt install dir, which on a single-box setup holds the real
// gt and must keep holding it.
//
// Symlinks rather than copies, so a `make install` upgrade of gt-proxy-client
// carries to the shims automatically; keeping the client in step with the
// orchestrator's proxy is a version-coupling problem, and a stale copy here
// would present as an agent that mysteriously stops being able to call gt.
// (Fleet-wide freshness is push_binaries' job — design §11 phase 4.)
func installShims(plan *workerServicePlan) error {
	if err := os.MkdirAll(plan.ShimDir, 0755); err != nil {
		return fmt.Errorf("creating shim dir: %w", err)
	}
	for _, name := range []string{"gt", "bd"} {
		link := filepath.Join(plan.ShimDir, name)
		// Replace whatever is there: a stale shim pointing at an old path is
		// worse than no shim, because it fails at agent runtime.
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("replacing %s: %w", link, err)
		}
		if err := os.Symlink(plan.ProxyClient, link); err != nil {
			return fmt.Errorf("linking %s -> %s: %w", link, plan.ProxyClient, err)
		}
	}
	return nil
}

// proxyClientPath finds gt-proxy-client, preferring the copy installed next to
// the running gt so the worker's shims match this build.
func proxyClientPath() (string, error) {
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), "gt-proxy-client")
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand, nil
		}
	}
	path, err := exec.LookPath("gt-proxy-client")
	if err != nil {
		return "", fmt.Errorf("gt-proxy-client not found next to gt or on PATH — the agent's `gt`/`bd` are that binary under those names, so the worker cannot reach the control plane without it; run `make install` on this machine")
	}
	return path, nil
}

// installWorkerService writes the job and (re)starts it.
func installWorkerService(plan *workerServicePlan) error {
	if err := os.MkdirAll(plan.StateDir, 0700); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	if err := installShims(plan); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plan.PlistPath), 0755); err != nil {
		return fmt.Errorf("creating LaunchAgents dir: %w", err)
	}
	if err := os.WriteFile(plan.PlistPath, plan.Plist, 0644); err != nil {
		return fmt.Errorf("writing %s: %w", plan.PlistPath, err)
	}
	// Replace any previous job; bootout errors when nothing is loaded, which is
	// the normal first-install case.
	_ = exec.Command("launchctl", "bootout", guiDomain()+"/com.gastown.worker").Run()
	if out, err := exec.Command("launchctl", "bootstrap", guiDomain(), plan.PlistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

var workerServiceUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Stop and remove the worker service",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := templates.WorkerLaunchdPlistPath()
		if err != nil {
			return err
		}
		_ = exec.Command("launchctl", "bootout", guiDomain()+"/com.gastown.worker").Run()
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %s: %w", path, err)
		}
		fmt.Println("Removed com.gastown.worker (session state and logs are left in place)")
		return nil
	},
}

var workerServiceRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the worker service",
	Long: `Restart the worker service.

Live sessions do not survive: the orchestrator reports them orphaned and
re-provisions. Prefer restarting when no polecat is attached.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		out, err := exec.Command("launchctl", "kickstart", "-k", guiDomain()+"/com.gastown.worker").CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl kickstart: %s", strings.TrimSpace(string(out)))
		}
		fmt.Println("Restarted com.gastown.worker")
		return nil
	},
}

var workerServiceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the worker service's launchd state",
	RunE: func(cmd *cobra.Command, args []string) error {
		out, err := exec.Command("launchctl", "print", guiDomain()+"/com.gastown.worker").CombinedOutput()
		if err != nil {
			fmt.Println("com.gastown.worker is not loaded")
			return nil
		}
		// launchctl print is verbose; surface the fields an operator acts on.
		for _, line := range strings.Split(string(out), "\n") {
			t := strings.TrimSpace(line)
			for _, want := range []string{"state =", "pid =", "last exit code =", "path =", "runs ="} {
				if strings.HasPrefix(t, want) {
					fmt.Println("  " + t)
				}
			}
		}
		return nil
	},
}

// defaultWorkerStateDir is the macOS-appropriate session state location. The
// binary's own default (/var/lib/gt-worker) is a Linux path that a user-level
// launchd job cannot create.
func defaultWorkerStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", "gt-worker"), nil
}

// workerClientPath finds gt-worker-client, preferring the copy installed
// alongside the running gt so a supervised worker can never be a different
// build than the CLI that configured it.
func workerClientPath() (string, error) {
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), "gt-worker-client")
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand, nil
		}
	}
	path, err := exec.LookPath("gt-worker-client")
	if err != nil {
		return "", fmt.Errorf("gt-worker-client not found next to gt or on PATH — run `make install` on this machine first")
	}
	return path, nil
}

// workerServicePath is the PATH the supervised worker runs with. launchd hands
// a job a bare PATH, but the worker execs git, docker and the agent binary, so
// the usual tool locations plus the gt install dir must be present.
func workerServicePath(binary string) string {
	parts := []string{filepath.Dir(binary)}
	for _, p := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		parts = append(parts, p)
	}
	if home, err := os.UserHomeDir(); err == nil {
		parts = append(parts, filepath.Join(home, ".local", "bin"))
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range parts {
		if p == "" || p == "." || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return strings.Join(out, ":")
}

// guiDomain is the per-user launchd domain for this uid.
func guiDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func init() {
	f := workerServiceInstallCmd.Flags()
	f.String("listen", "", "unix:///path/to.sock or TCP host:port (required)")
	f.String("proxy-url", "", "orchestrator proxy base URL reachable from THIS machine (required)")
	f.String("state-dir", "", "session state dir (default ~/Library/Application Support/gt-worker)")
	f.String("worker-name", "", "this machine's enrolled name, reported in the handshake")
	f.String("agent-env-file", "", "KEY=VALUE file supplying agent credentials worker-side (§8)")
	f.String("tls-dir", "", "enrollment output dir holding worker.crt/worker.key + the worker CA (TCP mTLS)")
	f.String("exec-modes", "native", "comma-separated exec modes to advertise (native,container)")
	f.Int("max-sessions", 1, "concurrent session cap")
	f.String("gt-dir", "", "injected gastown bits dir for container mode")

	workerServiceCmd.AddCommand(workerServiceInstallCmd, workerServiceUninstallCmd,
		workerServiceRestartCmd, workerServiceStatusCmd)
	workerCmd.AddCommand(workerServiceCmd)
}
