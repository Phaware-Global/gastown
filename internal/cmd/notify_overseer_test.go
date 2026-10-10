package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// The slack_overseer_only gate: gt escalate must never POST to Slack on its
// own unless the operator explicitly opts back in.
func TestExecuteExternalActionsSlackOverseerOnlyGate(t *testing.T) {
	var hits atomic.Int32
	slackStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer slackStub.Close()

	t.Run("default suppresses slack and is not a missing-contact skip", func(t *testing.T) {
		hits.Store(0)
		cfg := &config.EscalationConfig{Contacts: config.EscalationContacts{SlackWebhook: slackStub.URL}}
		statuses := executeExternalActions([]string{"slack"}, cfg, "hq-esc1", "critical", "desc", t.TempDir())
		if hits.Load() != 0 {
			t.Fatalf("slack webhook was called %d times, want 0", hits.Load())
		}
		if len(statuses) != 1 || statuses[0].Channel != "slack" {
			t.Fatalf("statuses = %#v, want one slack status", statuses)
		}
		if !strings.Contains(statuses[0].Warning, "slack route suppressed") || !strings.Contains(statuses[0].Warning, "gt notify-overseer") {
			t.Errorf("Warning = %q, want suppression warning pointing at gt notify-overseer", statuses[0].Warning)
		}
		if statuses[0].Skipped || statuses[0].RuntimeNotified {
			t.Errorf("status = %#v, want neither Skipped nor RuntimeNotified", statuses[0])
		}
		if skips := collectMissingContactSkips("critical", statuses); len(skips) != 0 {
			t.Errorf("collectMissingContactSkips = %v, want none (suppression must not hard-fail)", skips)
		}
	})

	t.Run("explicit false posts to slack", func(t *testing.T) {
		hits.Store(0)
		off := false
		cfg := &config.EscalationConfig{SlackOverseerOnly: &off, Contacts: config.EscalationContacts{SlackWebhook: slackStub.URL}}
		statuses := executeExternalActions([]string{"slack"}, cfg, "hq-esc1", "critical", "desc", t.TempDir())
		if hits.Load() != 1 {
			t.Fatalf("slack webhook was called %d times, want 1", hits.Load())
		}
		if len(statuses) != 1 || !statuses[0].RuntimeNotified {
			t.Fatalf("statuses = %#v, want delivered slack status", statuses)
		}
	})
}

func TestValidateNotifyOverseerCategory(t *testing.T) {
	for _, ok := range []string{"infra", "ops", "security", " Infra ", "SECURITY"} {
		if got, err := validateNotifyOverseerCategory(ok); err != nil || got != strings.ToLower(strings.TrimSpace(ok)) {
			t.Errorf("validateNotifyOverseerCategory(%q) = %q, %v; want ok", ok, got, err)
		}
	}
	for _, bad := range []string{"", "product", "stakeholder", "feature", "infra,ops"} {
		_, err := validateNotifyOverseerCategory(bad)
		if err == nil {
			t.Errorf("validateNotifyOverseerCategory(%q) = nil error, want rejection", bad)
			continue
		}
		if !strings.Contains(err.Error(), "infra, ops, security") {
			t.Errorf("error for %q = %v, want it to list the allowed categories", bad, err)
		}
	}
}

func TestValidateNotifyOverseerSender(t *testing.T) {
	for _, ok := range []string{"mayor/", "overseer", ""} {
		if err := validateNotifyOverseerSender(ok); err != nil {
			t.Errorf("validateNotifyOverseerSender(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"deacon/", "gastown/witness", "gastown/crew/dom", "gastown/polecats/toast"} {
		if err := validateNotifyOverseerSender(bad); err == nil || !strings.Contains(err.Error(), "restricted to the mayor") {
			t.Errorf("validateNotifyOverseerSender(%q) = %v, want mayor-only rejection", bad, err)
		}
	}
}

func TestFormatOpsBrief(t *testing.T) {
	now := time.Date(2026, 10, 10, 14, 5, 0, 0, time.UTC)
	refs := []notifyOverseerRef{
		{ID: "hq-abc", Title: "dolt-archive: dolt push failed for 1 remote(s)"},
		{ID: "hq-def", Title: "Agent host disk 96% full"},
	}
	got := formatOpsBrief(now, "infra", "CI pipeline down", "Runners offline since 09:40; 4 PRs blocked.\n", refs)
	want := "🚨 *Gastown Ops Brief – 2026-10-10 14:05 UTC*\n" +
		"`[INFRA]` *CI pipeline down*\n" +
		"Runners offline since 09:40; 4 PRs blocked.\n" +
		"\n*References:*\n" +
		"• hq-abc — dolt-archive: dolt push failed for 1 remote(s)\n" +
		"• hq-def — Agent host disk 96% full\n" +
		"\n_Ack with: `gt escalate ack hq-abc hq-def`_\n"
	if got != want {
		t.Errorf("formatOpsBrief() =\n%s\nwant:\n%s", got, want)
	}

	noRefs := formatOpsBrief(now, "security", "Credential rotation needed", "body", nil)
	if strings.Contains(noRefs, "References") || strings.Contains(noRefs, "Ack with") {
		t.Errorf("formatOpsBrief() without refs must omit references/footer, got:\n%s", noRefs)
	}
	if !strings.HasPrefix(noRefs, "🚨 *Gastown Ops Brief – ") || !strings.Contains(noRefs, "`[SECURITY]`") {
		t.Errorf("formatOpsBrief() header/badge missing, got:\n%s", noRefs)
	}
	// Must not look like the old per-escalation format.
	if strings.Contains(got, "Escalation hq-") || strings.Contains(got, "🔴") {
		t.Errorf("formatOpsBrief() must not reuse the per-escalation format, got:\n%s", got)
	}
}

func TestSlackForwardedLabel(t *testing.T) {
	now := time.Date(2026, 10, 10, 14, 5, 9, 0, time.UTC)
	if got, want := slackForwardedLabel(now), "slack-forwarded:20261010T140509Z"; got != want {
		t.Errorf("slackForwardedLabel() = %q, want %q", got, want)
	}
}

func TestRunNotifyOverseerValidation(t *testing.T) {
	origSubject, origMessage, origStdin, origCategory := notifyOverseerSubject, notifyOverseerMessage, notifyOverseerStdin, notifyOverseerCategory
	defer func() {
		notifyOverseerSubject, notifyOverseerMessage, notifyOverseerStdin, notifyOverseerCategory = origSubject, origMessage, origStdin, origCategory
	}()
	t.Setenv("GT_ROLE", "")

	tests := []struct {
		name     string
		subject  string
		message  string
		stdin    bool
		category string
		role     string
		wantErr  string
	}{
		{name: "stdin and message conflict", subject: "s", message: "m", stdin: true, category: "infra", wantErr: "cannot use --stdin with --message"},
		{name: "missing subject", subject: "  ", message: "m", category: "infra", wantErr: "--subject is required"},
		{name: "missing message", subject: "s", category: "infra", wantErr: "message body is required"},
		{name: "product category rejected", subject: "s", message: "m", category: "product", wantErr: `invalid --category "product"`},
		{name: "non-mayor agent rejected", subject: "s", message: "m", category: "ops", role: "deacon", wantErr: "restricted to the mayor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			notifyOverseerSubject, notifyOverseerMessage, notifyOverseerStdin, notifyOverseerCategory = tt.subject, tt.message, tt.stdin, tt.category
			t.Setenv("GT_ROLE", tt.role)
			err := runNotifyOverseer(notifyOverseerCmd, nil)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("runNotifyOverseer() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
