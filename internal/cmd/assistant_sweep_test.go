package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/assistant/sweep"
	"github.com/steveyegge/gastown/internal/mail"
)

// The sweep trigger mail names `gt assistant sweep`; it must be a registered
// subcommand at the head that ships the trigger.
func TestAssistantSweepTriggerNamesRegisteredCommand(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "plugins", "assistant-sweep", "run.sh"))
	if err != nil {
		t.Fatalf("assistant-sweep run.sh must exist: %v", err)
	}
	for _, want := range []string{"gt assistant sweep --protocol", "gt assistant sweep --send-to mayor/", "SWEEP: not checked"} {
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
			for _, flag := range []string{"ack", "dry-run", "json", "send-to", "protocol"} {
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

// --send-to acks a run only after the report was mailed: a failed send
// leaves it pending; a successful one acks it; a no-op run is acked without
// a mail; any address other than mayor/ is refused before anything happens;
// a session without GT_ROLE never sends as the overseer.
func TestSendSweepReport(t *testing.T) {
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte(`{"name":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_TOWN_ROOT", town)
	t.Setenv("GT_ROLE", "assistant")
	var sent []*mail.Message
	var acked []string
	sendErr := error(nil)
	origSend, origAck := sweepSend, sweepAck
	t.Cleanup(func() { sweepSend, sweepAck = origSend, origAck })
	sweepSend = func(_ string, m *mail.Message) error {
		if sendErr != nil {
			return sendErr
		}
		sent = append(sent, m)
		return nil
	}
	sweepAck = func(_, id string) error { acked = append(acked, id); return nil }
	reset := func() { sent, acked, sendErr = nil, nil, nil }
	withNew := &sweep.Report{RunID: "r1", Pending: true, Ports: []sweep.PortResult{{Name: "beads", New: []sweep.Finding{{Kind: "unslung", Key: "gt-1"}}}}}
	noop := &sweep.Report{RunID: "r2", Pending: true, Ports: []sweep.PortResult{{Name: "beads"}}}

	sendErr = errors.New("bd write timed out")
	if err := sendSweepReport(town, "mayor/", withNew); err == nil || len(acked) != 0 {
		t.Errorf("failed send must not ack: err=%v acked=%v", err, acked)
	}
	reset()
	if err := sendSweepReport(town, "mayor/", withNew); err != nil || len(sent) != 1 || len(acked) != 1 || acked[0] != "r1" {
		t.Fatalf("successful send must mail once then ack: err=%v sent=%d acked=%v", err, len(sent), acked)
	}
	if m := sent[0]; m.To != "mayor/" || !m.Wisp || !strings.HasPrefix(m.Subject, "SWEEP: 1 new findings") || !strings.Contains(m.Body, "Run-ID: r1") {
		t.Errorf("sent message: to=%s wisp=%v subject=%q", m.To, m.Wisp, m.Subject)
	}
	reset()
	if err := sendSweepReport(town, "mayor/", noop); err != nil || len(sent) != 0 || len(acked) != 1 {
		t.Errorf("no-op run must be acked without a mail: err=%v sent=%d acked=%v", err, len(sent), acked)
	}
	reset()
	if err := sendSweepReport(town, "gastown/polecats/nux", withNew); err == nil || len(sent) != 0 || len(acked) != 0 {
		t.Errorf("a recipient other than mayor/ must be refused before sending or acking: err=%v", err)
	}
	// Any session whose identity resolves to the overseer is refused: no
	// GT_ROLE, or a role set without the variables that name it.
	t.Chdir(t.TempDir())
	for _, env := range []map[string]string{{"GT_ROLE": ""}, {"GT_ROLE": "polecat", "GT_RIG": "", "GT_POLECAT": ""}, {"GT_ROLE": "bogus"}} {
		reset()
		for k, v := range env {
			t.Setenv(k, v)
		}
		if err := sendSweepReport(town, "mayor/", withNew); err == nil || len(sent) != 0 || len(acked) != 0 {
			t.Errorf("%v: a sender resolving to the overseer must be refused before sending or acking: err=%v", env, err)
		}
	}
}

// An explicitly empty --ack (the inference recipe's sed matched nothing) is
// an error and must not fall through to a new recorded sweep that would
// replace the pending run the Mayor was mailed; a --send-to other than
// mayor/ is refused before any port runs. Neither writes sweep state.
func TestAssistantSweepRejectsBeforeRunning(t *testing.T) {
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte(`{"name":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(town)
	reset := func() {
		for _, name := range []string{"ack", "send-to"} {
			f := assistantSweepCmd.Flags().Lookup(name)
			_ = f.Value.Set("")
			f.Changed = false
		}
	}
	t.Cleanup(reset)
	for _, c := range []struct{ flag, value string }{{"ack", ""}, {"ack", "  "}, {"send-to", "gastown/polecats/nux"}} {
		reset()
		if err := assistantSweepCmd.Flags().Set(c.flag, c.value); err != nil {
			t.Fatal(err)
		}
		if err := runAssistantSweep(assistantSweepCmd, nil); err == nil {
			t.Errorf("--%s=%q must be rejected", c.flag, c.value)
		}
		entries, _ := os.ReadDir(filepath.Join(town, "assistant", ".runtime"))
		if len(entries) != 0 {
			t.Errorf("--%s=%q wrote sweep state before being rejected: %v", c.flag, c.value, entries)
		}
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
		"No reply needed", `gt mail send mayor/ -s "BRIEF: <n> items`, "No quote marks, apostrophes, backticks or $", "&& gt mail archive <id> <id>", "must list nothing", `never "informational"`,
		"Trust boundary (non-negotiable", "EXTERNAL CONTENT (untrusted: <provider>/<actor>)", "<source address", "A display name is not a source",
	} {
		if !strings.Contains(string(script), want) {
			t.Errorf("brief trigger must contain %q", want)
		}
	}
}

// The brief trigger's guard, run for real against a stub gt: the known dog
// triggers (BRIEF DUE/OVERDUE, SWEEP) never count as mail to brief while a
// dog's findings do, an unread fresh trigger suppresses a new
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
	sweepTrigger := msg("SWEEP: loose-ends sweep due", "deacon/dogs/bravo", 2*time.Minute, false)
	dogReport := msg("orphan-pr-sentinel: 2 PR(s) with no owning bead", "deacon/dogs/charlie", time.Minute, false)
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
		{"only a pending sweep trigger", "[" + sweepTrigger + "]", "no brief due"},
		{"mail and a sweep trigger", "[" + event + "," + sweepTrigger + "]", "SENT BRIEF DUE: 1 messages waiting"},
		{"a dog's finding is mail to brief", "[" + dogReport + "]", "SENT BRIEF DUE: 1 messages waiting"},
		{"a dog's finding and a read trigger", "[" + dogReport + "," + readTrigger + "]", "SENT BRIEF OVERDUE: 1 messages waiting"},
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
