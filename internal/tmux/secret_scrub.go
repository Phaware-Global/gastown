package tmux

import (
	"os"
	"runtime"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// Secrets are kept out of the printed global environment of the server. Those
// some role may hold are held there as hidden variables, which no pane inherits,
// and the rest are removed. Each session is given the ones its role may hold in
// its own session environment. This guards against accidental printing only;
// "show-environment -gh" still reads the hidden ones.

// sessionRole returns the GT_ROLE of the named session according to gt's own
// naming. A session's environment is never asked: its agent can write it.
var sessionRole = func(string) (string, bool) { return "", false }

// SetSessionRoleResolver sets how an existing session's role is established. A
// session the resolver does not know is granted no secrets. The session package,
// which owns the naming, registers it; call it only during initialization.
func SetSessionRoleResolver(resolve func(session string) (gtRole string, ok bool)) {
	sessionRole = resolve
}

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
// what this process says, else the server, whose environment any pane can write.
func (t *Tmux) secretTownRoot(hint string) string {
	for _, root := range []string{hint, os.Getenv("GT_TOWN_ROOT"), os.Getenv("GT_ROOT")} {
		if root != "" {
			return root
		}
	}
	root, _ := t.GetGlobalEnvironment("GT_TOWN_ROOT")
	return root
}

// WithholdGlobalSecrets takes every secret out of the printed global environment
// of the server, hiding those some role may hold and removing the rest. Each
// existing session gets, in its own environment, those of the server's secrets
// its role may hold, so that its next respawn still has them. townRoot may be
// empty.
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
	holdable := config.AllSessionSecretNames(townRoot)
	for name, value := range visible {
		if slices.Contains(holdable, name) {
			_, _ = t.run("set-environment", "-g", "-h", name, value)
		} else {
			_, _ = t.run("set-environment", "-g", "-u", name)
		}
	}
	if sessions, err := t.ListSessions(); err == nil {
		for _, session := range sessions {
			if gtRole, ok := sessionRole(session); ok {
				t.grantSecrets(session, gtRole, townRoot, false)
			}
		}
	}
}

// grantSecrets sets, in the session's environment, each secret its role may hold
// that the session does not already have: the one the server holds hidden, so
// that it does not matter who starts the session, else, with fromCaller, this
// process's own. fromCaller is only for a session this process is creating.
func (t *Tmux) grantSecrets(session, gtRole, townRoot string, fromCaller bool) {
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
			value = ""
			if fromCaller {
				value = os.Getenv(name)
			}
		}
		if value != "" {
			_ = t.SetEnvironment(session, name, value)
		}
	}
}

// isolateSecrets withholds the global secrets and gives session, which this
// process has just created, the ones its role may hold. An empty gtRole is
// looked up by the session's name.
func (t *Tmux) isolateSecrets(session, gtRole, townRoot string) {
	if runtime.GOOS == "windows" {
		return
	}
	townRoot = t.secretTownRoot(townRoot)
	if gtRole == "" {
		gtRole, _ = sessionRole(session)
	}
	t.WithholdGlobalSecrets(townRoot)
	t.grantSecrets(session, gtRole, townRoot, true)
}

// scrubForTarget prepares a respawn of the target pane, using the role of the
// session that owns it. An unknown session is granted nothing.
func (t *Tmux) scrubForTarget(target, command string) string {
	townRoot := t.secretTownRoot("")
	var gtRole string
	session, err := t.run("display-message", "-p", "-t", target, "#{session_name}")
	if session = strings.TrimSpace(session); err == nil && session != "" && runtime.GOOS != "windows" {
		t.WithholdGlobalSecrets(townRoot)
		var ok bool
		if gtRole, ok = sessionRole(session); ok {
			t.grantSecrets(session, gtRole, townRoot, false)
		}
	}
	return scrubSecrets(gtRole, townRoot, command)
}
