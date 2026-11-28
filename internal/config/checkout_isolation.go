package config

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
)

// The reviewer runs with its working directory inside a checkout of the PR
// under review, so the PR's author controls the project files there. Claude
// Code runs hooks, applies env and starts MCP servers from those files at
// session start, with the session's credentials in scope. These launch flags
// and the check below keep all of that from being loaded.
//
// They do not cover CLAUDE.md or anything else the agent reads as text.

// CodegraphMCPArg is the gt subcommand that serves the code graph to a session
// in a PR checkout.
const CodegraphMCPArg = "codegraph-mcp"

var gtExecutable = os.Executable

// checkoutMCPConfig is the reviewer's own MCP configuration: the code graph
// server the review passes use. It is started through this gt binary, which
// runs codegraph from an absolute path; a bare name would be looked up from the
// checkout, where a version-manager shim reads the PR author's configuration.
func checkoutMCPConfig() string {
	gt, err := gtExecutable()
	if err != nil {
		gt = "" // refused by verifyCheckoutIsolation
	}
	data, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"codegraph": map[string]any{
		"type": "stdio", "command": gt, "args": []string{"reviewer", CodegraphMCPArg},
	}}})
	return string(data)
}

// pinCheckoutTools keeps the checkout from choosing which programs a session in
// it runs, or their versions: the agent binary becomes an absolute path, and
// mise is told to ignore configuration files in the checkout.
func pinCheckoutTools(rc *RuntimeConfig, env map[string]string, checkout string) error {
	if !strings.ContainsRune(rc.Command, '/') && !filepath.IsAbs(rc.Command) {
		path, err := exec.LookPath(rc.Command)
		if err != nil || !filepath.IsAbs(path) {
			return fmt.Errorf("refusing to start in a PR checkout: no absolute path for %q: %v", rc.Command, err)
		}
		rc.Command = path
	}
	const ignored = "MISE_IGNORED_CONFIG_PATHS"
	if inherited := os.Getenv(ignored); env[ignored] == "" && inherited != "" {
		env[ignored] = inherited
	}
	if env[ignored] != "" {
		env[ignored] += string(os.PathListSeparator)
	}
	env[ignored] += checkout
	return nil
}

// runsInPRCheckout reports whether sessions of role start inside a PR checkout.
func runsInPRCheckout(role string) bool {
	return role == constants.RoleReviewer
}

// addCheckoutIsolationFlags appends the isolation flags the agent's own
// arguments do not already set. A conflicting value the agent does set is left
// in place for verifyCheckoutIsolation to refuse.
func addCheckoutIsolationFlags(args []string, settingsFile string) []string {
	args = slices.Clip(args)
	if len(flagValues(args, "--settings")) == 0 {
		args = append(args, "--settings", settingsFile)
	}
	if len(flagValues(args, "--setting-sources")) == 0 {
		args = append(args, "--setting-sources", "user")
	}
	own := checkoutMCPConfig()
	if slices.Contains(flagValues(args, "--mcp-config"), own) {
		return args
	}
	// An operator's --mcp-config adds servers; ours is always there too.
	// --mcp-config takes several values, so a flag must follow it or it would
	// swallow the positional prompt.
	if i := slices.Index(args, "--strict-mcp-config"); i >= 0 {
		return slices.Insert(args, i, "--mcp-config", own)
	}
	return append(args, "--mcp-config", own, "--strict-mcp-config")
}

// verifyCheckoutIsolation returns an error unless a session started from rc
// loads no settings, hooks, plugins or MCP servers from its working directory.
// settingsFile is the only --settings value accepted: it carries the role's
// tool guards.
func verifyCheckoutIsolation(rc *RuntimeConfig, env map[string]string, settingsFile string) error {
	refuse := func(format string, a ...any) error {
		return fmt.Errorf("refusing to start in a PR checkout: "+format, a...)
	}
	if !isClaudeAgent(rc) {
		return refuse("agent %q is not Claude Code, so its project configuration cannot be excluded", rc.Command)
	}
	// A relative path resolves inside the checkout.
	for _, program := range append([]string{rc.Command}, rc.ExecWrapper...) {
		if strings.ContainsRune(program, '/') && !filepath.IsAbs(program) {
			return refuse("command %q is a relative path", program)
		}
	}
	if dir := env["CLAUDE_CONFIG_DIR"]; dir != "" && !filepath.IsAbs(dir) {
		return refuse("CLAUDE_CONFIG_DIR %q is a relative path", dir)
	}
	if slices.Contains(rc.Args, "--") {
		return refuse(`the agent arguments contain "--", which turns the isolation flags into prompt text`)
	}
	for _, value := range flagValues(rc.Args, "--setting-sources") {
		for _, source := range strings.Split(value, ",") {
			if source = strings.TrimSpace(source); source != "user" {
				return refuse("setting source %q is allowed by the agent arguments; only \"user\" is", source)
			}
		}
	}
	if !filepath.IsAbs(settingsFile) {
		return refuse("the settings file %q is a relative path", settingsFile)
	}
	if got := flagValues(rc.Args, "--settings"); !slices.Equal(got, []string{settingsFile}) {
		return refuse("--settings must be %s only, got %q", settingsFile, got)
	}
	if len(flagValues(rc.Args, "--setting-sources")) == 0 || !slices.Contains(rc.Args, "--strict-mcp-config") {
		return refuse("--setting-sources or --strict-mcp-config is missing")
	}
	for _, flag := range []string{"--mcp-config", "--plugin-dir"} {
		for _, value := range flagValues(rc.Args, flag) {
			if !strings.HasPrefix(value, "{") && !filepath.IsAbs(value) {
				return refuse("%s %q is a relative path", flag, value)
			}
		}
	}
	return nil
}

// flagValues returns every value args give flag, in the "--flag value..." and
// "--flag=value" forms.
func flagValues(args []string, flag string) []string {
	var values []string
	for i := 0; i < len(args); i++ {
		if value, ok := strings.CutPrefix(args[i], flag+"="); ok {
			values = append(values, value)
			continue
		}
		if args[i] != flag {
			continue
		}
		for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			values = append(values, args[i])
		}
	}
	return values
}
