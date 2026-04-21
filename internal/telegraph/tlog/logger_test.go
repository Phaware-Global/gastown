package tlog_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/telegraph/tlog"
)

func parseLines(buf *bytes.Buffer) []map[string]interface{} {
	var out []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

func str(m map[string]interface{}, k string) string {
	v, _ := m[k].(string)
	return v
}

func TestLogger_Accept(t *testing.T) {
	var buf bytes.Buffer
	l := tlog.New(&buf)
	l.Accept("jira", "1.2.3.4", "evt-1", 256, 5)

	if v := l.Counters.Accept.Load(); v != 1 {
		t.Errorf("Accept counter = %d, want 1", v)
	}
	lines := parseLines(&buf)
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	line := lines[0]
	if str(line, "event") != "accept" {
		t.Errorf("event = %q, want accept", str(line, "event"))
	}
	if str(line, "provider") != "jira" {
		t.Errorf("provider = %q, want jira", str(line, "provider"))
	}
	if str(line, "component") != "telegraph" {
		t.Errorf("component = %q, want telegraph", str(line, "component"))
	}
	if str(line, "ts") == "" {
		t.Error("ts field missing")
	}
	if line["bytes_len"].(float64) != 256 {
		t.Errorf("bytes_len = %v, want 256", line["bytes_len"])
	}
}

func TestLogger_Reject_AllReasons(t *testing.T) {
	cases := []struct {
		reason  string
		counter func(*tlog.Logger) int64
	}{
		{tlog.ReasonHMACInvalid, func(l *tlog.Logger) int64 { return l.Counters.RejectHMACInvalid.Load() }},
		{tlog.ReasonUnknownEventType, func(l *tlog.Logger) int64 { return l.Counters.RejectUnknownType.Load() }},
		{tlog.ReasonParseError, func(l *tlog.Logger) int64 { return l.Counters.RejectParseError.Load() }},
		{tlog.ReasonBackpressure, func(l *tlog.Logger) int64 { return l.Counters.RejectBackpressure.Load() }},
		{tlog.ReasonProviderDisabled, func(l *tlog.Logger) int64 { return l.Counters.RejectProviderDis.Load() }},
	}

	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			var buf bytes.Buffer
			l := tlog.New(&buf)
			l.Reject("jira", "1.2.3.4", tc.reason, "")

			if v := tc.counter(l); v != 1 {
				t.Errorf("counter for %q = %d, want 1", tc.reason, v)
			}
			lines := parseLines(&buf)
			if len(lines) != 1 {
				t.Fatalf("want 1 line, got %d", len(lines))
			}
			if str(lines[0], "event") != "reject" {
				t.Errorf("event = %q, want reject", str(lines[0], "event"))
			}
			if str(lines[0], "reason") != tc.reason {
				t.Errorf("reason = %q, want %q", str(lines[0], "reason"), tc.reason)
			}
		})
	}
}

func TestLogger_Deliver(t *testing.T) {
	var buf bytes.Buffer
	l := tlog.New(&buf)
	l.Deliver("jira", "issue.created", "evt-42", "alice", "PROJ-1", "hq-wisp-abc")

	if v := l.Counters.Deliver.Load(); v != 1 {
		t.Errorf("Deliver counter = %d, want 1", v)
	}
	lines := parseLines(&buf)
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	line := lines[0]
	checks := map[string]string{
		"event":      "deliver",
		"provider":   "jira",
		"event_type": "issue.created",
		"event_id":   "evt-42",
		"actor":      "alice",
		"subject":    "PROJ-1",
		"mail_id":    "hq-wisp-abc",
	}
	for k, want := range checks {
		if str(line, k) != want {
			t.Errorf("%s = %q, want %q", k, str(line, k), want)
		}
	}
}

func TestLogger_Drop(t *testing.T) {
	var buf bytes.Buffer
	l := tlog.New(&buf)
	l.Drop("jira", "issue.created", "evt-1", "dedup")

	if v := l.Counters.Drop.Load(); v != 1 {
		t.Errorf("Drop counter = %d, want 1", v)
	}
	lines := parseLines(&buf)
	if str(lines[0], "event") != "drop" {
		t.Errorf("event = %q, want drop", str(lines[0], "event"))
	}
}

func TestLogger_NudgeSent(t *testing.T) {
	var buf bytes.Buffer
	l := tlog.New(&buf)
	l.NudgeSent()

	if v := l.Counters.NudgeSent.Load(); v != 1 {
		t.Errorf("NudgeSent counter = %d, want 1", v)
	}
	lines := parseLines(&buf)
	if str(lines[0], "event") != "nudge_sent" {
		t.Errorf("event = %q, want nudge_sent", str(lines[0], "event"))
	}
}

func TestLogger_NudgeSuppressed(t *testing.T) {
	var buf bytes.Buffer
	l := tlog.New(&buf)
	l.NudgeSuppressed()

	if v := l.Counters.NudgeSuppressed.Load(); v != 1 {
		t.Errorf("NudgeSuppressed counter = %d, want 1", v)
	}
	lines := parseLines(&buf)
	if str(lines[0], "event") != "nudge_suppressed" {
		t.Errorf("event = %q, want nudge_suppressed", str(lines[0], "event"))
	}
}

func TestLogger_Nil_NoPanic(t *testing.T) {
	var l *tlog.Logger
	// All methods on nil *Logger must be no-ops.
	l.Accept("jira", "1.2.3.4", "", 0, 0)
	l.Reject("jira", "1.2.3.4", tlog.ReasonHMACInvalid, "")
	l.Deliver("jira", "issue.created", "", "alice", "PROJ-1", "")
	l.Drop("jira", "issue.created", "", "dedup")
	l.NudgeSent()
	l.NudgeSuppressed()
}

func TestLogger_CountersAreAdditive(t *testing.T) {
	var buf bytes.Buffer
	l := tlog.New(&buf)
	for range 5 {
		l.Accept("jira", "1.2.3.4", "", 0, 0)
	}
	for range 3 {
		l.Reject("jira", "1.2.3.4", tlog.ReasonHMACInvalid, "")
	}
	if v := l.Counters.Accept.Load(); v != 5 {
		t.Errorf("Accept = %d, want 5", v)
	}
	if v := l.Counters.RejectHMACInvalid.Load(); v != 3 {
		t.Errorf("RejectHMACInvalid = %d, want 3", v)
	}
}
