package tmux

import (
	"runtime"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// scrubSecrets prefixes a POSIX pane command so that it starts without the
// secret variables its role may not hold. The tmux server hands every pane its
// own environment, so the removal has to happen inside the pane. It runs before
// the command, which keeps variables the command itself sets.
//
// Only variable names are read. If they cannot be listed, the pane exits rather
// than start with secrets in scope.
func scrubSecrets(gtRole, townRoot, command string) string {
	if command == "" || runtime.GOOS == "windows" {
		return command
	}
	var allowed []string
	for _, name := range config.SessionSecretAllowlist(gtRole, townRoot) {
		if isEnvName(name) {
			allowed = append(allowed, name)
		}
	}
	suffixes := strings.Join(config.SecretEnvSuffixes, "|")
	program := `BEGIN{for(k in ENVIRON) if(toupper(k) ~ /(` + suffixes + `)$/ && k ~ /^[A-Za-z_][A-Za-z0-9_]*$/ && index(" ` +
		strings.Join(allowed, " ") + ` ", " " k " ")==0) print "unset " k}`
	return `gt_unset=$(awk ` + config.ShellQuote(program) + `) || exit 1; eval "$gt_unset"; unset gt_unset; ` + command
}

func isEnvName(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool {
		return r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) < 0
}

// scrubForTarget scrubs a respawn command using the role recorded in the
// environment of the session that owns the pane.
func (t *Tmux) scrubForTarget(target, command string) string {
	session, err := t.run("display-message", "-p", "-t", target, "#{session_name}")
	var gtRole, townRoot string
	if err == nil {
		session = strings.TrimSpace(session)
		gtRole, _ = t.GetEnvironment(session, "GT_ROLE")
		townRoot, _ = t.GetEnvironment(session, "GT_ROOT")
	}
	return scrubSecrets(gtRole, townRoot, command)
}
