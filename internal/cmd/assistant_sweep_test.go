package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	reset()
	t.Setenv("GT_ROLE", "")
	if err := sendSweepReport(town, "mayor/", withNew); err == nil || len(sent) != 0 || len(acked) != 0 {
		t.Errorf("without GT_ROLE the report must not be sent (it would claim the overseer): err=%v", err)
	}
}
