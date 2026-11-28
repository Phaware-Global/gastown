package config

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Resolved at package init: TestMain later puts a stub claude first on PATH.
var realClaudeBin, errNoClaude = exec.LookPath("claude")

// reviewerTestRig returns a rig whose reviewer role resolves to the given agent.
func reviewerTestRig(t *testing.T, agent *RuntimeConfig) (townRoot, rigPath string) {
	t.Helper()
	townRoot = t.TempDir()
	rigPath = filepath.Join(townRoot, "myrig")
	town := NewTownSettings()
	if agent != nil {
		town.Agents = map[string]*RuntimeConfig{"under-test": agent}
		town.RoleAgents = map[string]string{"reviewer": "under-test"}
	}
	if err := SaveTownSettings(TownSettingsPath(townRoot), town); err != nil {
		t.Fatal(err)
	}
	if err := SaveRigSettings(RigSettingsPath(rigPath), NewRigSettings()); err != nil {
		t.Fatal(err)
	}
	return townRoot, rigPath
}

func flagValue(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func TestReviewerLaunchArgs(t *testing.T) {
	claude := func(args ...string) *RuntimeConfig {
		return &RuntimeConfig{Provider: "claude", Command: "claude", Args: args}
	}
	tests := []struct {
		name    string
		agent   *RuntimeConfig
		wantMCP []string // besides the reviewer's own
	}{
		{"default agent", nil, nil},
		{"custom claude agent", claude("--dangerously-skip-permissions", "--model", "opus"), nil},
		{"agent with its own strict MCP config", claude("--strict-mcp-config", "--mcp-config", "/etc/mcp.json"), []string{"/etc/mcp.json"}},
		{"agent with a non-strict MCP config", claude("--mcp-config", "/etc/mcp.json"), []string{"/etc/mcp.json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			townRoot, rigPath := reviewerTestRig(t, tt.agent)
			settings := filepath.Join(rigPath, "reviewer", ".claude", "settings.json")

			rc := ResolveRoleAgentConfig("reviewer", townRoot, rigPath)
			if got := flagValue(rc.Args, "--settings"); got != settings {
				t.Errorf("--settings = %q, want %q", got, settings)
			}
			if got := flagValue(rc.Args, "--setting-sources"); got != "user" {
				t.Errorf("--setting-sources = %q, want user", got)
			}
			want := append(slices.Clone(tt.wantMCP), checkoutMCPConfig())
			got := flagValues(rc.Args, "--mcp-config")
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("--mcp-config = %q, want %q", got, want)
			}
			if !slices.Contains(rc.Args, "--strict-mcp-config") {
				t.Errorf("missing --strict-mcp-config: %q", rc.Args)
			}

			cmd, err := BuildStartupCommandWithAgentOverride(
				map[string]string{"GT_ROLE": "myrig/reviewer"}, rigPath, "the prompt", "")
			if err != nil {
				t.Fatal(err)
			}
			for _, flag := range []string{"--settings ", "--setting-sources user", "--strict-mcp-config"} {
				if n := strings.Count(cmd, flag); n != 1 {
					t.Errorf("%q appears %d times in %s", flag, n, cmd)
				}
			}
		})
	}
}

// Only the reviewer is isolated; every other role keeps its project settings.
func TestOtherRolesKeepProjectSettings(t *testing.T) {
	townRoot, rigPath := reviewerTestRig(t, nil)
	for _, role := range []string{"polecat", "crew", "witness", "refinery"} {
		rc := ResolveRoleAgentConfig(role, townRoot, rigPath)
		if slices.Contains(rc.Args, "--setting-sources") || slices.Contains(rc.Args, "--strict-mcp-config") {
			t.Errorf("%s: unexpected isolation flags %q", role, rc.Args)
		}
	}
}

func TestReviewerStartRefused(t *testing.T) {
	claude := func(args ...string) *RuntimeConfig {
		return &RuntimeConfig{Provider: "claude", Command: "claude", Args: args}
	}
	tests := []struct {
		name  string
		agent *RuntimeConfig
		want  string
	}{
		{"project source", claude("--setting-sources", "user,project"), "setting source"},
		{"local source, = form", claude("--setting-sources=local"), "setting source"},
		{"own settings file", claude("--settings", ".claude/settings.json"), "--settings"},
		{"own settings, = form", claude("--settings=.claude/settings.json"), "--settings"},
		{"relative MCP config", claude("--strict-mcp-config", "--mcp-config", ".mcp.json"), "--mcp-config"},
		{"second relative MCP config", claude("--mcp-config", "/etc/a.json", ".mcp.json"), "--mcp-config"},
		{"relative plugin dir", claude("--plugin-dir", "plugins"), "--plugin-dir"},
		{"end of flags", claude("--"), `"--"`},
		{"relative binary", &RuntimeConfig{Provider: "claude", Command: "./claude"}, "command"},
		{"relative config dir", &RuntimeConfig{Provider: "claude", Command: "claude", Env: map[string]string{"CLAUDE_CONFIG_DIR": ".claude"}}, "CLAUDE_CONFIG_DIR"},
		{"agent not on PATH", &RuntimeConfig{Provider: "claude", Command: "no-such-claude"}, "absolute path"},
		{"not claude", &RuntimeConfig{Provider: "codex", Command: "codex"}, "not Claude Code"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, rigPath := reviewerTestRig(t, tt.agent)
			cmd, err := BuildStartupCommandWithAgentOverride(
				map[string]string{"GT_ROLE": "myrig/reviewer"}, rigPath, "p", "")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one naming %s (command %q)", err, tt.want, cmd)
			}
		})
	}
}

// The reviewer settings file is the one --settings value accepted, so a
// relative one would be read from the checkout.
func TestVerifyCheckoutIsolationSettingsPath(t *testing.T) {
	for _, tt := range []struct {
		settings string
		refused  bool
	}{
		{filepath.Join(t.TempDir(), "settings.json"), false},
		{"myrig/reviewer/.claude/settings.json", true},
		{".claude/settings.json", true},
		{"", true},
	} {
		rc := &RuntimeConfig{Provider: "claude", Command: filepath.Join(t.TempDir(), "claude")}
		rc.Args = addCheckoutIsolationFlags(nil, tt.settings)
		err := verifyCheckoutIsolation(rc, nil, tt.settings)
		if refused := err != nil; refused != tt.refused {
			t.Errorf("settings %q: err = %v, want refused = %v", tt.settings, err, tt.refused)
		}
	}
}

// A bare tool name is looked up from the checkout, where a version-manager shim
// reads the PR author's configuration.
func TestReviewerToolsPinned(t *testing.T) {
	var cfg struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
		}
	}
	if err := json.Unmarshal([]byte(checkoutMCPConfig()), &cfg); err != nil {
		t.Fatal(err)
	}
	cg, ok := cfg.MCPServers["codegraph"]
	if !ok || len(cfg.MCPServers) != 1 || !filepath.IsAbs(cg.Command) {
		t.Errorf("MCP config = %s, want codegraph alone with an absolute command", checkoutMCPConfig())
	}

	townRoot, rigPath := reviewerTestRig(t, nil)
	checkout := filepath.Join(rigPath, "reviewer", "rig")
	// Respawns take their command and env from the resolved config alone.
	t.Setenv("MISE_IGNORED_CONFIG_PATHS", checkout)
	rc := ResolveRoleAgentConfig("reviewer", townRoot, rigPath)
	if !filepath.IsAbs(rc.Command) || rc.Env["MISE_IGNORED_CONFIG_PATHS"] != checkout {
		t.Errorf("resolved command %q, mise ignore %q", rc.Command, rc.Env["MISE_IGNORED_CONFIG_PATHS"])
	}
	for _, tt := range []struct{ inherited, want string }{
		{"", checkout},
		{"/etc/mise", "/etc/mise" + string(os.PathListSeparator) + checkout},
	} {
		t.Setenv("MISE_IGNORED_CONFIG_PATHS", tt.inherited)
		cmd, err := BuildStartupCommandWithAgentOverride(
			map[string]string{"GT_ROLE": "myrig/reviewer"}, rigPath, "p", "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(cmd, "MISE_IGNORED_CONFIG_PATHS="+ShellQuote(tt.want)+" ") {
			t.Errorf("inherited %q: want MISE_IGNORED_CONFIG_PATHS=%s in %s", tt.inherited, tt.want, cmd)
		}
		claudeBin, err := exec.LookPath("claude")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(cmd, " "+claudeBin+" ") {
			t.Errorf("agent binary not pinned to %s: %s", claudeBin, cmd)
		}
	}
}

// Runs the real Claude Code binary in a checkout that ships hooks and an MCP
// server, with the launch arguments the reviewer gets. The API address is
// unreachable: settings and MCP servers load before the first request.
func TestReviewerSessionIgnoresCheckoutConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hooks")
	}
	if errNoClaude != nil {
		t.Skip("claude not installed")
	}

	marks, home, bin := t.TempDir(), t.TempDir(), t.TempDir()
	// Stands in for the gt binary that serves the code graph.
	stub := filepath.Join(bin, "gt")
	defer func(orig func() (string, error)) { gtExecutable = orig }(gtExecutable)
	gtExecutable = func() (string, error) { return stub, nil }

	operatorMCP := filepath.Join(t.TempDir(), "mcp.json")
	townRoot, rigPath := reviewerTestRig(t, &RuntimeConfig{
		Provider: "claude", Command: "claude",
		Args: []string{"--strict-mcp-config", "--mcp-config", operatorMCP},
	})
	rc := ResolveRoleAgentConfig("reviewer", townRoot, rigPath)
	checkout := filepath.Join(rigPath, "reviewer", "rig")

	write := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	hook := func(name string) string {
		return `"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"touch '` +
			filepath.Join(marks, name) + `'"}]}]}`
	}
	write(filepath.Join(rigPath, "reviewer", ".claude", "settings.json"), "{"+hook("role")+"}", 0o600)
	write(filepath.Join(checkout, ".claude", "settings.json"), `{"enableAllProjectMcpServers":true,`+hook("project")+"}", 0o600)
	write(filepath.Join(checkout, ".claude", "settings.local.json"), "{"+hook("local")+"}", 0o600)
	write(filepath.Join(checkout, ".mcp.json"), `{"mcpServers":{"from-checkout":{"command":"touch","args":["`+
		filepath.Join(marks, "mcp")+`"]}}}`, 0o600)
	// Stands in for the real codegraph server named in the launch arguments.
	write(stub, "#!/bin/sh\nexit 0\n", 0o755)
	write(operatorMCP, `{"mcpServers":{"from-operator":{"command":"`+stub+`"}}}`, 0o600)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	args := append(slices.Clone(rc.Args), "-p", "x", "--output-format", "stream-json", "--verbose")
	c := exec.CommandContext(ctx, realClaudeBin, args...)
	c.Dir = checkout
	c.WaitDelay = 5 * time.Second
	c.Env = []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + home,
		"CLAUDE_CONFIG_DIR=" + home,
		"ANTHROPIC_API_KEY=sk-not-a-key",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:1",
		"CLAUDE_CODE_MAX_RETRIES=0",
	}
	out, runErr := c.Output() // fails: there is no API to answer

	var servers []string
	initialized := false
	for _, line := range strings.Split(string(out), "\n") {
		var event struct {
			Subtype string `json:"subtype"`
			Servers []struct {
				Name string `json:"name"`
			} `json:"mcp_servers"`
		}
		if json.Unmarshal([]byte(line), &event) != nil || event.Subtype != "init" {
			continue
		}
		initialized = true
		for _, s := range event.Servers {
			servers = append(servers, s.Name)
		}
	}
	if !initialized {
		t.Fatalf("no init event (run error: %v): %s", runErr, out)
	}
	slices.Sort(servers)
	if !slices.Equal(servers, []string{"codegraph", "from-operator"}) {
		t.Errorf("MCP servers = %q, want codegraph and the operator's", servers)
	}
	for name, want := range map[string]bool{"role": true, "project": false, "local": false, "mcp": false} {
		_, err := os.Stat(filepath.Join(marks, name))
		if ran := err == nil; ran != want {
			t.Errorf("%s config ran = %v, want %v", name, ran, want)
		}
	}
}
