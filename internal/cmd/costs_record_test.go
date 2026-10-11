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

func TestParseModelFamilyVersion(t *testing.T) {
	tests := []struct {
		model, family, version string
	}{
		{"claude-opus-5-5", "opus", "5.5"},
		{"claude-opus-5-5[1m]", "opus", "5.5"},
		{"claude-sonnet-5", "sonnet", "5"},
		{"claude-opus-4-5-20251101", "opus", "4.5"},
		{"claude-sonnet-4-20250514", "sonnet", "4"},
		{"claude-3-5-haiku-20241022", "haiku", "3.5"},
		{"claude-fable-5-1", "fable", "5.1"},
		{"claude-haiku-7", "haiku", "7"},
		{"<synthetic>", "", ""},
		{"", "", ""},
	}
	for _, tt := range tests {
		family, version := parseModelFamilyVersion(tt.model)
		if family != tt.family || version != tt.version {
			t.Errorf("parseModelFamilyVersion(%q) = (%q, %q), want (%q, %q)", tt.model, family, version, tt.family, tt.version)
		}
	}
}

func TestPricingForModel(t *testing.T) {
	tests := []struct {
		model string
		want  string // key in modelPricing whose prices we expect
	}{
		{"claude-opus-5-5", "opus-5.5"},
		{"claude-opus-5-5[1m]", "opus-5.5"},
		{"claude-sonnet-5-5", "sonnet-5.5"},
		{"claude-sonnet-5", "sonnet-5"},
		{"claude-haiku-5-5", "haiku-5.5"},
		{"claude-opus-4-5-20251101", "opus-4.5"},
		{"claude-sonnet-4-20250514", "sonnet-4"},
		{"claude-3-5-haiku-20241022", "haiku-3.5"},
		{"claude-haiku-7", "haiku-5.5"}, // unknown version: family's latest row
		{"<synthetic>", "default"},
		{"", "default"},
	}
	for _, tt := range tests {
		if got := pricingForModel(tt.model); got != modelPricing[tt.want] {
			t.Errorf("pricingForModel(%q) = %+v, want prices of %q", tt.model, got, tt.want)
		}
	}

	// Spot-check the rows the town actually runs against the published list
	// (input, output, cache read, cache write per MTok).
	want := map[string]modelPrice{
		"claude-opus-5-5":   {4, 20, 0.20, 5},
		"claude-sonnet-5-5": {2, 10, 0.10, 2.5},
		"claude-sonnet-5":   {2, 10, 0.20, 2.5},
		"claude-haiku-5-5":  {0.10, 0.50, 0.01, 0.125},
	}
	for model, w := range want {
		if got := pricingForModel(model); got != w {
			t.Errorf("pricingForModel(%q) = %+v, want %+v", model, got, w)
		}
	}
}

// writeTranscriptTurns appends n assistant turns to a transcript file and
// stamps its mtime so findLatestTranscript picks it.
func writeTranscriptTurns(t *testing.T, path string, n int, mtime time.Time) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		fmt.Fprintln(f, `{"type":"assistant","message":{"model":"claude-opus-5-5","usage":{"input_tokens":1000,"output_tokens":100}}}`)
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

func writeCostLog(t *testing.T, rows []CostLogEntry) {
	t.Helper()
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
}

// setupRecordEnv points gt costs record at a temp log, a temp Claude config
// dir and a fixed session, and returns the project dir transcripts go in.
func setupRecordEnv(t *testing.T) string {
	t.Helper()
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
	return projectDir
}

func approxEqual(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.6f, want %.6f", what, got, want)
	}
}

func record(t *testing.T, step string) {
	t.Helper()
	if err := runCostsRecord(nil, nil); err != nil {
		t.Fatalf("%s: %v", step, err)
	}
}

func TestRunCostsRecord_LogsIncrementNotCumulative(t *testing.T) {
	projectDir := setupRecordEnv(t)
	base := time.Now().Add(-time.Hour)
	oneTurn := calculateCost(&TokenUsage{Model: "claude-opus-5-5", InputTokens: 1000, OutputTokens: 100})
	a := filepath.Join(projectDir, "session-a.jsonl")
	b := filepath.Join(projectDir, "session-b.jsonl")
	c := filepath.Join(projectDir, "session-c.jsonl")

	// A legacy entry (no cumulative_usd, no transcript): its cost_usd was the
	// session total at the time, so it is the baseline for the next record.
	writeCostLog(t, []CostLogEntry{{SessionID: "hq-mayor", Role: "mayor", CostUSD: oneTurn, EndedAt: base}})

	writeTranscriptTurns(t, a, 3, base.Add(1*time.Minute)) // 2 turns beyond the legacy baseline
	record(t, "record 1")
	writeTranscriptTurns(t, a, 2, base.Add(2*time.Minute)) // +2
	record(t, "record 2")
	// Session name reused with a fresh transcript that costs MORE than the old
	// one's final total: must be charged in full, not as a difference.
	writeTranscriptTurns(t, b, 6, base.Add(3*time.Minute))
	record(t, "record 3")
	// Back to transcript A (two processes sharing a workdir): charged against
	// A's own baseline, not B's.
	writeTranscriptTurns(t, a, 1, base.Add(4*time.Minute))
	record(t, "record 4")
	// Fresh transcript that costs less than the previous one.
	writeTranscriptTurns(t, c, 1, base.Add(5*time.Minute))
	record(t, "record 5")

	entries := readCostLog(t)
	if len(entries) != 6 {
		t.Fatalf("got %d log entries, want 6 (1 legacy + 5 recorded)", len(entries))
	}
	type want struct {
		cost, cumulative float64
		transcript       string
	}
	wants := []want{
		{2 * oneTurn, 3 * oneTurn, "session-a.jsonl"},
		{2 * oneTurn, 5 * oneTurn, "session-a.jsonl"},
		{6 * oneTurn, 6 * oneTurn, "session-b.jsonl"},
		{1 * oneTurn, 6 * oneTurn, "session-a.jsonl"},
		{1 * oneTurn, 1 * oneTurn, "session-c.jsonl"},
	}
	for i, w := range wants {
		e := entries[i+1]
		if e.CumulativeUSD == nil {
			t.Fatalf("record %d has no cumulative_usd", i+1)
		}
		approxEqual(t, e.CostUSD, w.cost, fmt.Sprintf("record %d cost", i+1))
		approxEqual(t, *e.CumulativeUSD, w.cumulative, fmt.Sprintf("record %d cumulative", i+1))
		if e.Transcript != w.transcript {
			t.Errorf("record %d transcript = %q, want %q", i+1, e.Transcript, w.transcript)
		}
	}

	// Summing today's entries gives today's spend: 12 turns were written today
	// (the legacy row has no in-day baseline and contributes 0).
	dayEntries, err := querySessionCostEntries(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, e := range dayEntries {
		total += e.CostUSD
	}
	approxEqual(t, total, 12*oneTurn, "sum of today's entries")
}

func TestRunCostsRecord_UnreadableTranscriptKeepsBaseline(t *testing.T) {
	setupRecordEnv(t)
	cum := 42.0
	writeCostLog(t, []CostLogEntry{{SessionID: "hq-mayor", Role: "mayor", CostUSD: 1, CumulativeUSD: &cum, Transcript: "x.jsonl", EndedAt: time.Now()}})

	// No transcript exists for the workdir: extraction fails.
	record(t, "record")

	entries := readCostLog(t)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	got := entries[1]
	if got.CostUSD != 0 || got.CumulativeUSD == nil || *got.CumulativeUSD != 42 || got.Transcript != "x.jsonl" {
		t.Errorf("entry after failed extraction = %+v, want cost 0 with cumulative 42 and transcript x.jsonl carried forward", got)
	}
}

func TestDeleteSessionCostEntries_LeavesBaselineForNextRecord(t *testing.T) {
	projectDir := setupRecordEnv(t)
	base := time.Now().Add(-time.Hour)
	oneTurn := calculateCost(&TokenUsage{Model: "claude-opus-5-5", InputTokens: 1000, OutputTokens: 100})
	a := filepath.Join(projectDir, "session-a.jsonl")

	writeTranscriptTurns(t, a, 3, base.Add(1*time.Minute))
	record(t, "record 1")
	writeTranscriptTurns(t, a, 2, base.Add(2*time.Minute))
	record(t, "record 2")

	// Digest the day: today's rows go away, but a baseline marker must remain.
	deleted, err := deleteSessionCostEntries(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}
	entries := readCostLog(t)
	if len(entries) != 1 || !entries[0].Baseline || entries[0].CostUSD != 0 || entries[0].Transcript != "session-a.jsonl" {
		t.Fatalf("after delete got %+v, want a single baseline marker for session-a.jsonl", entries)
	}
	approxEqual(t, *entries[0].CumulativeUSD, 5*oneTurn, "baseline cumulative")

	// The marker carries no spend for the ledger...
	dayEntries, err := querySessionCostEntries(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(dayEntries) != 0 {
		t.Errorf("ledger returned %d entries after digest, want 0", len(dayEntries))
	}

	// ...but the next record is charged only its own growth, not the lifetime.
	writeTranscriptTurns(t, a, 1, base.Add(3*time.Minute))
	record(t, "record 3")
	entries = readCostLog(t)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	approxEqual(t, entries[1].CostUSD, oneTurn, "record after digest cost")
	approxEqual(t, *entries[1].CumulativeUSD, 6*oneTurn, "record after digest cumulative")
}

func TestQuerySessionCostEntries_CollapsesLegacyCumulativeRows(t *testing.T) {
	t.Setenv("GT_HOME", t.TempDir())
	now := time.Now()
	yesterday := now.Add(-24 * time.Hour)
	f := func(v float64) *float64 { return &v }

	writeCostLog(t, []CostLogEntry{
		{SessionID: "hq-mayor", Role: "mayor", CostUSD: 999, EndedAt: yesterday},                   // other day: ignored
		{SessionID: "hq-mayor", Role: "mayor", CostUSD: 100, EndedAt: now},                         // legacy, first of day: 0
		{SessionID: "hq-mayor", Role: "mayor", CostUSD: 150, EndedAt: now},                         // legacy: +50
		{SessionID: "hq-mayor", Role: "mayor", CostUSD: 170, EndedAt: now},                         // legacy: +20
		{SessionID: "hq-mayor", Role: "mayor", CostUSD: 20, EndedAt: now},                          // legacy, rollover: +20
		{SessionID: "hq-mayor", Role: "mayor", CostUSD: 5, CumulativeUSD: f(25), EndedAt: now},     // new format: +5
		{SessionID: "hq-mayor", Role: "mayor", CumulativeUSD: f(25), Baseline: true, EndedAt: now}, // digest marker: skipped
		{SessionID: "hq-deacon", Role: "deacon", CostUSD: 40, EndedAt: now},                        // legacy, only row: 0
	})

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

func TestDeleteSessionCostEntries_YesterdayDigestKeepsTodayBaseline(t *testing.T) {
	projectDir := setupRecordEnv(t)
	oneTurn := calculateCost(&TokenUsage{Model: "claude-opus-5-5", InputTokens: 1000, OutputTokens: 100})
	a := filepath.Join(projectDir, "session-a.jsonl")
	yesterday := time.Now().Add(-24 * time.Hour)
	base := time.Now().Add(-time.Hour)

	// Yesterday: the session recorded a total of 5 turns.
	five := 5 * oneTurn
	writeCostLog(t, []CostLogEntry{{SessionID: "hq-mayor", Role: "mayor", CostUSD: five, CumulativeUSD: &five, Transcript: "session-a.jsonl", EndedAt: yesterday}})
	writeTranscriptTurns(t, a, 5, base)

	// Today: +2 and +1 are recorded while the session keeps running.
	writeTranscriptTurns(t, a, 2, base.Add(1*time.Minute))
	record(t, "record today 1")
	writeTranscriptTurns(t, a, 1, base.Add(2*time.Minute))
	record(t, "record today 2")

	// The deacon digests yesterday, then one more turn is recorded.
	if _, err := deleteSessionCostEntries(yesterday); err != nil {
		t.Fatal(err)
	}
	writeTranscriptTurns(t, a, 1, base.Add(3*time.Minute))
	record(t, "record after digest")

	entries := readCostLog(t)
	if len(entries) != 4 || !entries[0].Baseline {
		t.Fatalf("got %d entries (first baseline=%v), want the marker first followed by 3 records", len(entries), len(entries) > 0 && entries[0].Baseline)
	}
	approxEqual(t, entries[3].CostUSD, oneTurn, "record after yesterday's digest")

	dayEntries, err := querySessionCostEntries(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, e := range dayEntries {
		total += e.CostUSD
	}
	approxEqual(t, total, 4*oneTurn, "today's total after digesting yesterday")
}

func TestRunCostsRecord_LegacyDigestMarkerIsBaseline(t *testing.T) {
	projectDir := setupRecordEnv(t)
	oneTurn := calculateCost(&TokenUsage{Model: "claude-opus-5-5", InputTokens: 1000, OutputTokens: 100})
	yesterday := time.Now().Add(-24 * time.Hour)

	// Yesterday's only entry predates transcript tracking; digest it.
	writeCostLog(t, []CostLogEntry{{SessionID: "hq-mayor", Role: "mayor", CostUSD: 5 * oneTurn, EndedAt: yesterday}})
	if _, err := deleteSessionCostEntries(yesterday); err != nil {
		t.Fatal(err)
	}

	// The transcript has grown by one turn since then.
	writeTranscriptTurns(t, filepath.Join(projectDir, "session-a.jsonl"), 6, time.Now())
	record(t, "record after legacy digest")

	entries := readCostLog(t)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want marker + record", len(entries))
	}
	approxEqual(t, entries[1].CostUSD, oneTurn, "record after legacy digest marker")
}
