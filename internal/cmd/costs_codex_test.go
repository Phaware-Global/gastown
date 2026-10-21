package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Lines in the Codex 0.163 rollout format (field names copied from a real
// rollout on the agent machine, values shortened).
func codexMeta(cwd string) string {
	return fmt.Sprintf(`{"timestamp":"2026-10-21T00:47:15Z","type":"session_meta","payload":{"id":"s1","cwd":%q,"cli_version":"0.163.0","originator":"codex_exec","model_provider":"openai","source":"exec"}}`, cwd)
}

func codexTurn(model string) string {
	return fmt.Sprintf(`{"timestamp":"2026-10-21T00:47:16Z","type":"turn_context","payload":{"model":%q,"cwd":"/w"}}`, model)
}

func codexUsageLine(input, cached, write, output int) string {
	u := fmt.Sprintf(`{"input_tokens":%d,"cached_input_tokens":%d,"cache_write_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":0,"total_tokens":%d}`, input, cached, write, output, input+output)
	return `{"timestamp":"2026-10-21T00:48:07Z","ordinal":14,"type":"token_usage_record","payload":{"thread_id":"s1","usage":` + u + `,"turn_token_usage":` + u + `,"thread_token_usage":` + u + `}}`
}

func writeRollout(t *testing.T, path string, mod time.Time, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// Cached tokens are inside input_tokens and are charged at the cached rate;
// each response is priced with the model of the turn it belongs to; a model
// with no price is reported and left out.
func TestParseCodexRolloutCost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	writeRollout(t, path, time.Now(),
		codexMeta("/w"),
		`{"type":"response_item","payload":{"type":"message","content":"a very long tool output"}}`,
		codexTurn("gpt-6.1-sol"),
		codexUsageLine(21508, 12288, 0, 82),
		codexUsageLine(21614, 21376, 100, 48),
		codexTurn("gpt-6-luna"),
		codexUsageLine(1000, 0, 0, 1000),
		codexTurn("gpt-reserve"),
		codexUsageLine(5000, 0, 0, 5000),
		codexUsageLine(5000, 0, 0, 5000),
	)
	cost, unpriced, err := parseCodexRolloutCost(path)
	if err != nil {
		t.Fatal(err)
	}
	sol, luna := openAIPricing["gpt-6.1-sol"], openAIPricing["gpt-6-luna"]
	want := (float64(21508-12288)*sol.InputPerMillion + 12288*sol.CacheReadPerMillion + 82*sol.OutputPerMillion +
		float64(21614-21376-100)*sol.InputPerMillion + 21376*sol.CacheReadPerMillion + 100*sol.CacheCreatePerMillion + 48*sol.OutputPerMillion +
		1000*luna.InputPerMillion + 1000*luna.OutputPerMillion) / 1_000_000
	approxEqual(t, cost, want, "rollout cost")
	if strings.Join(unpriced, ",") != "gpt-reserve" {
		t.Errorf("unpriced = %v, want [gpt-reserve] once", unpriced)
	}
}

// The newest rollout started in the work dir wins; rollouts for other dirs,
// non-rollout files and date directories past the search window are ignored.
func TestFindLatestCodexRollout(t *testing.T) {
	sessions := t.TempDir()
	now := time.Date(2026, 10, 21, 1, 0, 0, 0, time.Local)
	day := func(d int, name string) string {
		return filepath.Join(sessions, now.AddDate(0, 0, -d).Format("2006/01/02"), name)
	}
	writeRollout(t, day(1, "rollout-old.jsonl"), now.Add(-30*time.Hour), codexMeta("/w"))
	writeRollout(t, day(0, "rollout-mine.jsonl"), now.Add(-time.Hour), codexMeta("/w"))
	writeRollout(t, day(0, "rollout-other.jsonl"), now.Add(-time.Minute), codexMeta("/elsewhere"))
	writeRollout(t, day(0, "notes.jsonl"), now, codexMeta("/w"))
	writeRollout(t, day(codexRolloutMaxAgeDays, "rollout-stale.jsonl"), now, codexMeta("/stale"))

	path, _, err := findLatestCodexRollout(sessions, "/w", now)
	if err != nil || filepath.Base(path) != "rollout-mine.jsonl" {
		t.Errorf("got %q, %v; want rollout-mine.jsonl", path, err)
	}
	if _, _, err := findLatestCodexRollout(sessions, "/stale", now); err == nil {
		t.Error("found a rollout outside the search window")
	}
}

// gt costs record charges a Codex session from its rollout, and a work dir
// that also has older Claude transcripts is charged from the newer file.
func TestRunCostsRecord_CodexRollout(t *testing.T) {
	projectDir := setupRecordEnv(t)
	codexHome := os.Getenv("CODEX_HOME")
	workDir := os.Getenv("GT_CWD")
	now := time.Now()
	rollout := filepath.Join(codexHome, "sessions", now.Format("2006/01/02"), "rollout-a.jsonl")

	writeTranscriptTurns(t, filepath.Join(projectDir, "claude.jsonl"), 1, now.Add(-2*time.Hour))
	writeRollout(t, rollout, now.Add(-time.Hour), codexMeta(workDir), codexTurn("gpt-6.1-sol"), codexUsageLine(10000, 0, 0, 1000))
	record(t, "first codex record")

	writeRollout(t, rollout, now, codexMeta(workDir), codexTurn("gpt-6.1-sol"), codexUsageLine(10000, 0, 0, 1000), codexUsageLine(20000, 10000, 0, 500))
	record(t, "second codex record")

	entries := readCostLog(t)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	sol := openAIPricing["gpt-6.1-sol"]
	first := (10000*sol.InputPerMillion + 1000*sol.OutputPerMillion) / 1_000_000
	second := (10000*sol.InputPerMillion + 10000*sol.CacheReadPerMillion + 500*sol.OutputPerMillion) / 1_000_000
	if entries[0].Transcript != "rollout-a.jsonl" || entries[1].Transcript != "rollout-a.jsonl" {
		t.Errorf("transcripts = %q, %q; want the rollout", entries[0].Transcript, entries[1].Transcript)
	}
	approxEqual(t, entries[0].CostUSD, first, "first record")
	approxEqual(t, entries[1].CostUSD, second, "second record (increment)")
	approxEqual(t, entryCumulative(&entries[1]), first+second, "session total")

	// A newer Claude transcript in the same dir takes over again.
	writeTranscriptTurns(t, filepath.Join(projectDir, "claude-new.jsonl"), 1, now.Add(time.Minute))
	record(t, "claude record")
	if got := readCostLog(t)[2].Transcript; got != "claude-new.jsonl" {
		t.Errorf("transcript = %q, want the newer Claude transcript", got)
	}
}
