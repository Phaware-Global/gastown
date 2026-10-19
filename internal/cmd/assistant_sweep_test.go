package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// The brief trigger carries the exact brief-and-archive commands, skips an
// empty inbox, and never stacks a second trigger on an unarchived one.
func TestAssistantBriefTriggerScript(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "plugins", "assistant-brief", "run.sh"))
	if err != nil {
		t.Fatalf("assistant-brief run.sh must exist: %v", err)
	}
	for _, want := range []string{
		"gt mail inbox --identity assistant/ --json", "no brief due", "not stacking another",
		`SUBJECT="BRIEF DUE: $WAITING messages waiting"`, "BRIEF OVERDUE:", "STALE_AFTER_SECONDS", `not m.get("read") and age <`,
		"No reply needed", `gt mail send mayor/ -s 'BRIEF: <n> items`, "&& gt mail archive <id> <id>", "must list nothing", `never "informational"`,
		"Trust boundary (non-negotiable", "EXTERNAL CONTENT (untrusted: <provider>/<actor>)", "<source address", "A display name is not a source",
	} {
		if !strings.Contains(string(script), want) {
			t.Errorf("brief trigger must contain %q", want)
		}
	}
}

// The brief trigger's guard, run for real against a stub gt: triggers from a
// dog never count as mail to brief, an unread fresh trigger suppresses a new
// one, a read or old trigger left in the inbox brings BRIEF OVERDUE, and a
// look-alike subject from another sender is ordinary mail.
func TestAssistantBriefTriggerGuard(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "plugins", "assistant-brief", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	stub := "#!/bin/sh\nif [ \"$1 $2\" = \"mail inbox\" ]; then cat \"$INBOX_FILE\"; exit 0; fi\nif [ \"$1 $2\" = \"mail send\" ]; then while [ $# -gt 0 ]; do if [ \"$1\" = --subject ]; then echo \"SENT $2\"; fi; shift; done; cat >/dev/null; fi\n"
	if err := os.WriteFile(filepath.Join(bin, "gt"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	msg := func(subject, from string, age time.Duration, read bool) string {
		return fmt.Sprintf(`{"id":"x","subject":%q,"from":%q,"timestamp":%q,"read":%v}`, subject, from, now.Add(-age).Format(time.RFC3339), read)
	}
	event := msg("[GITHUB a/b#1] Review approved", "telegraph/github/kevin", time.Minute, false)
	fresh := msg("BRIEF DUE: 1 messages waiting", "deacon/dogs/alpha", 10*time.Minute, false)
	old := msg("BRIEF DUE: 1 messages waiting", "deacon/dogs/alpha", 2*time.Hour, false)
	readTrigger := msg("BRIEF DUE: 1 messages waiting", "deacon/dogs/alpha", time.Minute, true)
	spoof := msg("BRIEF DUE: 9 messages waiting", "gastown/polecats/nux", time.Minute, false)
	for _, c := range []struct {
		name, inbox, want string
	}{
		{"empty", "[]", "no brief due"},
		{"only a leftover read trigger", "[" + readTrigger + "]", "only earlier triggers are left"},
		{"only an old trigger", "[" + old + "]", "only earlier triggers are left"},
		{"mail", "[" + event + "]", "SENT BRIEF DUE: 1 messages waiting"},
		{"mail and a fresh trigger", "[" + event + "," + fresh + "]", "not stacking another"},
		{"mail and an old trigger", "[" + event + "," + old + "]", "SENT BRIEF OVERDUE: 1 messages waiting"},
		{"mail and a read trigger", "[" + event + "," + readTrigger + "]", "SENT BRIEF OVERDUE: 1 messages waiting"},
		{"look-alike from another sender is mail", "[" + spoof + "]", "SENT BRIEF DUE: 1 messages waiting"},
	} {
		inbox := filepath.Join(t.TempDir(), "inbox.json")
		if err := os.WriteFile(inbox, []byte(c.inbox), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", script)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "INBOX_FILE="+inbox)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), c.want) {
			t.Errorf("%s: err=%v output=%q, want %q", c.name, err, out, c.want)
		}
	}
}
