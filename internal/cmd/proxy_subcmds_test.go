package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

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

// The proxy allows a bd subcommand by its first word only, so a parent command
// grants every child. `bd mol` and `bd formula` have children that write files
// on the proxy host (mol distill --output, formula convert) and hard-delete
// molecules (mol burn), so neither may be on the list. A polecat reads its
// formula steps through gt prime instead.
func TestProxySubcmds_BdListHasNoHostWritingParents(t *testing.T) {
	for _, sub := range strings.Split(bdSafeSubcmds, ",") {
		if sub == "mol" || sub == "formula" {
			t.Errorf("bd %s must not be proxy-allowlisted: it grants every child command", sub)
		}
	}
}

// A proxied caller can raise an escalation but must not settle one.
func TestRefuseProxiedEscalationChange(t *testing.T) {
	t.Setenv("GT_PROXY_IDENTITY", "")
	if err := refuseProxiedEscalationChange("close"); err != nil {
		t.Fatalf("a local caller must be allowed: %v", err)
	}
	t.Setenv("GT_PROXY_IDENTITY", "demo/furiosa")
	for _, action := range []string{"ack", "close", "stale"} {
		if err := refuseProxiedEscalationChange(action); err == nil {
			t.Errorf("gt escalate %s must be refused for a proxied caller", action)
		}
	}
	for _, run := range []func(*cobra.Command, []string) error{runEscalateAck, runEscalateClose, runEscalateStale} {
		if err := run(nil, []string{"hq-x"}); err == nil || !strings.Contains(err.Error(), "through the proxy") {
			t.Errorf("expected the proxy refusal before any other work, got: %v", err)
		}
	}
}
