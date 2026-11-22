package cmd

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/workspace"
)

// gt proxy service — supervise gt-proxy-server on the orchestrator.
//
// A remote polecat's gt and bd are proxy clients (docs/proxy-server.md): while
// the proxy is down, every control-plane call a remote agent makes fails. The
// worker side has `gt worker service`; this is the orchestrator's half.
var proxyCmd = &cobra.Command{
	Use:     "proxy",
	GroupID: GroupServices,
	Short:   "Manage the control-plane proxy for remote polecats",
	RunE:    requireSubcommand,
}

var proxyServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Run gt-proxy-server as a supervised background service",
	RunE:  requireSubcommand,
}

const proxyServiceLabel = "com.gastown.proxy"

var proxyServiceInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install and start the proxy service (macOS launchd)",
	Long: `Install a launchd job that keeps gt-proxy-server running on this machine.

Run this on the ORCHESTRATOR, from inside the town (or pass --town-root). The
job runs the installed gt-proxy-server in place, from the town root, with a
PATH captured from this shell — the same way the daemon's job gets its PATH —
so the gt and bd commands it runs for remote polecats resolve the same tools
the town's own sessions do. Nothing else is put in its environment.

Workers must be able to reach the listen address, and the proxy certificate
must carry the name or IP they dial: set extra_san_hosts / extra_san_ips in
the config file (default <town>/.runtime/proxy/config.json) BEFORE installing,
because the certificate is issued at startup. After changing the config:

  gt proxy service restart

Example:

  gt proxy service install --listen 0.0.0.0:9876`,
	RunE: func(cmd *cobra.Command, args []string) error {
		o := proxyServiceOpts{}
		o.Listen, _ = cmd.Flags().GetString("listen")
		o.Config, _ = cmd.Flags().GetString("config")
		o.TownRoot, _ = cmd.Flags().GetString("town-root")

		plan, err := planProxyService(o)
		if err != nil {
			return err
		}
		if err := installProxyService(plan); err != nil {
			return err
		}
		fmt.Printf("Installed and started %s\n  plist:  %s\n  binary: %s\n  town:   %s\n  log:    %s\n",
			proxyServiceLabel, plan.PlistPath, plan.Binary, plan.TownRoot, plan.LogPath)
		return nil
	},
}

// proxyServiceOpts is the operator's requested proxy configuration.
type proxyServiceOpts struct {
	Listen   string
	Config   string
	TownRoot string
}

// proxyServicePlan is a validated, fully-resolved job description, separate
// from the install so the validation is testable without touching launchd.
type proxyServicePlan struct {
	Binary    string
	TownRoot  string
	LogPath   string
	PlistPath string
	Args      []string
	Path      string
	Plist     []byte
}

// planProxyService validates opts and renders the job.
//
// Everything that can be wrong is caught here: a job with a bad config or town
// root loads, exits, and is restarted by launchd forever, and the operator
// finds out from a worker that cannot reach the control plane.
func planProxyService(o proxyServiceOpts) (*proxyServicePlan, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("gt proxy service currently supports macOS/launchd only; on Linux run gt-proxy-server under systemd (see docs/proxy-server.md)")
	}

	townRoot := o.TownRoot
	if townRoot == "" {
		var err error
		if townRoot, err = workspace.FindFromCwdOrError(); err != nil {
			return nil, fmt.Errorf("finding the town root (run from inside the town, or pass --town-root): %w", err)
		}
	}
	townRoot, err := filepath.Abs(townRoot)
	if err != nil {
		return nil, fmt.Errorf("resolving --town-root: %w", err)
	}
	if fi, err := os.Stat(townRoot); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("--town-root %s is not a directory", townRoot)
	}

	binary, err := proxyServerPath()
	if err != nil {
		return nil, err
	}

	// -town-root is always explicit: the server's own default is $GT_TOWN or
	// ~/gt, and a launchd job has neither this shell's environment nor any
	// reason to guess.
	args := []string{binary, "-town-root", townRoot}

	if o.Listen != "" {
		if _, _, err := net.SplitHostPort(o.Listen); err != nil {
			return nil, fmt.Errorf("--listen %q: want host:port: %w", o.Listen, err)
		}
		args = append(args, "-listen", o.Listen)
	}

	// The config path is always explicit too, for the same reason as the town
	// root. A missing file is fine — the server treats it as empty — but one
	// that is there and does not parse would make the job exit on every start.
	cfg := o.Config
	if cfg == "" {
		cfg = filepath.Join(townRoot, ".runtime", "proxy", "config.json")
	} else if cfg, err = filepath.Abs(cfg); err != nil {
		return nil, fmt.Errorf("resolving --config: %w", err)
	}
	switch data, err := os.ReadFile(cfg); {
	case err == nil:
		if !json.Valid(data) {
			return nil, fmt.Errorf("--config %s is not valid JSON", cfg)
		}
	case os.IsNotExist(err):
		if o.Config != "" {
			return nil, fmt.Errorf("--config %s: %w", cfg, err)
		}
	default:
		return nil, fmt.Errorf("--config %s: %w", cfg, err)
	}
	args = append(args, "-config", cfg)

	logPath := filepath.Join(townRoot, ".runtime", "proxy", "proxy-server.log")
	jobPath := templates.ProxyServicePath(binary)

	plist, err := templates.RenderProxyLaunchd(templates.ProxySupervisorData{
		Args:     args,
		TownRoot: townRoot,
		LogPath:  logPath,
		Path:     jobPath,
	})
	if err != nil {
		return nil, err
	}
	plistPath, err := templates.ProxyLaunchdPlistPath()
	if err != nil {
		return nil, err
	}
	return &proxyServicePlan{
		Binary: binary, TownRoot: townRoot, LogPath: logPath,
		PlistPath: plistPath, Args: args, Path: jobPath, Plist: plist,
	}, nil
}

// proxyServerPath finds gt-proxy-server, preferring the copy installed next to
// the running gt so the supervised proxy is the same build as this CLI.
//
// The job runs that binary where it is. Unlike the worker, nothing replaces it
// except `make install`, which signs it in place — a second copy would be a
// second file to keep signed, for no gain.
func proxyServerPath() (string, error) {
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), "gt-proxy-server")
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand, nil
		}
	}
	path, err := exec.LookPath("gt-proxy-server")
	if err != nil {
		return "", fmt.Errorf("gt-proxy-server not found next to gt or on PATH — run `make install` on this machine first")
	}
	return filepath.Abs(path)
}

// installProxyService writes the job and (re)starts it.
func installProxyService(plan *proxyServicePlan) error {
	if err := os.MkdirAll(filepath.Dir(plan.LogPath), 0700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(plan.LogPath), err)
	}
	if err := os.MkdirAll(filepath.Dir(plan.PlistPath), 0755); err != nil {
		return fmt.Errorf("creating LaunchAgents dir: %w", err)
	}
	if err := os.WriteFile(plan.PlistPath, plan.Plist, 0644); err != nil {
		return fmt.Errorf("writing %s: %w", plan.PlistPath, err)
	}
	// Replace any previous job; bootout errors when nothing is loaded, which is
	// the normal first-install case.
	_ = exec.Command("launchctl", "bootout", guiDomain()+"/"+proxyServiceLabel).Run()
	if out, err := exec.Command("launchctl", "bootstrap", guiDomain(), plan.PlistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

var proxyServiceUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Stop and remove the proxy service",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := templates.ProxyLaunchdPlistPath()
		if err != nil {
			return err
		}
		_ = exec.Command("launchctl", "bootout", guiDomain()+"/"+proxyServiceLabel).Run()
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %s: %w", path, err)
		}
		fmt.Printf("Removed %s (the CA, config and logs are left in place)\n", proxyServiceLabel)
		return nil
	},
}

var proxyServiceRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the proxy service",
	Long: `Restart the proxy service.

Needed after changing the config file: the server certificate, with its
extra_san_hosts / extra_san_ips, is issued at startup. Remote polecats' gt
and bd calls fail for the moment the proxy is down.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		out, err := exec.Command("launchctl", "kickstart", "-k", guiDomain()+"/"+proxyServiceLabel).CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl kickstart: %s", strings.TrimSpace(string(out)))
		}
		fmt.Printf("Restarted %s\n", proxyServiceLabel)
		return nil
	},
}

var proxyServiceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the proxy service's launchd state",
	RunE: func(cmd *cobra.Command, args []string) error {
		out, err := exec.Command("launchctl", "print", guiDomain()+"/"+proxyServiceLabel).CombinedOutput()
		if err != nil {
			fmt.Printf("%s is not loaded\n", proxyServiceLabel)
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

func init() {
	f := proxyServiceInstallCmd.Flags()
	f.String("listen", "", "host:port for workers to reach (default: the config file's listen_addr, else 0.0.0.0:9876)")
	f.String("config", "", "proxy config file (default <town-root>/.runtime/proxy/config.json)")
	f.String("town-root", "", "Gas Town root directory (default: the town containing the current directory)")

	proxyServiceCmd.AddCommand(proxyServiceInstallCmd, proxyServiceUninstallCmd,
		proxyServiceRestartCmd, proxyServiceStatusCmd)
	proxyCmd.AddCommand(proxyServiceCmd)
	rootCmd.AddCommand(proxyCmd)
}
