package tmux

import (
	"os"
	"runtime"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// Secrets are kept out of the printed global environment of the server: they are
// held there as hidden variables, which no pane inherits, and each session is
// given the ones its role may hold in its own session environment. This guards
// against accidental printing only; "show-environment -gh" still reads them.

// scrubSecrets prefixes a POSIX pane command so that it starts without the
// secret variables its role may not hold, however they reached the pane. It runs
// before the command, which keeps variables the command itself sets.
//
// Only variable names are read. If they cannot be listed, the pane exits rather
// than start with secrets in scope.
func scrubSecrets(gtRole, townRoot, command string) string {
	if command == "" || runtime.GOOS == "windows" {
		return command
	}
	allowed := envNames(config.SessionSecretAllowlist(gtRole, townRoot))
	configured := envNames(config.ConfiguredSecretEnvNames(townRoot))
	suffixes := strings.Join(config.SecretEnvSuffixes, "|")
	program := `BEGIN{for(k in ENVIRON) if((toupper(k) ~ /(` + suffixes + `)$/ || index(" ` +
		strings.Join(configured, " ") + ` ", " " k " ")) && k ~ /^[A-Za-z_][A-Za-z0-9_]*$/ && index(" ` +
		strings.Join(allowed, " ") + ` ", " " k " ")==0) print "unset " k}`
	return `gt_unset=$(awk ` + config.ShellQuote(program) + `) || exit 1; eval "$gt_unset"; unset gt_unset; ` + command
}

func envNames(names []string) []string {
	var valid []string
	for _, name := range names {
		if isEnvName(name) {
			valid = append(valid, name)
		}
	}
	return valid
}

func isEnvName(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool {
		return r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) < 0
}

// secretTownRoot returns the town whose config names the secrets: hint, else
// what the server or this process says.
func (t *Tmux) secretTownRoot(hint string) string {
	if hint != "" {
		return hint
	}
	if root, err := t.GetGlobalEnvironment("GT_TOWN_ROOT"); err == nil && root != "" {
		return root
	}
	if root := os.Getenv("GT_TOWN_ROOT"); root != "" {
		return root
	}
	return os.Getenv("GT_ROOT")
}

// WithholdGlobalSecrets hides every secret in the server's global environment.
// Each existing session first gets, in its own environment, the secrets its role
// may hold, so that its next respawn still has them. townRoot may be empty.
// Without a running server there is nothing to do.
//
// Values are read one name at a time and never logged.
func (t *Tmux) WithholdGlobalSecrets(townRoot string) {
	if runtime.GOOS == "windows" {
		return
	}
	listing, err := t.run("show-environment", "-g")
	if err != nil {
		return
	}
	townRoot = t.secretTownRoot(townRoot)
	configured := config.ConfiguredSecretEnvNames(townRoot)
	visible := map[string]string{}
	for _, line := range strings.Split(listing, "\n") {
		name, _, set := strings.Cut(line, "=")
		if !set || !isEnvName(name) || !config.IsSecretEnvName(name, configured) {
			continue
		}
		// A value may span lines, so the listing cannot supply it. This also
		// skips a continuation line that looks like a variable.
		out, err := t.run("show-environment", "-g", name)
		if value, ok := strings.CutPrefix(out, name+"="); err == nil && ok {
			visible[name] = value
		}
	}
	if len(visible) == 0 {
		return
	}
	// Hide before granting: grantSecrets reads the hidden variables.
	for name, value := range visible {
		_, _ = t.run("set-environment", "-g", "-h", name, value)
	}
	if sessions, err := t.ListSessions(); err == nil {
		for _, session := range sessions {
			gtRole, _ := t.GetEnvironment(session, "GT_ROLE")
			t.grantSecrets(session, gtRole, townRoot)
		}
	}
}

// grantSecrets sets, in the session's environment, each secret its role may hold
// that the session does not already have: the one the server holds hidden, so
// that it does not matter who starts the session, else this process's own.
func (t *Tmux) grantSecrets(session, gtRole, townRoot string) {
	configured := config.ConfiguredSecretEnvNames(townRoot)
	for _, name := range envNames(config.SessionSecretAllowlist(gtRole, townRoot)) {
		// The allowlist can name variables that are not secrets by name.
		if !config.IsSecretEnvName(name, configured) {
			continue
		}
		if _, err := t.GetEnvironment(session, name); err == nil {
			continue
		}
		out, err := t.run("show-environment", "-g", "-h", name)
		value, ok := strings.CutPrefix(out, name+"=")
		if err != nil || !ok {
			value = os.Getenv(name)
		}
		if value != "" {
			_ = t.SetEnvironment(session, name, value)
		}
	}
}

// isolateSecrets hides the secrets in the global environment and gives session
// the ones its role may hold.
func (t *Tmux) isolateSecrets(session, gtRole, townRoot string) {
	if runtime.GOOS == "windows" {
		return
	}
	townRoot = t.secretTownRoot(townRoot)
	t.WithholdGlobalSecrets(townRoot)
	t.grantSecrets(session, gtRole, townRoot)
}

// scrubForTarget prepares a respawn of the target pane, using the role recorded
// in the environment of the session that owns it.
func (t *Tmux) scrubForTarget(target, command string) string {
	session, err := t.run("display-message", "-p", "-t", target, "#{session_name}")
	var gtRole, townRoot string
	if session = strings.TrimSpace(session); err == nil && session != "" {
		gtRole, _ = t.GetEnvironment(session, "GT_ROLE")
		townRoot, _ = t.GetEnvironment(session, "GT_ROOT")
		t.isolateSecrets(session, gtRole, townRoot)
	}
	return scrubSecrets(gtRole, t.secretTownRoot(townRoot), command)
}
