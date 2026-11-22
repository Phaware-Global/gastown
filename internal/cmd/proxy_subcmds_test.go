package cmd

import "testing"

// A remote polecat's gt is a proxy shim, so every gt command its hooks and
// role template call must be on the proxy allowlist or it fails with 403.
func TestProxySubcmds_CoverPolecatHookAndTemplateCommands(t *testing.T) {
	allowed := map[string]bool{}
	for _, c := range rootCmd.Commands() {
		if c.Annotations[AnnotationPolecatSafe] == "true" {
			allowed[c.Name()] = true
		}
	}
	// tap: PreToolUse guards and the Stop reminder in the polecat settings.
	// escalate: the polecat role template's stuck/blocked path.
	for _, name := range []string{"prime", "mail", "done", "hook", "handoff", "mol", "tap", "escalate"} {
		if !allowed[name] {
			t.Errorf("gt %s is not polecat-safe, so a remote polecat cannot run it", name)
		}
	}
}
