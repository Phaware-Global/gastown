package cmd

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPricingForModel_FamilyFallback(t *testing.T) {
	tests := []struct {
		model string
		want  string // key in modelPricing whose prices we expect
	}{
		{"claude-opus-4-5-20251101", "claude-opus-4-5-20251101"},
		{"claude-opus-5-5", "claude-opus-4-5-20251101"},
		{"claude-sonnet-5-5", "claude-sonnet-4-20250514"},
		{"claude-sonnet-5", "claude-sonnet-4-20250514"},
		{"claude-haiku-5-5", "claude-3-5-haiku-20241022"},
		{"<synthetic>", "default"},
		{"", "default"},
	}
	for _, tt := range tests {
		if got := pricingForModel(tt.model); got != modelPricing[tt.want] {
			t.Errorf("pricingForModel(%q) = %+v, want prices of %q", tt.model, got, tt.want)
		}
	}
}

// writeTranscriptTurns appends n assistant turns, each with the given usage, to
// a transcript file and stamps its mtime so findLatestTranscript picks it.
func writeTranscriptTurns(t *testing.T, path string, n int, output int, mtime time.Time) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		fmt.Fprintf(f, `{"type":"assistant","message":{"model":"claude-opus-5-5","usage":{"input_tokens":1000,"output_tokens":%d}}}`+"\n", output)
	}
	f.Close()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func readCostLog(t *testing.T) []CostLogEntry {
	t.Helper()
	data, err := os.ReadFile(getCostsLogPath())
	if err != nil {
		t.Fatalf("reading costs log: %v", err)
	}
	var entries []CostLogEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e CostLogEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("parsing log line %q: %v", line, err)
		}
		entries = append(entries, e)
	}
	return entries
}

func TestRunCostsRecord_LogsIncrementNotCumulative(t *testing.T) {
	home := t.TempDir()
	workDir := filepath.Join(home, "mayor")
	t.Setenv("GT_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
	t.Setenv("GT_SESSION", "hq-mayor")
	t.Setenv("GT_CWD", workDir)
	oldSession, oldWorkItem := recordSession, recordWorkItem
	recordSession, recordWorkItem = "", ""
	t.Cleanup(func() { recordSession, recordWorkItem = oldSession, oldWorkItem })

	projectDir, err := getClaudeProjectDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(projectDir, "session-a.jsonl")
	base := time.Now().Add(-time.Hour)

	oneTurn := calculateCost(&TokenUsage{Model: "claude-opus-5-5", InputTokens: 1000, OutputTokens: 100})
	approx := func(got, want float64, what string) {
		t.Helper()
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("%s = %.6f, want %.6f", what, got, want)
		}
	}

	// A legacy entry (no cumulative_usd) for this session: its cost_usd was the
	// cumulative total at the time, so it must serve as the baseline.
	legacy := CostLogEntry{SessionID: "hq-mayor", Role: "mayor", CostUSD: oneTurn, EndedAt: base}
	line, _ := json.Marshal(legacy)
	if err := os.MkdirAll(filepath.Dir(getCostsLogPath()), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(getCostsLogPath(), append(line, '\n'), 0644); err != nil {
		t.Fatal(err)
	}

	// Transcript now holds 3 turns: 2 more than the legacy baseline.
	writeTranscriptTurns(t, transcript, 3, 100, base.Add(time.Minute))
	if err := runCostsRecord(nil, nil); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	// 2 more turns since then.
	writeTranscriptTurns(t, transcript, 2, 100, base.Add(2*time.Minute))
	if err := runCostsRecord(nil, nil); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	// Session rolled over to a fresh transcript with 1 turn: counts in full.
	writeTranscriptTurns(t, filepath.Join(projectDir, "session-b.jsonl"), 1, 100, base.Add(3*time.Minute))
	if err := runCostsRecord(nil, nil); err != nil {
		t.Fatalf("record 3: %v", err)
	}

	entries := readCostLog(t)
	if len(entries) != 4 {
		t.Fatalf("got %d log entries, want 4 (1 legacy + 3 recorded)", len(entries))
	}
	for i, e := range entries[1:] {
		if e.CumulativeUSD == nil {
			t.Fatalf("entry %d has no cumulative_usd", i+1)
		}
	}
	approx(entries[1].CostUSD, 2*oneTurn, "record 1 cost (increment over legacy baseline)")
	approx(*entries[1].CumulativeUSD, 3*oneTurn, "record 1 cumulative")
	approx(entries[2].CostUSD, 2*oneTurn, "record 2 cost")
	approx(*entries[2].CumulativeUSD, 5*oneTurn, "record 2 cumulative")
	approx(entries[3].CostUSD, oneTurn, "record 3 cost (after transcript rollover)")
	approx(*entries[3].CumulativeUSD, oneTurn, "record 3 cumulative")

	// Summing today's entries must give what was actually spent today
	// (legacy baseline contributes 0: no in-day baseline before it).
	dayEntries, err := querySessionCostEntries(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, e := range dayEntries {
		total += e.CostUSD
	}
	approx(total, 5*oneTurn, "sum of today's entries")
}

func TestRunCostsRecord_UnreadableTranscriptKeepsBaseline(t *testing.T) {
	home := t.TempDir()
	workDir := filepath.Join(home, "mayor")
	t.Setenv("GT_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
	t.Setenv("GT_SESSION", "hq-mayor")
	t.Setenv("GT_CWD", workDir)
	oldSession, oldWorkItem := recordSession, recordWorkItem
	recordSession, recordWorkItem = "", ""
	t.Cleanup(func() { recordSession, recordWorkItem = oldSession, oldWorkItem })

	cum := 42.0
	prev := CostLogEntry{SessionID: "hq-mayor", Role: "mayor", CostUSD: 1, CumulativeUSD: &cum, EndedAt: time.Now()}
	line, _ := json.Marshal(prev)
	if err := os.MkdirAll(filepath.Dir(getCostsLogPath()), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(getCostsLogPath(), append(line, '\n'), 0644); err != nil {
		t.Fatal(err)
	}

	// No transcript exists for workDir: extraction fails.
	if err := runCostsRecord(nil, nil); err != nil {
		t.Fatalf("record: %v", err)
	}
	entries := readCostLog(t)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	got := entries[1]
	if got.CostUSD != 0 || got.CumulativeUSD == nil || *got.CumulativeUSD != 42 {
		t.Errorf("entry after failed extraction = cost %.2f cumulative %v, want cost 0 and cumulative 42 carried forward", got.CostUSD, got.CumulativeUSD)
	}
}

func TestQuerySessionCostEntries_CollapsesLegacyCumulativeRows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GT_HOME", home)
	now := time.Now()
	yesterday := now.Add(-24 * time.Hour)
	f := func(v float64) *float64 { return &v }

	rows := []CostLogEntry{
		{SessionID: "hq-mayor", Role: "mayor", CostUSD: 999, EndedAt: yesterday},               // other day: ignored
		{SessionID: "hq-mayor", Role: "mayor", CostUSD: 100, EndedAt: now},                     // legacy, first of day: 0
		{SessionID: "hq-mayor", Role: "mayor", CostUSD: 150, EndedAt: now},                     // legacy: +50
		{SessionID: "hq-mayor", Role: "mayor", CostUSD: 170, EndedAt: now},                     // legacy: +20
		{SessionID: "hq-mayor", Role: "mayor", CostUSD: 20, EndedAt: now},                      // legacy, rollover: +20
		{SessionID: "hq-mayor", Role: "mayor", CostUSD: 5, CumulativeUSD: f(25), EndedAt: now}, // new format: +5
		{SessionID: "hq-deacon", Role: "deacon", CostUSD: 40, EndedAt: now},                    // legacy, only row: 0
	}
	if err := os.MkdirAll(filepath.Dir(getCostsLogPath()), 0755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, r := range rows {
		b, _ := json.Marshal(r)
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(getCostsLogPath(), []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}

	entries, err := querySessionCostEntries(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 6 {
		t.Fatalf("got %d entries for today, want 6", len(entries))
	}
	wantCosts := []float64{0, 50, 20, 20, 5, 0}
	var total float64
	for i, e := range entries {
		if e.CostUSD != wantCosts[i] {
			t.Errorf("entry %d (%s) cost = %.2f, want %.2f", i, e.SessionID, e.CostUSD, wantCosts[i])
		}
		total += e.CostUSD
	}
	if total != 95 {
		t.Errorf("total = %.2f, want 95", total)
	}
	if n := countDistinctSessions(entries); n != 2 {
		t.Errorf("countDistinctSessions = %d, want 2", n)
	}
}
