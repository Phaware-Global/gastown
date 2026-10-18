package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The sweep trigger mail names `gt assistant sweep`; it must be a registered
// subcommand at the head that ships the trigger.
func TestAssistantSweepTriggerNamesRegisteredCommand(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "plugins", "assistant-sweep", "run.sh"))
	if err != nil {
		t.Fatalf("assistant-sweep run.sh must exist: %v", err)
	}
	for _, want := range []string{"gt assistant sweep", "gt assistant sweep --ack <run-id>", "SWEEP: not checked"} {
		if !strings.Contains(string(script), want) {
			t.Errorf("trigger body must mention %q", want)
		}
	}
	// The trigger may defer to a role-prompt protocol only once the template
	// actually carries one.
	if strings.Contains(string(script), "Sweep Protocol") {
		tmpl, err := os.ReadFile(filepath.Join("..", "templates", "roles", "assistant.md.tmpl"))
		if err != nil || !strings.Contains(string(tmpl), "## Sweep Protocol") {
			t.Error("trigger defers to a Sweep Protocol the assistant template does not define")
		}
	}
	found := false
	for _, c := range assistantCmd.Commands() {
		if c.Name() == "sweep" {
			found = true
			for _, flag := range []string{"ack", "dry-run", "json"} {
				if c.Flags().Lookup(flag) == nil {
					t.Errorf("gt assistant sweep must register --%s", flag)
				}
			}
		}
	}
	if !found {
		t.Fatal("gt assistant sweep is not registered but the trigger mail names it")
	}
}
