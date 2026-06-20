package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/workspace"
)

var pressureCheckJSON bool

var pressureCheckCmd = &cobra.Command{
	Use:     "pressure-check",
	GroupID: GroupServices,
	Short:   "Check system pressure (CPU, memory, session cap)",
	Long: `Evaluate system pressure using the same deterministic, per-core check
that the daemon uses before spawning agent sessions.

Exit codes:
  0 = OK (system is within configured limits, or all checks disabled)
  1 = BLOCKED (system pressure exceeds a configured threshold)

CPU check uses LOAD-PER-CORE, not raw load average. A raw 1-min load of
30 on an 8-core machine is 3.75/core — well within normal for I/O-bound
workloads (each Claude API call counts as a waiting process).

Thresholds are read from the town's operational settings
(operational.daemon.pressure_cpu_threshold etc). All checks are DISABLED
by default (0 = off). When disabled, the command always exits 0.

Use this command to perform a codified, documented pressure check instead
of improvising your own uptime/sysctl heuristics.

Examples:
  gt pressure-check          # Check and print result
  gt pressure-check --json   # Machine-readable output`,
	RunE: runPressureCheck,
}

func init() {
	pressureCheckCmd.Flags().BoolVar(&pressureCheckJSON, "json", false, "Output as JSON")
	rootCmd.AddCommand(pressureCheckCmd)
}

func runPressureCheck(_ *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		// Outside a town workspace: all checks disabled — report OK.
		return printPressureResult(daemon.PressureResult{OK: true}, false, pressureCheckJSON)
	}

	opCfg := config.LoadOperationalConfig(townRoot)
	daemonCfg := opCfg.GetDaemonConfig()

	cfg := daemon.PressureConfig{
		CPUThreshold:   daemonCfg.PressureCPUThresholdV(),
		MemThresholdGB: daemonCfg.PressureMemThresholdGBV(),
		MaxSessions:    daemonCfg.PressureMaxSessionsV(),
	}

	allDisabled := cfg.CPUThreshold <= 0 && cfg.MemThresholdGB <= 0 && cfg.MaxSessions <= 0
	result := daemon.CheckPressureConfig(cfg)

	if err := printPressureResult(result, allDisabled, pressureCheckJSON); err != nil {
		return err
	}

	if !result.OK {
		return NewSilentExit(1)
	}
	return nil
}

func printPressureResult(result daemon.PressureResult, allDisabled bool, asJSON bool) error {
	if asJSON {
		type jsonResult struct {
			OK             bool    `json:"ok"`
			Reason         string  `json:"reason,omitempty"`
			LoadAvg1       float64 `json:"load_avg_1"`
			LoadPerCore    float64 `json:"load_per_core"`
			NumCPU         int     `json:"num_cpu"`
			MemAvailableGB float64 `json:"mem_available_gb"`
			ActiveSessions int     `json:"active_sessions"`
		}
		numCPU := runtime.NumCPU()
		var loadPerCore float64
		if numCPU > 0 {
			loadPerCore = result.LoadAvg1 / float64(numCPU)
		}
		out := jsonResult{
			OK:             result.OK,
			Reason:         result.Reason,
			LoadAvg1:       result.LoadAvg1,
			LoadPerCore:    loadPerCore,
			NumCPU:         numCPU,
			MemAvailableGB: result.MemAvailableGB,
			ActiveSessions: result.ActiveSessions,
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}

	if result.OK {
		if allDisabled {
			fmt.Println("OK: all pressure checks disabled (configure thresholds in operational settings to enable)")
		} else {
			fmt.Println("OK: system pressure within configured limits")
		}
	} else {
		fmt.Printf("BLOCKED: %s\n", result.Reason)
	}
	return nil
}
